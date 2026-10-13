/**
 * Workflow run state persistence for pause/resume support.
 */
import { createHash, randomUUID } from "node:crypto";
import { join } from "node:path";
import { RunRecordDeletedError, RunRecordUnreadableError, WorkflowErrorCode } from "./errors.js";
import { isDeadLockDebris, newLockToken, pidIsAlive, releaseFileLock, tryClaimFileLock, } from "./file-lock.js";
import { ensureDir as ensureDirFs, listFilesSafe, resolvePersistenceFs, unlinkIfExistsSafe, } from "./fs-persistence.js";
import { reportDiagnostic } from "./host-diagnostics.js";
import { settleInterruptedPersistedAgents } from "./run-agent-settlement.js";
import { createRunRecordStore } from "./run-record-store.js";
import { workflowProjectPaths } from "./workflow-paths.js";
/**
 * Sanitize a persisted/incoming auto-resume attempt counter: corrupt or
 * foreign values (non-number, NaN, Infinity, negative, non-integer) become
 * undefined — a NaN/negative counter would defeat the scheduler's give-up
 * cap and produce NaN timer delays.
 */
export function sanitizeAutoResumeAttempts(value) {
    return typeof value === "number" && Number.isInteger(value) && value >= 0 ? value : undefined;
}
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
export const TERMINAL_RUN_STATUSES = new Set(["completed", "failed", "aborted"]);
const PERSISTED_AGENT_STATUSES = [
    "queued",
    "running",
    "done",
    "error",
    "skipped",
];
/** Every status a persisted agent row may validly carry — exhaustively
 * checked against PersistedAgentState["status"] by the assertion above.
 * Forward-compat note: resume seeding DROPS rows with out-of-union statuses
 * (e.g. written by a newer release) — deliberate garbage-vs-unknown tradeoff:
 * an unknown status cannot be ghost-settled or displayed safely, so the row
 * is treated as corrupt rather than re-persisted as a lie. */
export const VALID_PERSISTED_AGENT_STATUSES = new Set(PERSISTED_AGENT_STATUSES);
/** Cause stamped onto leftover in-flight agents when a run reaches a terminal status. */
export function terminalRunInterruptCause(status, error) {
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
export function settleNonTerminalPersistedAgents(agents, status, error, endedAt) {
    if (!TERMINAL_RUN_STATUSES.has(status))
        return agents;
    return settleInterruptedPersistedAgents(agents, terminalRunInterruptCause(status, error), endedAt);
}
/**
 * Absorb same-tick list reads before checking directory stamps and reconciling
 * lightweight per-file views. Full histories are hydrated only on demand.
 */
const LIST_CACHE_TTL_MS = 300;
/** A record is its head or its backup: a backup alone still recovers the run. */
const RECORD_FILE = /^(.+)\.json(\.bak)?$/;
export function createRunPersistence(cwd, fsOverride, options) {
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
    const runPath = (runId) => join(runsDir, `${runId}.json`);
    const lockPath = (runId) => join(runsDir, `${runId}.lock`);
    // A record is its head file or the backup that recovers it.
    const recordExists = (runId) => {
        const path = runPath(runId);
        return _existsSync(path) || _existsSync(`${path}.bak`);
    };
    // Runs this instance has written or read. Every write path re-saves a state it
    // obtained that way, so a known run whose record is now absent was deleted by
    // another writer; recreating it would resurrect the deleted run. A run this
    // instance has never seen is still created normally.
    const knownRunIds = new Set();
    const ownerIsGone = (owner) => !pidIsAlive(owner.pid);
    // Short writer mutex serializes append + head commit, including metadata
    // writes while a long-lived execution lease is held. Never busy-wait: a
    // write that finds another writer fails and the caller decides.
    const mutate = (runId, fn) => {
        ensureDir();
        const path = `${runPath(runId)}.write-lock`;
        const token = newLockToken();
        if (!tryClaimFileLock(fs, path, { pid: process.pid, token }, ownerIsGone)) {
            throw Object.assign(new Error(`Run ${runId} is being written by another process`), { code: "EBUSY" });
        }
        try {
            return fn();
        }
        finally {
            releaseFileLock(fs, path, token);
            invalidateListCache();
        }
    };
    /**
     * The run's execution lease. A lease whose owner process is gone, or that was
     * recorded for another runs directory, may be taken over.
     */
    const acquireLease = (runId) => {
        ensureDir();
        const path = runPath(runId);
        const owner = {
            runId,
            runPath: path,
            pid: process.pid,
            startedAt: new Date().toISOString(),
            token: newLockToken(),
        };
        const stale = (existing) => existing.runPath !== path || !pidIsAlive(existing.pid);
        return tryClaimFileLock(fs, lockPath(runId), owner, stale) ? { runId, token: owner.token } : null;
    };
    const releaseLease = (lease) => {
        try {
            releaseFileLock(fs, lockPath(lease.runId), lease.token);
        }
        catch {
            // Best-effort cleanup only.
        }
    };
    // Merge a small write into the run's existing record; never creates one.
    const updateExistingRecord = (runId, write) => mutate(runId, () => {
        if (!recordExists(runId))
            return false;
        knownRunIds.add(runId);
        return write(runPath(runId));
    });
    // list() cache: recomputed lazily, invalidated synchronously by every
    // mutation this instance performs (save()/delete()) so a stale read can
    // never outlive a mutation this process made. A read from another process
    // (or a direct fs write bypassing this instance) is picked up once the TTL
    // elapses, same as before this cache existed on the next un-cached call.
    let listCache;
    let listCacheAt = 0;
    let directoryStamp = "";
    let reconciledAt = 0;
    const directoryVersion = () => {
        try {
            const s = fs.statSync(runsDir);
            return `${s.ino}:${s.mtimeMs}:${s.ctimeMs}`;
        }
        catch {
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
    const listViews = new Map();
    // Only the file a reader starts from is stat'ed. While the head is present
    // it is that file; writers refresh the backup only together with the head,
    // so the head's identity also covers a backup it may fall back to. The
    // backup's presence is part of the stamp, so a backup appearing or vanishing
    // next to an unreadable head is still noticed.
    const viewStamp = (runId, files) => {
        const path = runPath(runId);
        const s = _statSync(files.head ? path : `${path}.bak`);
        return `${files.head}:${files.backup}:${s.ino}:${s.size}:${s.mtimeMs}`;
    };
    // The run's lightweight view, or undefined when it has no readable record.
    const listView = (runId, files) => {
        const stamp = viewStamp(runId, files);
        const cached = listViews.get(runId);
        if (cached?.stamp === stamp)
            return cached.view;
        listViews.delete(runId);
        // The same recovery entry as load(): a missing or unparsable head falls
        // back to its backup. A head naming another run is not this run's record,
        // and load() could not find it under that name.
        const view = records.peek(runPath(runId));
        if (view?.runId !== runId)
            return undefined;
        knownRunIds.add(runId);
        listViews.set(runId, { stamp, view });
        return view;
    };
    // list() runs on every navigator refresh; one report per record and damage is enough.
    const reportedDamage = new Set();
    const reportDamagedOnce = (runId, error) => {
        const key = `${runId}\n${error.message}`;
        if (reportedDamage.has(key))
            return;
        reportedDamage.add(key);
        reportDiagnostic(`Skipped workflow run ${runId}: ${error.message}. Remove it with /workflows rm ${runId}`);
    };
    const computeList = () => {
        const candidates = new Map();
        for (const name of listFilesSafe(fs, runsDir)) {
            const match = RECORD_FILE.exec(name);
            if (!match)
                continue;
            const files = candidates.get(match[1]) ?? { head: false, backup: false };
            if (match[2])
                files.backup = true;
            else
                files.head = true;
            candidates.set(match[1], files);
        }
        for (const runId of listViews.keys())
            if (!candidates.has(runId))
                listViews.delete(runId);
        const views = [];
        for (const [runId, files] of candidates) {
            try {
                const view = listView(runId, files);
                if (view)
                    views.push(view);
            }
            catch (error) {
                // Skip damaged or vanished records without caching the failure: a
                // transient read error must not hide a valid record until it changes.
                listViews.delete(runId);
                if (error instanceof RunRecordUnreadableError)
                    reportDamagedOnce(runId, error);
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
        if (excess <= 0)
            return;
        for (const run of terminal.slice(0, excess)) {
            // Hold the run's lease through the deletion, in the same order as resume
            // (lease, then writer mutex), so no process can take the run over
            // between the check and the delete.
            const lease = acquireLease(run.runId);
            if (!lease)
                continue;
            try {
                mutate(run.runId, () => {
                    const fresh = records.peek(runPath(run.runId));
                    if (fresh && TERMINAL_RUN_STATUSES.has(fresh.status) && !fresh.pendingDelivery)
                        deleteRunFiles(run.runId);
                });
            }
            catch {
                // Contended or unreadable records remain available for the next pass.
            }
            finally {
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
    const deleteRunFiles = (runId) => {
        const path = runPath(runId);
        const isLockDebrisName = (name) => [`${runId}.json.write-lock.`, `${runId}.lock.`].some((prefix) => name.startsWith(prefix)) &&
            (name.endsWith(".owner") || name.includes(".reap-"));
        listViews.delete(runId);
        records.forget(path);
        // A missing file is already deleted, which keeps every step idempotent.
        const unlink = (file) => {
            try {
                _unlinkSync(file);
                return true;
            }
            catch (error) {
                if (error.code === "ENOENT")
                    return false;
                throw error;
            }
        };
        let removed = false;
        if (unlink(`${path}.bak`))
            removed = true;
        if (unlink(path))
            removed = true;
        // Committed. Attempt every leftover before reporting the first failure.
        let failure;
        const cleanUp = (file, isRunData) => {
            try {
                if (unlink(file) && isRunData)
                    removed = true;
            }
            catch (error) {
                failure ??= { error };
            }
        };
        // The event log and any staged generation logs a compaction left behind.
        for (const log of records.logFiles(path))
            cleanUp(log, true);
        try {
            for (const name of _existsSync(runsDir) ? fs.readdirSync(runsDir) : []) {
                // The run log, exported results, and temporary files of interrupted writes.
                const runData = name === `${runId}.log` ||
                    name.startsWith(`${runId}.json.result-`) ||
                    (name.startsWith(`${runId}.json.`) && name.endsWith(".tmp"));
                if (runData)
                    cleanUp(join(runsDir, name), true);
                // Lock debris (claim candidates, reap guards) of processes that are gone
                // is removed but is not run data; a live process's claim stays untouched.
                else if (isLockDebrisName(name) && isDeadLockDebris(fs, join(runsDir, name)))
                    cleanUp(join(runsDir, name), false);
            }
        }
        catch (error) {
            failure ??= { error };
        }
        if (failure)
            throw failure.error;
        // The lock goes LAST. deleteRun() holds its lease across this sequence;
        // releasing it sooner would let another process resume a record that is
        // about to be deleted. After a failure the lease holder releases it.
        unlinkIfExistsSafe(fs, lockPath(runId));
        return removed;
    };
    return {
        exportResult(runId, result) {
            return mutate(runId, () => {
                if (!_existsSync(runPath(runId)))
                    throw new Error("Run disappeared before result export");
                const json = JSON.stringify({ runId, result }, null, 2);
                const hash = createHash("sha256").update(json).digest("hex");
                const path = `${runPath(runId)}.result-${hash}`;
                if (_existsSync(path))
                    return path;
                const temporary = `${path}.${randomUUID()}.tmp`;
                try {
                    _writeFileSync(temporary, json, { flush: true });
                    fs.renameSync(temporary, path);
                }
                finally {
                    unlinkIfExistsSafe(fs, temporary);
                }
                return path;
            });
        },
        save(state) {
            ensureDir();
            state.updatedAt = new Date().toISOString();
            const path = runPath(state.runId);
            // Atomic write: a crash mid-write can't corrupt the live file (tmp+rename is
            // atomic on the same filesystem). A .bak from the previous good save is the
            // recovery fallback if the primary is somehow truncated.
            mutate(state.runId, () => {
                if (knownRunIds.has(state.runId) && !recordExists(state.runId))
                    throw new RunRecordDeletedError(state.runId);
                const current = records.peek(path);
                if (current)
                    state.sessionId = current.sessionId;
                records.save(path, state);
                knownRunIds.add(state.runId);
            });
            invalidateListCache();
            // Only a terminal write can grow the terminal-run count, so only check
            // the cap then — a "running"/"paused" save is on the hot path (every
            // progress tick) and must not pay for a retention scan.
            if (TERMINAL_RUN_STATUSES.has(state.status))
                enforceRetention();
        },
        loadPreview(runId) {
            const record = records.peek(runPath(runId));
            if (record)
                knownRunIds.add(runId);
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
                if (!current)
                    return null;
                knownRunIds.add(runId);
                if (current.sessionId === sessionId || !from(current.sessionId)) {
                    return { moved: false, sessionId: current.sessionId };
                }
                records.updateMetadata(path, { sessionId });
                return { moved: true, sessionId };
            });
        },
        load(runId) {
            // Null means no record. A damaged one throws RunRecordUnreadableError:
            // reading it as missing would let a caller treat the run as deleted.
            const state = records.read(runPath(runId));
            if (state)
                knownRunIds.add(runId);
            return state;
        },
        list() {
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
        delete(runId) {
            try {
                return mutate(runId, () => deleteRunFiles(runId));
            }
            finally {
                invalidateListCache();
            }
        },
        acquireRunLease: acquireLease,
        releaseRunLease: releaseLease,
        getRunsDir() {
            return runsDir;
        },
    };
}
/**
 * Generate a unique run ID.
 */
export function generateRunId() {
    const timestamp = Date.now().toString(36);
    const random = Math.random().toString(36).slice(2, 8);
    return `${timestamp}-${random}`;
}
