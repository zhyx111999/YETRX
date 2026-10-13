/**
 * Workflow run state persistence for pause/resume support.
 */

import { createHash, randomUUID } from "node:crypto";
import { join } from "node:path";
import type { AgentUsage } from "./agent-usage.js";
import type { WorkflowAgentSnapshot } from "./display.js";
import { RunRecordDeletedError, RunRecordUnreadableError, WorkflowErrorCode } from "./errors.js";
import {
  isDeadLockDebris,
  type LockOwner,
  newLockToken,
  pidIsAlive,
  releaseFileLock,
  tryClaimFileLock,
} from "./file-lock.js";
import {
  ensureDir as ensureDirFs,
  listFilesSafe,
  type PersistenceFsLayer,
  resolvePersistenceFs,
  unlinkIfExistsSafe,
} from "./fs-persistence.js";
import { reportDiagnostic } from "./host-diagnostics.js";
import type { AgentTimestamps } from "./manager/agent-rows.js";
import { settleInterruptedPersistedAgents } from "./run-agent-settlement.js";
import { createRunRecordStore } from "./run-record-store.js";
import type { WorkflowCheckpoint } from "./workflow.js";
import { workflowProjectPaths } from "./workflow-paths.js";

export type RunStatus = "pending" | "running" | "paused" | "completed" | "failed" | "aborted";

/**
 * Why a paused run is paused: a provider quota ran out (`usage_limit`, which
 * the usage-limit scheduler resumes), it waits at a checkpoint
 * (`workflow_checkpoint`), or the process running it ended (`interrupted`).
 * A plain pause() records none.
 */
export type PauseReason = "usage_limit" | "workflow_checkpoint" | "interrupted";

/**
 * An agent row as the record keeps it: the display row, plus when its latest
 * execution ran, which the live snapshot keeps beside its rows.
 */
export interface PersistedAgentState extends WorkflowAgentSnapshot, Partial<AgentTimestamps> {}

/** Serialized journal entry, keyed by the call's index (see JournalEntry). */
export interface PersistedJournalEntry {
  index: number;
  hash: string;
  result: unknown;
  storeDelta?: Record<string, unknown>;
  /** The model the call ran on; absent on checkpoints, which run no model. */
  model?: string;
}

/**
 * Sanitize a persisted/incoming auto-resume attempt counter: corrupt or
 * foreign values (non-number, NaN, Infinity, negative, non-integer) become
 * undefined — a NaN/negative counter would defeat the scheduler's give-up
 * cap and produce NaN timer delays.
 */
export function sanitizeAutoResumeAttempts(value: unknown): number | undefined {
  return typeof value === "number" && Number.isInteger(value) && value >= 0 ? value : undefined;
}

export interface PersistedRunState {
  runId: string;
  workflowName: string;
  script: string;
  args?: unknown;
  /** The pi session that receives the run's result and lists it. Set when the
   * record is created and changed only by RunPersistence.moveSession(); runs
   * persist on disk across sessions but the navigator shows only the current
   * session's runs (undefined = shown in every session). */
  sessionId?: string;
  /** Immutable parent session identity for this workflow run. */
  parentSessionId?: string;
  /** Immutable parent session file for this workflow run, when persisted. */
  parentSessionFile?: string;
  status: RunStatus;
  /**
   * Terminal failure/abort message. Written for `failed` and `aborted` runs;
   * absent on running/paused/completed and on records persisted before this field.
   */
  error?: string;
  /**
   * Classified terminal cause. Written with `error` for `failed` and `aborted`
   * runs; absent on running/paused/completed runs.
   */
  errorCode?: WorkflowErrorCode;
  /** Why a paused run is paused. */
  pauseReason?: PauseReason;
  /** Provider reset hint for a usage-limit pause, e.g. "Resets in ~3h" (verbatim). */
  resetHint?: string;
  /** Durable workflow-controlled suspension and its at-most-once response. */
  checkpoint?: WorkflowCheckpoint;
  phases: string[];
  currentPhase?: string;
  agents: PersistedAgentState[];
  logs: string[];
  result?: unknown;
  startedAt: string;
  updatedAt: string;
  completedAt?: string;
  durationMs?: number;
  tokenUsage?: AgentUsage;
  /**
   * Cached agent/checkpoint results for resume, keyed by deterministic call
   * index. `storeDelta` is this call's SharedStore write delta, replayed
   * additively on resume.
   */
  journal?: PersistedJournalEntry[];
  /**
   * Named toolset tag (WorkflowManagerOptions.toolsets). ToolDefinitions are
   * functions and can't be serialized, so this tag is how a resumed run (e.g.
   * /deep-research with web tools) re-resolves the tool set it started with.
   */
  toolset?: string;
  /**
   * Auto-resume attempt counter for the current usage_limit pause-cycle.
   * Owned by WorkflowManager (written on every persistRun; the scheduler
   * records through recordAutoResumeAttempts, never a raw save).
   * Absent/0 means no auto-resume attempt has been recorded yet.
   */
  autoResumeAttempts?: number;
  /**
   * Undelivered background-result payload waiting for the originating session's
   * delivery endpoint. Written before the send attempt (fail-closed); cleared
   * only after a successful session-routed delivery. `complete` recomputes text
   * from the persisted result on flush so we don't retain a second full copy.
   */
  pendingDelivery?: PendingDeliveryMarker;
}

/**
 * Disk/memory marker for a background result that still needs conversation
 * delivery. Kept small on purpose — never store full agent transcripts here.
 */
export type PendingDeliveryMarker =
  | { kind: "complete"; deliveryId?: string }
  | { kind: "text"; text: string; deliveryId?: string };

export interface RunPersistence {
  /** Immutable, directly readable result artifact for conversation delivery. */
  exportResult(runId: string, result: unknown): string;
  /** Read routing metadata without hydrating history; detail fields are lazy. */
  loadPreview(runId: string): PersistedRunState | null;
  /** Under the caller's run lease, settle orphaned agents using a log delta. */
  recoverInterrupted(runId: string): boolean;
  /**
   * Save current run state. Throws RunRecordDeletedError instead of recreating a
   * run this instance has written or read after another writer deleted it.
   * An existing record keeps its session owner, which is written back into
   * `state`: a full save must not undo another writer's moveSession().
   */
  save(state: PersistedRunState): void;
  /** Merge small delivery fields without hydrating the run history. */
  updateMetadata(
    runId: string,
    patch: Partial<Pick<PersistedRunState, "pendingDelivery" | "autoResumeAttempts">>,
    expectedDeliveryId?: string,
  ): boolean;
  /**
   * The only change of an existing record's session owner. Under the writer
   * lock, moves it to `sessionId` when `from` accepts the current owner, so
   * concurrent moves and saves cannot lose one another. Returns the owner the
   * record has afterwards, or null when the record does not exist.
   */
  moveSession(
    runId: string,
    sessionId: string,
    from: (owner: string | undefined) => boolean,
  ): { moved: boolean; sessionId?: string } | null;
  /** Load a persisted run by ID. */
  load(runId: string): PersistedRunState | null;
  /**
   * List every run's lightweight view, read through the same head recovery as
   * load(), so runs only their backup holds are included.
   */
  list(): PersistedRunState[];
  /**
   * Delete a persisted run with its sidecars, exported results, and run log.
   * Returns whether any of its data was removed. Throws when a file could not
   * be removed; the run is then either intact or already unlistable, and
   * deleting again finishes the cleanup.
   */
  delete(runId: string): boolean;
  /**
   * Acquire an exclusive cross-process lease for a run. Returns null when another
   * live process owns the run; stale/corrupt lock files are removed and retried.
   */
  acquireRunLease(runId: string): RunLease | null;
  /** Release a lease previously returned by acquireRunLease(). */
  releaseRunLease(lease: RunLease): void;
  /** Get runs directory path. */
  getRunsDir(): string;
}

export interface RunLease {
  runId: string;
  token: string;
}

/** An execution lease: the lock owner record plus the run it covers. */
type LockFile = LockOwner & {
  runId: string;
  runPath: string;
  startedAt: string;
};

/**
 * Filesystem operations used by run persistence.
 * Exposed for testing – pass overrides to inject mock implementations.
 * (Alias of the shared PersistenceFsLayer — see fs-persistence.ts.)
 */
export type FsLayer = PersistenceFsLayer;

/**
 * Retention policy for terminal (completed/failed/aborted) runs kept on
 * disk. Bounded so a long-lived project directory can't accumulate an
 * unbounded number of run files (each polled/listed on every list() call).
 * A run in "running" or "paused" status is NEVER counted against this cap
 * or evicted by it — only genuinely finished runs age out, oldest (by
 * updatedAt) first, once the terminal-run count exceeds the cap. 300 is
 * generous enough to cover weeks of typical usage while keeping list()'s
 * per-call directory scan bounded.
 */
const DEFAULT_MAX_TERMINAL_RUNS_ON_DISK = 300;

export const TERMINAL_RUN_STATUSES: ReadonlySet<RunStatus> = new Set(["completed", "failed", "aborted"]);

const PERSISTED_AGENT_STATUSES = [
  "queued",
  "running",
  "done",
  "error",
  "skipped",
] as const satisfies readonly PersistedAgentState["status"][];

// Exhaustiveness: adding a member to PersistedAgentState["status"] without
// listing it above fails to compile HERE (Exclude yields a non-never).
type AssertNever<T extends never> = T;
type _PersistedAgentStatusExhaustiveCheck = AssertNever<
  Exclude<PersistedAgentState["status"], (typeof PERSISTED_AGENT_STATUSES)[number]>
>;

/** Every status a persisted agent row may validly carry — exhaustively
 * checked against PersistedAgentState["status"] by the assertion above.
 * Forward-compat note: resume seeding DROPS rows with out-of-union statuses
 * (e.g. written by a newer release) — deliberate garbage-vs-unknown tradeoff:
 * an unknown status cannot be ghost-settled or displayed safely, so the row
 * is treated as corrupt rather than re-persisted as a lie. */
export const VALID_PERSISTED_AGENT_STATUSES: ReadonlySet<PersistedAgentState["status"]> = new Set(
  PERSISTED_AGENT_STATUSES,
);

/** Cause stamped onto leftover in-flight agents when a run reaches a terminal status. */
export function terminalRunInterruptCause(
  status: RunStatus,
  error?: { message?: string; code?: WorkflowErrorCode },
): { error: string; errorCode?: WorkflowErrorCode } {
  if (status === "aborted") {
    return { error: "aborted", errorCode: error?.code ?? WorkflowErrorCode.WORKFLOW_ABORTED };
  }
  if (status === "failed") {
    return {
      error: error?.message ?? "run failed",
      errorCode: error?.code ?? WorkflowErrorCode.UNKNOWN,
    };
  }
  return { error: "run completed" };
}

/**
 * Fail-closed rewrite of leftover queued/running agents on a terminal run.
 * Completed/failed/aborted must never persist a still-`running` agent.
 */
export function settleNonTerminalPersistedAgents(
  agents: PersistedAgentState[],
  status: RunStatus,
  error: { message?: string; code?: WorkflowErrorCode } | undefined,
  endedAt: string,
): PersistedAgentState[] {
  if (!TERMINAL_RUN_STATUSES.has(status)) return agents;
  return settleInterruptedPersistedAgents(agents, terminalRunInterruptCause(status, error), endedAt);
}

export interface RunPersistenceOptions {
  /** Override DEFAULT_MAX_TERMINAL_RUNS_ON_DISK (tests; advanced tuning). */
  maxTerminalRunsOnDisk?: number;
}

/**
 * Absorb same-tick list reads before checking directory stamps and reconciling
 * lightweight per-file views. Full histories are hydrated only on demand.
 */
const LIST_CACHE_TTL_MS = 300;

/** A record is its head or its backup: a backup alone still recovers the run. */
const RECORD_FILE = /^(.+)\.json(\.bak)?$/;

interface RecordFiles {
  head: boolean;
  backup: boolean;
}

export function createRunPersistence(
  cwd: string,
  fsOverride?: Partial<FsLayer>,
  options?: RunPersistenceOptions,
): RunPersistence {
  const fs = resolvePersistenceFs(fsOverride);
  const records = createRunRecordStore(fs);
  const _existsSync = fs.existsSync;
  const _readFileSync = fs.readFileSync;
  const _statSync = fs.statSync;
  const _unlinkSync = fs.unlinkSync;
  const _writeFileSync = fs.writeFileSync;
  const maxTerminalRunsOnDisk = options?.maxTerminalRunsOnDisk ?? DEFAULT_MAX_TERMINAL_RUNS_ON_DISK;

  const { runsDir } = workflowProjectPaths(cwd);

  const ensureDir = () => ensureDirFs(fs, runsDir);

  const runPath = (runId: string) => join(runsDir, `${runId}.json`);
  const lockPath = (runId: string) => join(runsDir, `${runId}.lock`);
  // A record is its head file or the backup that recovers it.
  const recordExists = (runId: string) => {
    const path = runPath(runId);
    return _existsSync(path) || _existsSync(`${path}.bak`);
  };
  // Runs this instance has written or read. Every write path re-saves a state it
  // obtained that way, so a known run whose record is now absent was deleted by
  // another writer; recreating it would resurrect the deleted run. A run this
  // instance has never seen is still created normally.
  const knownRunIds = new Set<string>();

  const ownerIsGone = (owner: LockOwner) => !pidIsAlive(owner.pid);

  // Short writer mutex serializes append + head commit, including metadata
  // writes while a long-lived execution lease is held. Never busy-wait: a
  // write that finds another writer fails and the caller decides.
  const mutate = <T>(runId: string, fn: () => T): T => {
    ensureDir();
    const path = `${runPath(runId)}.write-lock`;
    const token = newLockToken();
    if (!tryClaimFileLock(fs, path, { pid: process.pid, token }, ownerIsGone)) {
      throw Object.assign(new Error(`Run ${runId} is being written by another process`), { code: "EBUSY" });
    }
    try {
      return fn();
    } finally {
      releaseFileLock(fs, path, token);
      invalidateListCache();
    }
  };

  /**
   * The run's execution lease. A lease whose owner process is gone, or that was
   * recorded for another runs directory, may be taken over.
   */
  const acquireLease = (runId: string): RunLease | null => {
    ensureDir();
    const path = runPath(runId);
    const owner: LockFile = {
      runId,
      runPath: path,
      pid: process.pid,
      startedAt: new Date().toISOString(),
      token: newLockToken(),
    };
    const stale = (existing: LockOwner) => existing.runPath !== path || !pidIsAlive(existing.pid);
    return tryClaimFileLock(fs, lockPath(runId), owner, stale) ? { runId, token: owner.token } : null;
  };

  const releaseLease = (lease: RunLease): void => {
    try {
      releaseFileLock(fs, lockPath(lease.runId), lease.token);
    } catch {
      // Best-effort cleanup only.
    }
  };

  // Merge a small write into the run's existing record; never creates one.
  const updateExistingRecord = (runId: string, write: (path: string) => boolean): boolean =>
    mutate(runId, () => {
      if (!recordExists(runId)) return false;
      knownRunIds.add(runId);
      return write(runPath(runId));
    });

  // list() cache: recomputed lazily, invalidated synchronously by every
  // mutation this instance performs (save()/delete()) so a stale read can
  // never outlive a mutation this process made. A read from another process
  // (or a direct fs write bypassing this instance) is picked up once the TTL
  // elapses, same as before this cache existed on the next un-cached call.
  let listCache: PersistedRunState[] | undefined;
  let listCacheAt = 0;
  let directoryStamp = "";
  let reconciledAt = 0;
  const directoryVersion = () => {
    try {
      const s = fs.statSync(runsDir);
      return `${s.ino}:${s.mtimeMs}:${s.ctimeMs}`;
    } catch {
      return "missing";
    }
  };
  const invalidateListCache = () => {
    listCache = undefined;
  };

  // Per-run view cache, keyed by runId. Re-stat is cheap; re-reading and
  // parsing every head is not, and would scale with the lifetime run history
  // rather than with what changed since the last pass. A view is reused while
  // the file it is read from keeps its inode, size and mtime. The inode is
  // load-bearing: heads are replaced by tmp-write + rename, which allocates a
  // new inode, so two saves landing in the same coarse mtime tick with equal
  // byte length ("paused" and "failed" are the same length) still differ.
  // Entries for runs that left the directory are pruned, so the map tracks
  // what is on disk rather than lifetime history.
  const listViews = new Map<string, { stamp: string; view: PersistedRunState }>();

  // Only the file a reader starts from is stat'ed. While the head is present
  // it is that file; writers refresh the backup only together with the head,
  // so the head's identity also covers a backup it may fall back to. The
  // backup's presence is part of the stamp, so a backup appearing or vanishing
  // next to an unreadable head is still noticed.
  const viewStamp = (runId: string, files: RecordFiles): string => {
    const path = runPath(runId);
    const s = _statSync(files.head ? path : `${path}.bak`);
    return `${files.head}:${files.backup}:${s.ino}:${s.size}:${s.mtimeMs}`;
  };

  // The run's lightweight view, or undefined when it has no readable record.
  const listView = (runId: string, files: RecordFiles): PersistedRunState | undefined => {
    const stamp = viewStamp(runId, files);
    const cached = listViews.get(runId);
    if (cached?.stamp === stamp) return cached.view;
    listViews.delete(runId);
    // The same recovery entry as load(): a missing or unparsable head falls
    // back to its backup. A head naming another run is not this run's record,
    // and load() could not find it under that name.
    const view = records.peek(runPath(runId));
    if (view?.runId !== runId) return undefined;
    knownRunIds.add(runId);
    listViews.set(runId, { stamp, view });
    return view;
  };

  // list() runs on every navigator refresh; one report per record and damage is enough.
  const reportedDamage = new Set<string>();
  const reportDamagedOnce = (runId: string, error: RunRecordUnreadableError) => {
    const key = `${runId}\n${error.message}`;
    if (reportedDamage.has(key)) return;
    reportedDamage.add(key);
    reportDiagnostic(`Skipped workflow run ${runId}: ${error.message}. Remove it with /workflows rm ${runId}`);
  };

  const computeList = (): PersistedRunState[] => {
    const candidates = new Map<string, RecordFiles>();
    for (const name of listFilesSafe(fs, runsDir)) {
      const match = RECORD_FILE.exec(name);
      if (!match) continue;
      const files = candidates.get(match[1]) ?? { head: false, backup: false };
      if (match[2]) files.backup = true;
      else files.head = true;
      candidates.set(match[1], files);
    }
    for (const runId of listViews.keys()) if (!candidates.has(runId)) listViews.delete(runId);
    const views: PersistedRunState[] = [];
    for (const [runId, files] of candidates) {
      try {
        const view = listView(runId, files);
        if (view) views.push(view);
      } catch (error) {
        // Skip damaged or vanished records without caching the failure: a
        // transient read error must not hide a valid record until it changes.
        listViews.delete(runId);
        if (error instanceof RunRecordUnreadableError) reportDamagedOnce(runId, error);
      }
    }
    return views.sort((a, b) => new Date(b.updatedAt).getTime() - new Date(a.updatedAt).getTime());
  };

  // Bound the number of terminal (completed/failed/aborted) runs kept on
  // disk (see DEFAULT_MAX_TERMINAL_RUNS_ON_DISK) — called after every save()
  // whose state is terminal, since that's the only time the terminal count
  // can grow. Running/paused and undelivered runs are never candidates: they're
  // filtered out before the cap is even considered.
  const enforceRetention = () => {
    const terminal = computeList()
      .filter((r) => TERMINAL_RUN_STATUSES.has(r.status) && !r.pendingDelivery)
      .sort((a, b) => new Date(a.updatedAt).getTime() - new Date(b.updatedAt).getTime());
    const excess = terminal.length - maxTerminalRunsOnDisk;
    if (excess <= 0) return;
    for (const run of terminal.slice(0, excess)) {
      // Hold the run's lease through the deletion, in the same order as resume
      // (lease, then writer mutex), so no process can take the run over
      // between the check and the delete.
      const lease = acquireLease(run.runId);
      if (!lease) continue;
      try {
        mutate(run.runId, () => {
          const fresh = records.peek(runPath(run.runId));
          if (fresh && TERMINAL_RUN_STATUSES.has(fresh.status) && !fresh.pendingDelivery) deleteRunFiles(run.runId);
        });
      } catch {
        // Contended or unreadable records remain available for the next pass.
      } finally {
        releaseLease(lease);
      }
    }
    invalidateListCache();
  };

  /**
   * Delete a run's files; the caller holds its writer mutex. The record stays
   * whole until its last recovery candidate is gone: the backup goes first, so
   * a stale backup never stands in for a removed head, and removing the head
   * is the commit point. Only after it do the files the head depends on go, so
   * a failure at any step leaves either a complete, loadable record or no
   * record at all, never a listed head whose log is gone. Every step tolerates
   * an already-missing file, so a retry finishes what a failed deletion left.
   * Returns whether any of the run's data was removed.
   */
  const deleteRunFiles = (runId: string): boolean => {
    const path = runPath(runId);
    const isLockDebrisName = (name: string) =>
      [`${runId}.json.write-lock.`, `${runId}.lock.`].some((prefix) => name.startsWith(prefix)) &&
      (name.endsWith(".owner") || name.includes(".reap-"));
    listViews.delete(runId);
    records.forget(path);
    // A missing file is already deleted, which keeps every step idempotent.
    const unlink = (file: string): boolean => {
      try {
        _unlinkSync(file);
        return true;
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code === "ENOENT") return false;
        throw error;
      }
    };
    let removed = false;
    if (unlink(`${path}.bak`)) removed = true;
    if (unlink(path)) removed = true;
    // Committed. Attempt every leftover before reporting the first failure.
    let failure: { error: unknown } | undefined;
    const cleanUp = (file: string, isRunData: boolean) => {
      try {
        if (unlink(file) && isRunData) removed = true;
      } catch (error) {
        failure ??= { error };
      }
    };
    // The event log and any staged generation logs a compaction left behind.
    for (const log of records.logFiles(path)) cleanUp(log, true);
    try {
      for (const name of _existsSync(runsDir) ? fs.readdirSync(runsDir) : []) {
        // The run log, exported results, and temporary files of interrupted writes.
        const runData =
          name === `${runId}.log` ||
          name.startsWith(`${runId}.json.result-`) ||
          (name.startsWith(`${runId}.json.`) && name.endsWith(".tmp"));
        if (runData) cleanUp(join(runsDir, name), true);
        // Lock debris (claim candidates, reap guards) of processes that are gone
        // is removed but is not run data; a live process's claim stays untouched.
        else if (isLockDebrisName(name) && isDeadLockDebris(fs, join(runsDir, name)))
          cleanUp(join(runsDir, name), false);
      }
    } catch (error) {
      failure ??= { error };
    }
    if (failure) throw failure.error;
    // The lock goes LAST. deleteRun() holds its lease across this sequence;
    // releasing it sooner would let another process resume a record that is
    // about to be deleted. After a failure the lease holder releases it.
    unlinkIfExistsSafe(fs, lockPath(runId));
    return removed;
  };

  return {
    exportResult(runId, result) {
      return mutate(runId, () => {
        if (!_existsSync(runPath(runId))) throw new Error("Run disappeared before result export");
        const json = JSON.stringify({ runId, result }, null, 2);
        const hash = createHash("sha256").update(json).digest("hex");
        const path = `${runPath(runId)}.result-${hash}`;
        if (_existsSync(path)) return path;
        const temporary = `${path}.${randomUUID()}.tmp`;
        try {
          _writeFileSync(temporary, json, { flush: true });
          fs.renameSync(temporary, path);
        } finally {
          unlinkIfExistsSafe(fs, temporary);
        }
        return path;
      });
    },
    save(state: PersistedRunState) {
      ensureDir();
      state.updatedAt = new Date().toISOString();
      const path = runPath(state.runId);
      // Atomic write: a crash mid-write can't corrupt the live file (tmp+rename is
      // atomic on the same filesystem). A .bak from the previous good save is the
      // recovery fallback if the primary is somehow truncated.
      mutate(state.runId, () => {
        if (knownRunIds.has(state.runId) && !recordExists(state.runId)) throw new RunRecordDeletedError(state.runId);
        const current = records.peek(path);
        if (current) state.sessionId = current.sessionId;
        records.save(path, state);
        knownRunIds.add(state.runId);
      });
      invalidateListCache();
      // Only a terminal write can grow the terminal-run count, so only check
      // the cap then — a "running"/"paused" save is on the hot path (every
      // progress tick) and must not pay for a retention scan.
      if (TERMINAL_RUN_STATUSES.has(state.status)) enforceRetention();
    },

    loadPreview(runId) {
      const record = records.peek(runPath(runId));
      if (record) knownRunIds.add(runId);
      return record;
    },

    recoverInterrupted(runId) {
      return updateExistingRecord(runId, (path) => records.recoverInterrupted(path));
    },

    updateMetadata(runId, patch, expectedDeliveryId) {
      return updateExistingRecord(runId, (path) => records.updateMetadata(path, patch, expectedDeliveryId));
    },

    moveSession(runId, sessionId, from) {
      return mutate(runId, () => {
        const path = runPath(runId);
        const current = records.peek(path);
        if (!current) return null;
        knownRunIds.add(runId);
        if (current.sessionId === sessionId || !from(current.sessionId)) {
          return { moved: false, sessionId: current.sessionId };
        }
        records.updateMetadata(path, { sessionId });
        return { moved: true, sessionId };
      });
    },

    load(runId: string): PersistedRunState | null {
      // Null means no record. A damaged one throws RunRecordUnreadableError:
      // reading it as missing would let a caller treat the run as deleted.
      const state = records.read(runPath(runId));
      if (state) knownRunIds.add(runId);
      return state;
    },

    list(): PersistedRunState[] {
      const now = Date.now();
      // Return a fresh array on every call (a cheap ref-copy) so a caller that
      // sorts/reverses/mutates the result in place can't corrupt the cache — the
      // pre-cache code re-parsed into a new array each call, preserve that.
      if (listCache && now - listCacheAt < LIST_CACHE_TTL_MS) {
        return [...listCache];
      }
      // Cooperative writers replace heads atomically, changing the directory
      // stamp. Reconcile in-place external edits at most every five seconds.
      const stamp = directoryVersion();
      if (listCache && stamp === directoryStamp && now - reconciledAt < 5000) {
        listCacheAt = now;
        return [...listCache];
      }
      const result = computeList();
      listCache = result;
      listCacheAt = now;
      reconciledAt = now;
      directoryStamp = stamp;
      return [...result];
    },

    delete(runId: string): boolean {
      try {
        return mutate(runId, () => deleteRunFiles(runId));
      } finally {
        invalidateListCache();
      }
    },

    acquireRunLease: acquireLease,

    releaseRunLease: releaseLease,

    getRunsDir(): string {
      return runsDir;
    },
  };
}

/**
 * Generate a unique run ID.
 */
export function generateRunId(): string {
  const timestamp = Date.now().toString(36);
  const random = Math.random().toString(36).slice(2, 8);
  return `${timestamp}-${random}`;
}
