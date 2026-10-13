import { LOADED_BUILD } from "./build-identity.js";
import type { WorkflowManager } from "./workflow-manager.js";

/**
 * Live extension state that Pi may hand from one extension generation to the
 * next during any in-process session replacement: `/reload`, `/new`, resume,
 * and fork. (Process exit / `quit` does not hand off — see the extension's
 * session_shutdown handler.) This deliberately stays process-local: run
 * snapshots and journals already provide the durable cold-start path, while
 * the live manager is what owns in-flight promises, abort controllers, event
 * streams, and the background-result delivery listener.
 *
 * Handoff is a single process-wide slot, not a cwd-keyed map. Pi runs one
 * AgentSessionRuntime at a time; the next factory only knows process.cwd()
 * (often the launch dir), while the live manager may already be bound to the
 * session project cwd (ctx.cwd). A cwd-keyed map made those two paths miss
 * each other. One slot + post-claim project checks is the correct model.
 */
export const WORKFLOW_EXTENSION_BUILD = LOADED_BUILD;

export interface WorkflowReloadRuntime {
  cwd: string;
  /** Build of the code that created this manager. Only the exact same build takes it over. */
  extensionBuild: string;
  manager: WorkflowManager;
  /**
   * Lets go of what the outgoing extension generation still holds for its
   * session, such as the captured host session, once no compatible generation
   * will take the runtime over.
   */
  release?: () => void;
}

export interface WorkflowRuntimeClaim {
  compatible?: WorkflowReloadRuntime;
  buildMismatch?: WorkflowReloadRuntime;
}

interface HandoffSlot {
  runtime: WorkflowReloadRuntime;
  timer: ReturnType<typeof setTimeout>;
}

const RELOAD_HANDOFF_KEY = Symbol.for("@quintinshaw/pi-dynamic-workflows:reload-handoff-slot");
const RELOAD_HANDOFF_TTL_MS = 30_000;

type HandoffRoot = typeof globalThis & { [RELOAD_HANDOFF_KEY]?: HandoffSlot | null };

function getSlot(): HandoffSlot | null {
  return (globalThis as HandoffRoot)[RELOAD_HANDOFF_KEY] ?? null;
}

function setSlot(slot: HandoffSlot | null): void {
  (globalThis as HandoffRoot)[RELOAD_HANDOFF_KEY] = slot;
}

function clearSlot(expected?: HandoffSlot | null): void {
  const current = getSlot();
  if (expected && current !== expected) return;
  if (current) clearTimeout(current.timer);
  setSlot(null);
}

/**
 * Stage a live runtime immediately before Pi tears down the old extension runner
 * for an in-process session replacement (reload / new / resume / fork).
 *
 * Replaces any previously staged runtime (at most one pending handoff per
 * process). If a different runtime was already staged, its still-running runs
 * are paused first — otherwise an overwrite would cancel that runtime's TTL
 * without pausing and leave inflight work burning tokens. The next factory
 * claims via {@link claimWorkflowRuntime} regardless of whether process.cwd()
 * matches the session project cwd.
 *
 * `ttlMs` is only ever overridden by tests; production callers rely on the
 * default so a slow/failed replacement doesn't strand a staged runtime forever.
 */
export function handoffWorkflowRuntime(runtime: WorkflowReloadRuntime, ttlMs: number = RELOAD_HANDOFF_TTL_MS): void {
  const previous = getSlot();
  if (previous && previous.runtime !== runtime) {
    // Overwriting a different staged runtime: pause its live runs now. clearSlot
    // alone would cancel the TTL without pausing (the expiry path is the only
    // other pause trigger, and we are about to clearTimeout it).
    pauseStrandedWorkflowRuntime(previous.runtime);
  }
  clearSlot();
  const slot: HandoffSlot = {
    runtime,
    timer: setTimeout(() => {
      if (getSlot() !== slot) return;
      // No new extension generation ever claimed this runtime. Anything still
      // "running" in it would otherwise burn tokens to completion and try to
      // deliver into a manager nobody can reach anymore, so pause it onto the
      // same journal-recovery path a version-mismatch reload uses.
      pauseStrandedWorkflowRuntime(runtime);
      setSlot(null);
    }, ttlMs),
  };
  slot.timer.unref?.();
  setSlot(slot);
}

/**
 * Take the staged runtime out of the process-wide slot, whatever its project:
 * the caller (extension factory / session_start) decides keep vs rebuild.
 * Returns undefined only when nothing is staged.
 *
 * Prefer {@link claimWorkflowRuntime} for version-aware claiming.
 */
export function takeWorkflowRuntime(): WorkflowReloadRuntime | undefined {
  const slot = getSlot();
  if (!slot) return undefined;
  clearSlot(slot);
  return slot.runtime;
}

/**
 * Claim a staged runtime and compare its build with this extension
 * generation. A different build (another installed copy of the package) falls
 * back to a fresh manager; only the exact same code retains live workflow
 * state (and the delivery listener / pending queue on that manager), so two
 * builds never act on one manager.
 *
 * Independent of cwd: the factory only knows process.cwd(), which may differ
 * from the session project the manager was bound to. Project identity is
 * checked after the claim by the extension (manager.getCwd() vs ctx.cwd).
 */
export function claimWorkflowRuntime(): WorkflowRuntimeClaim {
  const runtime = takeWorkflowRuntime();
  if (!runtime) return {};
  return runtime.extensionBuild === WORKFLOW_EXTENSION_BUILD ? { compatible: runtime } : { buildMismatch: runtime };
}

/**
 * Move a runtime's live runs onto the existing journal recovery path when no
 * compatible manager will carry them forward — process exit, a different
 * extension build, a cross-project handoff rejection, or a staged handoff
 * that expired unclaimed — and release what its generation still holds, since
 * no later generation will rebind it. An extension disabled before a reload
 * reaches this path through the expiry.
 *
 * Enumerates every live in-memory run (listLiveRuns), not the session-filtered
 * listRuns() view — after a session replacement the bound sessionId no longer
 * matches runs whose frozen sessionId is the previous session, and filtering
 * would silently leave them burning tokens.
 */
export function pauseStrandedWorkflowRuntime(runtime: WorkflowReloadRuntime): number {
  const paused = pauseLiveRuns(runtime.manager);
  runtime.release?.();
  return paused;
}

function pauseLiveRuns(manager: WorkflowManager): number {
  let live: Array<{ runId: string; status: string }>;
  try {
    live = manager.listLiveRuns();
  } catch {
    // Stubs in tests / partially constructed managers — nothing to pause.
    return 0;
  }
  let paused = 0;
  for (const run of live) {
    if (run.status === "running" && manager.pause(run.runId)) paused++;
  }
  return paused;
}

/**
 * Session-shutdown reasons that replace the extension runner in-process and
 * immediately load a fresh generation which can claim the staged runtime.
 * `quit` is intentionally absent: nothing will claim after process teardown.
 */
export const SESSION_REPLACEMENT_REASONS = new Set(["reload", "new", "resume", "fork"]);

/**
 * Test/cleanup helper. When `runtime` is provided, only clears if it is the
 * currently staged runtime (identity guard). When omitted, clears whatever is
 * staged.
 */
export function discardWorkflowRuntime(runtime?: WorkflowReloadRuntime): void {
  const slot = getSlot();
  if (!slot) return;
  if (runtime && slot.runtime !== runtime) return;
  clearSlot(slot);
}
