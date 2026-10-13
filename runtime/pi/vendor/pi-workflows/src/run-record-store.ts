/**
 * Versioned run records: append changed cells, or a snapshot once the history
 * outgrows the run, then atomically commit a small index head.
 */
import { createHash, randomUUID } from "node:crypto";
import { basename, dirname, join } from "node:path";
import { aggregateAgentUsage } from "./display.js";
import { errorMessage, RunRecordUnreadableError } from "./errors.js";
import { type PersistenceFsLayer, unlinkIfExistsSafe, writeJsonAtomicWithBackup } from "./fs-persistence.js";
import { INTERRUPTED_AGENT_CAUSE, settleInterruptedPersistedAgents } from "./run-agent-settlement.js";
import type { PersistedRunState } from "./run-persistence.js";

interface RunSummary {
  total: number;
  done: number;
  active: number;
  running: number;
  queued: number;
  error: number;
  skipped: number;
  activeLabels: string[];
  checkpoint: Pick<NonNullable<PersistedRunState["checkpoint"]>, "checkpointId" | "kind" | "status"> | null;
  usage: ReturnType<typeof aggregateAgentUsage>;
}
const summaries = new WeakMap<PersistedRunState, RunSummary>();
export function runSummary(state: PersistedRunState): RunSummary {
  const cached = summaries.get(state);
  if (cached) return cached;
  const agents = Array.isArray(state.agents) ? state.agents.filter((a) => a && typeof a === "object") : [];
  return {
    total: agents.length,
    done: agents.filter((a) => a.status === "done").length,
    active: agents.filter((a) => a.status === "running" || a.status === "queued").length,
    running: agents.filter((a) => a.status === "running").length,
    queued: agents.filter((a) => a.status === "queued").length,
    error: agents.filter((a) => a.status === "error").length,
    skipped: agents.filter((a) => a.status === "skipped").length,
    activeLabels: agents.filter((a) => a.status === "running").map((a) => a.label),
    checkpoint: state.checkpoint
      ? { checkpointId: state.checkpoint.checkpointId, kind: state.checkpoint.kind, status: state.checkpoint.status }
      : null,
    usage: aggregateAgentUsage(agents),
  };
}

// Only small routing/status fields belong in the index. Arbitrary user payloads,
// scripts, checkpoint responses, agent histories and results remain in the log.
const INDEX_KEYS = new Set([
  "runId",
  "workflowName",
  "status",
  "sessionId",
  "parentSessionId",
  "parentSessionFile",
  "startedAt",
  "updatedAt",
  "completedAt",
  "durationMs",
  "tokenUsage",
  "currentPhase",
  "pauseReason",
  "resetHint",
  "autoResumeAttempts",
]);
interface Head {
  format: "pi-workflow-run-v2";
  generation: string;
  bytes: number;
  sequence: number;
  hash: string;
  keys: string[];
  index: Record<string, unknown>;
  summary: RunSummary;
  pendingDelivery?: { kind: "complete" | "text"; deliveryId?: string };
}
type HeadFields = Pick<Head, "keys" | "index" | "summary" | "pendingDelivery">;
function markerIndex(marker: Head["pendingDelivery"]): Head["pendingDelivery"] {
  return marker ? { kind: marker.kind, deliveryId: marker.deliveryId } : undefined;
}
type Cell = string | string[] | Map<string, string>;
type Cells = Map<string, Cell>;
interface Delta {
  settleAgentsAt?: string;
  set: Record<string, unknown>;
  remove: string[];
  arrays: Record<string, { length: number; set: [number, unknown][] }>;
  objects?: Record<string, { set: Record<string, unknown>; remove: string[] }>;
}
interface Entry {
  generation: string;
  sequence: number;
  previous: string;
  delta: Delta;
}
interface Cached {
  commit: string;
  stamp: string;
  state: PersistedRunState;
  cells: Cells;
  size: number;
}
const FORMAT = "pi-workflow-run-v2";
// Generations name staged log files, so they must be plain file-name tokens.
const GENERATION = /^[\w-]+$/;
const MAX_CACHE_ENTRIES = 8;
const MAX_CACHE_BYTES = 16 * 1024 * 1024;
// A save rewrites the log as a one-entry snapshot once replaying it costs
// several times the run itself, so each snapshot follows at least three times
// its size in appended history. Below the floor replay is cheap anyway, and
// tiny runs are not snapshotted every few saves.
const SNAPSHOT_RATIO = 4;
const SNAPSHOT_MIN_LOG_BYTES = 16 * 1024;
// Readers take no lock, so a concurrent generation switch can promote or
// retire the log a just-read head names; they retry with the head on disk.
const READ_ATTEMPTS = 3;

function isHead(value: unknown): value is Head {
  return !!value && typeof value === "object" && (value as Head).format === FORMAT;
}
function validateHead(record: unknown): asserts record is Head {
  if (!isHead(record)) throw new Error("Unsupported run record format");
  if (
    !Number.isSafeInteger(record.bytes) ||
    record.bytes <= 0 ||
    !Number.isSafeInteger(record.sequence) ||
    record.sequence <= 0 ||
    typeof record.generation !== "string" ||
    !GENERATION.test(record.generation) ||
    typeof record.hash !== "string" ||
    !record.index ||
    typeof record.index.runId !== "string" ||
    !record.summary ||
    !Array.isArray(record.keys) ||
    record.keys.some((key) => typeof key !== "string")
  )
    throw new Error("Invalid run head");
}
/**
 * The committed head, from the head file or, when that is torn or damaged, its
 * backup. Null only when neither file exists. A head of another format came
 * from a different writer version and is never bypassed for an older backup;
 * when every file present is damaged the record fails closed instead of
 * reading as missing, so no write can recreate the run over it.
 */
function readHead(fs: PersistenceFsLayer, path: string): Head | null {
  let damage: string | undefined;
  for (const candidate of [path, `${path}.bak`]) {
    let record: unknown;
    try {
      record = JSON.parse(fs.readFileSync(candidate, "utf-8"));
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code !== "ENOENT") damage ??= (error as Error).message;
      continue;
    }
    const format = (record as { format?: unknown } | null)?.format;
    if (typeof format === "string" && format !== FORMAT) throw new Error("Unsupported run record format");
    try {
      validateHead(record);
      return record;
    } catch (error) {
      damage ??= (error as Error).message;
    }
  }
  if (damage !== undefined) throw new RunRecordUnreadableError(path, damage);
  return null;
}
function digest(line: string): string {
  return createHash("sha256").update(line).digest("hex");
}
/** Everything a head commits to; a cached replay is reused only for an identical commit. */
function commitOf(head: Head): string {
  return JSON.stringify([head.generation, head.sequence, head.bytes, head.hash, head.index.runId]);
}
function cellsOf(state: PersistedRunState): Cells {
  const cells: Cells = new Map();
  for (const [key, value] of Object.entries(state)) {
    if (value === undefined) continue;
    if (Array.isArray(value))
      cells.set(
        key,
        value.map((v) => JSON.stringify(v) ?? "null"),
      );
    else if (
      value &&
      typeof value === "object" &&
      Object.getPrototypeOf(value) === Object.prototype &&
      typeof (value as { toJSON?: unknown }).toJSON !== "function"
    ) {
      const entries = Object.entries(value).flatMap(([name, item]) => {
        const json = JSON.stringify(item);
        return json === undefined ? [] : [[name, json] as const];
      });
      cells.set(key, new Map(entries));
    } else cells.set(key, JSON.stringify(value));
  }
  return cells;
}
function cellsSize(cells: Cells, measure: (json: string) => number): number {
  let size = 0;
  for (const cell of cells.values())
    if (typeof cell === "string") size += measure(cell);
    else for (const json of cell.values()) size += measure(json);
  return size;
}
/** Whether replaying a head's log costs enough more than a snapshot of `cells` to rewrite it as one. */
function outgrown(head: Head, cells: Cells): boolean {
  return (
    head.bytes >= SNAPSHOT_MIN_LOG_BYTES &&
    head.bytes >= SNAPSHOT_RATIO * cellsSize(cells, (json) => Buffer.byteLength(json))
  );
}
function headFields(state: PersistedRunState, cells: Cells): HeadFields {
  return {
    keys: [...cells.keys()],
    index: Object.fromEntries(
      [...cells]
        .filter(([key]) => INDEX_KEYS.has(key))
        .map(([key, value]) => [
          key,
          value instanceof Map
            ? Object.fromEntries([...value].map(([name, json]) => [name, JSON.parse(json)]))
            : Array.isArray(value)
              ? value.map((json) => JSON.parse(json))
              : JSON.parse(value),
        ]),
    ),
    summary: runSummary(state),
    pendingDelivery: markerIndex(state.pendingDelivery),
  };
}
function deltaOf(before: Cells, after: Cells): Delta {
  const delta: Delta = { set: Object.create(null), remove: [], arrays: Object.create(null) };
  for (const key of before.keys()) if (!after.has(key)) delta.remove.push(key);
  for (const [key, value] of after) {
    const old = before.get(key);
    if (Array.isArray(value)) {
      const prior = Array.isArray(old) ? old : [];
      const set: [number, unknown][] = [];
      for (let i = 0; i < value.length; i++) if (value[i] !== prior[i]) set.push([i, JSON.parse(value[i])]);
      if (!Array.isArray(old) || value.length !== prior.length || set.length)
        delta.arrays[key] = { length: value.length, set };
    } else if (value instanceof Map) {
      const prior = old instanceof Map ? old : new Map();
      const set: Record<string, unknown> = Object.create(null);
      const remove = [...prior.keys()].filter((name) => !value.has(name));
      for (const [name, json] of value) if (prior.get(name) !== json) set[name] = JSON.parse(json);
      if (!(old instanceof Map)) delta.set[key] = {};
      if (remove.length || Object.keys(set).length) {
        const objects: NonNullable<Delta["objects"]> = delta.objects ?? Object.create(null);
        objects[key] = { set, remove };
        delta.objects = objects;
      }
    } else if (value !== old) delta.set[key] = JSON.parse(value);
  }
  return delta;
}
function applyDelta(state: Record<string, unknown>, delta: Delta): void {
  if (delta.settleAgentsAt)
    state.agents = settleInterruptedPersistedAgents(
      Array.isArray(state.agents) ? state.agents : [],
      INTERRUPTED_AGENT_CAUSE,
      delta.settleAgentsAt,
    );
  for (const key of delta.remove) delete state[key];
  for (const [key, value] of Object.entries(delta.set))
    Object.defineProperty(state, key, { value, writable: true, enumerable: true, configurable: true });
  for (const [key, patch] of Object.entries(delta.objects ?? {})) {
    const value =
      Object.hasOwn(state, key) && state[key] && typeof state[key] === "object" && !Array.isArray(state[key])
        ? (state[key] as Record<string, unknown>)
        : {};
    for (const name of patch.remove) delete value[name];
    for (const [name, item] of Object.entries(patch.set))
      Object.defineProperty(value, name, { value: item, writable: true, enumerable: true, configurable: true });
    Object.defineProperty(state, key, { value, writable: true, enumerable: true, configurable: true });
  }
  for (const [key, patch] of Object.entries(delta.arrays)) {
    if (!Number.isSafeInteger(patch.length) || patch.length < 0) throw new Error("Invalid run array length");
    const value = Object.hasOwn(state, key) && Array.isArray(state[key]) ? (state[key] as unknown[]) : [];
    value.length = patch.length;
    for (const [index, item] of patch.set) {
      if (!Number.isSafeInteger(index) || index < 0 || index >= patch.length)
        throw new Error("Invalid run array index");
      value[index] = item;
    }
    Object.defineProperty(state, key, { value, writable: true, enumerable: true, configurable: true });
  }
}

export function createRunRecordStore(fs: PersistenceFsLayer) {
  const cache = new Map<string, Cached>();
  const logPath = (path: string) => `${path}.events.jsonl`;
  const stagedLogPath = (path: string, generation: string) => `${path}.events.${generation}.jsonl`;
  /** Staged generation logs beside a record, whether committed or abandoned. */
  function stagedLogs(path: string): string[] {
    const dir = dirname(path);
    const prefix = `${basename(path)}.events.`;
    let names: string[];
    try {
      names = fs.readdirSync(dir);
    } catch {
      return [];
    }
    return names
      .filter(
        (name) =>
          name.startsWith(prefix) &&
          name.endsWith(".jsonl") &&
          GENERATION.test(name.slice(prefix.length, -".jsonl".length)),
      )
      .map((name) => join(dir, name));
  }
  /** The file holding a head's log, and the identity of its current contents. */
  function logFile(path: string, head: Head): { file: string; size: number; stamp: string } {
    // A generation switch commits its head before promoting the staged log.
    const staged = stagedLogPath(path, head.generation);
    const file = fs.existsSync(staged) ? staged : logPath(path);
    const s = fs.statSync(file);
    return { file, size: s.size, stamp: `${s.ino}:${s.size}:${s.mtimeMs}:${s.ctimeMs}` };
  }
  function remember(path: string, head: Head, stamp: string, state: PersistedRunState, cells: Cells): void {
    cache.delete(path);
    // The replayed state and its serialized cells both stay resident as UTF-16 strings.
    const size = 2 * 2 * cellsSize(cells, (json) => json.length);
    if (size <= MAX_CACHE_BYTES) cache.set(path, { commit: commitOf(head), stamp, state, cells, size });
    let total = [...cache.values()].reduce((n, v) => n + v.size, 0);
    while (cache.size > MAX_CACHE_ENTRIES || total > MAX_CACHE_BYTES) {
      const key = cache.keys().next().value as string;
      total -= cache.get(key)?.size ?? 0;
      cache.delete(key);
    }
  }
  function readPrefix(file: string, length: number): string {
    const fd = fs.openSync(file, "r");
    try {
      const bytes = Buffer.alloc(length);
      let read = 0;
      while (read < bytes.length) {
        const n = fs.readSync(fd, bytes, read, bytes.length - read, read);
        if (!n) throw new Error("Truncated committed run log");
        read += n;
      }
      return bytes.toString("utf8");
    } finally {
      fs.closeSync(fd);
    }
  }
  /** The full run a validated head commits to, replayed from its log (or the cache). */
  function hydrate(path: string, head: Head): PersistedRunState {
    const { file, size, stamp } = logFile(path, head);
    const cached = cache.get(path);
    if (cached?.commit === commitOf(head) && cached.stamp === stamp) {
      cache.delete(path);
      cache.set(path, cached);
      return cached.state;
    }
    if (size < head.bytes) throw new Error("Truncated committed run log");
    const raw = readPrefix(file, head.bytes);
    if (!raw.endsWith("\n")) throw new Error("Incomplete committed run entry");
    const state: Record<string, unknown> = {};
    let previous = "",
      sequence = 0;
    for (const line of raw.slice(0, -1).split("\n")) {
      const entry = JSON.parse(line) as Entry;
      if (entry.generation !== head.generation || entry.sequence !== ++sequence || entry.previous !== previous)
        throw new Error("Invalid run log chain");
      applyDelta(state, entry.delta);
      previous = digest(line);
    }
    if (previous !== head.hash || sequence !== head.sequence || state.runId !== head.index.runId)
      throw new Error("Run commit mismatch");
    const result = state as unknown as PersistedRunState;
    remember(path, head, stamp, result, cellsOf(result));
    return result;
  }
  /** Replays `head`, or the head now on disk when a concurrent switch moved its log. A damaged commit still fails. */
  function replay(path: string, head: Head): PersistedRunState {
    let current = head;
    for (let attempt = 1; ; attempt++) {
      try {
        return hydrate(path, current);
      } catch (error) {
        const latest = attempt < READ_ATTEMPTS ? readHead(fs, path) : null;
        if (!latest || latest.index.runId !== current.index.runId) throw error;
        current = latest;
      }
    }
  }
  /**
   * The run `head` commits to. A committed-but-damaged log fails closed,
   * rather than silently replaying an older paid-call prefix; only a corrupt
   * or missing head uses its backup.
   */
  function replayCommitted(path: string, head: Head): PersistedRunState {
    try {
      return replay(path, head);
    } catch (error) {
      throw error instanceof RunRecordUnreadableError ? error : new RunRecordUnreadableError(path, errorMessage(error));
    }
  }
  function read(path: string): PersistedRunState | null {
    const head = readHead(fs, path);
    return head ? structuredClone(replayCommitted(path, head)) : null;
  }
  /**
   * The light view of a run, or null when it has none. A head whose log is
   * missing or shorter than its commit (a retired generation's, or a torn
   * file) is unreadable here already, so a view never lists a record load()
   * cannot read; damage inside a log of the right size surfaces when a lazy
   * field is read.
   */
  function peek(path: string): PersistedRunState | null {
    const head = readHead(fs, path);
    if (!head) return null;
    let size: number;
    try {
      ({ size } = logFile(path, head));
    } catch (error) {
      throw new RunRecordUnreadableError(path, `committed run log is missing (${errorMessage(error)})`);
    }
    if (size < head.bytes) throw new RunRecordUnreadableError(path, "Truncated committed run log");
    return preview(path, head);
  }
  /** A light view of a run from its head; fields outside the index load the log on first access. */
  function preview(path: string, head: Head): PersistedRunState {
    const { index } = head;
    const view = { ...index } as unknown as PersistedRunState;
    // Getters close over only the small head/path, never the hydrated state.
    // Enumeration and spread still see every field of the full record.
    for (const key of head.keys)
      if (!Object.hasOwn(index, key))
        Object.defineProperty(view, key, {
          enumerable: true,
          configurable: true,
          get: () => structuredClone((replayCommitted(path, head) as unknown as Record<string, unknown>)[key]),
        });
    // Markers are needed to route pending delivery without loading results.
    if (head.pendingDelivery) {
      const marker = { ...head.pendingDelivery };
      if (marker.kind === "text")
        Object.defineProperty(marker, "text", {
          enumerable: true,
          get: () => {
            const full = replayCommitted(path, head).pendingDelivery;
            return full?.kind === "text" ? full.text : undefined;
          },
        });
      Object.defineProperty(view, "pendingDelivery", {
        value: marker,
        enumerable: true,
        configurable: true,
      });
    }
    summaries.set(view, head.summary);
    return view;
  }
  /** Finishes a generation switch whose head committed before its staged log was promoted. */
  function promote(path: string, head: Head): void {
    const staged = stagedLogPath(path, head.generation);
    if (fs.existsSync(staged)) fs.renameSync(staged, logPath(path));
  }
  /** Commits a head naming an entry already flushed to its log; the head rename is the commit point. */
  function publish(path: string, entry: Entry, line: string, offset: number, fields: HeadFields): Head {
    const next: Head = {
      ...fields,
      format: FORMAT,
      generation: entry.generation,
      bytes: offset + Buffer.byteLength(line) + 1,
      sequence: entry.sequence,
      hash: digest(line),
    };
    writeJsonAtomicWithBackup(fs, path, next);
    cache.delete(path);
    return next;
  }
  function append(path: string, head: Head, delta: Delta, fields: HeadFields): Head {
    promote(path, head);
    const entry: Entry = { generation: head.generation, sequence: head.sequence + 1, previous: head.hash, delta };
    const line = JSON.stringify(entry);
    const log = logPath(path);
    const size = fs.statSync(log).size;
    if (size < head.bytes) throw new Error("Truncated committed run log");
    // Bytes past the boundary belong to an append whose head never committed.
    if (size > head.bytes) fs.truncateSync(log, head.bytes);
    fs.writeFileSync(log, `${line}\n`, { flag: "a", flush: true });
    return publish(path, entry, line, head.bytes, fields);
  }
  /**
   * Starts a generation whose log is one snapshot of the whole run. The snapshot
   * is staged under its own name, so until the new head commits, the previous
   * head and its log stay intact; only then does the snapshot replace the log.
   * Every crash point leaves one committed head whose log is either the final
   * file or its staged file, and the next writer promotes or discards the rest.
   */
  function startGeneration(path: string, head: Head | undefined, snapshot: Delta, fields: HeadFields): Head {
    if (head) promote(path, head);
    // Every staged log left now belongs to a switch that never committed.
    for (const abandoned of stagedLogs(path)) unlinkIfExistsSafe(fs, abandoned);
    const entry: Entry = { generation: randomUUID(), sequence: 1, previous: "", delta: snapshot };
    const line = JSON.stringify(entry);
    const staged = stagedLogPath(path, entry.generation);
    fs.writeFileSync(staged, `${line}\n`, { flush: true });
    const next = publish(path, entry, line, 0, fields);
    try {
      fs.renameSync(staged, logPath(path));
    } catch {
      // Already committed: readers use the staged log until the next writer promotes it.
    }
    return next;
  }
  function save(path: string, state: PersistedRunState): void {
    const head = readHead(fs, path) ?? undefined;
    // Replaying the committed run also refuses to write over a damaged log.
    const prior = head ? hydrate(path, head) : undefined;
    const after = cellsOf(state);
    const fields = headFields(state, after);
    if (head && prior && !outgrown(head, after)) {
      const delta = deltaOf(cache.get(path)?.cells ?? cellsOf(prior), after);
      const next = append(path, head, delta, fields);
      // Internal cached records never escape; reuse unchanged cells in memory.
      applyDelta(prior as unknown as Record<string, unknown>, delta);
      remember(path, next, logFile(path, next).stamp, prior, after);
      return;
    }
    const snapshot = deltaOf(new Map(), after);
    const next = startGeneration(path, head, snapshot, fields);
    const fresh: Record<string, unknown> = {};
    applyDelta(fresh, snapshot);
    remember(path, next, logFile(path, next).stamp, fresh as unknown as PersistedRunState, after);
  }
  function updateMetadata(
    path: string,
    patch: Partial<Pick<PersistedRunState, "sessionId" | "pendingDelivery" | "autoResumeAttempts">>,
    expectedDeliveryId?: string,
  ): boolean {
    const record = readHead(fs, path);
    if (!record) return false;
    if (
      expectedDeliveryId &&
      record.pendingDelivery?.deliveryId &&
      record.pendingDelivery.deliveryId !== expectedDeliveryId
    )
      return false;
    const updated = { ...patch, updatedAt: new Date().toISOString() };
    const after = cellsOf(updated as PersistedRunState);
    const delta = deltaOf(new Map(Object.keys(updated).map((key) => [key, "null"])), after);
    const index = { ...record.index };
    for (const [key, value] of Object.entries(updated))
      if (INDEX_KEYS.has(key)) {
        if (value === undefined) delete index[key];
        else index[key] = value;
      }
    const keys = new Set(record.keys);
    for (const key of Object.keys(updated))
      if (after.has(key)) keys.add(key);
      else keys.delete(key);
    append(path, record, delta, {
      summary: record.summary,
      index,
      keys: [...keys],
      pendingDelivery: Object.hasOwn(patch, "pendingDelivery")
        ? markerIndex(patch.pendingDelivery)
        : record.pendingDelivery,
    });
    return true;
  }
  function recoverInterrupted(path: string): boolean {
    const head = readHead(fs, path);
    if (!head) return false;
    const status = head.index.status;
    const { summary } = head;
    if (status !== "running" && !(status === "paused" && summary.active > 0)) return false;
    const endedAt = new Date().toISOString();
    append(
      path,
      head,
      {
        set: { status: "paused", pauseReason: "interrupted", updatedAt: endedAt },
        remove: [],
        arrays: {},
        settleAgentsAt: endedAt,
      },
      {
        keys: [...new Set([...head.keys, "status", "pauseReason", "updatedAt", "agents"])],
        index: { ...head.index, status: "paused", pauseReason: "interrupted", updatedAt: endedAt },
        summary: {
          ...summary,
          active: 0,
          running: 0,
          queued: 0,
          skipped: summary.skipped + summary.active,
          activeLabels: [],
        },
        pendingDelivery: head.pendingDelivery,
      },
    );
    return true;
  }
  return {
    read,
    peek,
    save,
    updateMetadata,
    recoverInterrupted,
    forget: (path: string) => cache.delete(path),
    /** Every log file of a record, including staged generation logs. */
    logFiles: (path: string) => [logPath(path), ...stagedLogs(path)],
  };
}
