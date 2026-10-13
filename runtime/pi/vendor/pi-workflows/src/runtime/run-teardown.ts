/** Ends a run: drains every promise the script was handed, settles the run's verdict once, and closes the callbacks of abandoned agents. */

import {
  errorMessage,
  isWorkflowError,
  WorkflowCheckpointSuspensionError,
  WorkflowError,
  WorkflowErrorCode,
  workflowAbortedError,
  wrapError,
} from "../errors.js";
import type { RunContext } from "./run-context.js";
import { unobservedFailures } from "./script-calls.js";

/** How the script body ended: the value it returned, or the error that escaped it. */
export type ScriptOutcome =
  | { readonly ok: true; readonly value: unknown }
  | { readonly ok: false; readonly error: unknown };

/**
 * End the run after its script body has ended: return the run's result, or
 * throw its terminal error. An error that escaped the script seals the run at
 * once so its siblings stop; a checkpoint suspension waits them out so their
 * paid results journal. Everything else is decided after the drain, because
 * only then is it final which failures the script never observed.
 */
export async function finishRun(ctx: RunContext, outcome: ScriptOutcome): Promise<unknown> {
  const { options, state } = ctx;
  const escaped = !outcome.ok && !(outcome.error instanceof WorkflowCheckpointSuspensionError);
  if (escaped) sealRunFatal(ctx, outcome.error);
  await drainScriptCalls(ctx);
  closeAgentCallbacks(ctx);
  // An escaped error stays the run's error.
  const verdict = escaped ? outcome : settleVerdict(ctx, outcome);
  // Reported after the drain, because agents settling during it commit usage
  // last. Success only: failed and aborted runs own their usage records.
  if (verdict.ok) {
    try {
      options.onTokenUsage?.(state.tokenUsage);
    } catch {
      // Instrumentation must never break teardown or mask the run's outcome.
    }
  }
  ctx.store.dispose();
  if (!verdict.ok) throw verdict.error;
  return verdict.value;
}

/**
 * Called for a fatal error that escaped the script, or that the script never
 * observed; errors that parallel() or a script's own try/catch handled never
 * get here. Aborting in-flight siblings also covers a provider usage limit:
 * work still running on an exhausted quota is thrown away and re-runs on
 * resume, rather than burning more quota now.
 */
function sealRunFatal(ctx: RunContext, error: unknown): void {
  // An escaped error reaches the host before the drain: a pause/stop requested
  // while siblings settle must not erase the error that already ended this run.
  try {
    const observation = ctx.options.onRunFatal?.(error);
    // The hook is observational; consume a rejecting thenable so it cannot
    // become an unhandled rejection.
    if (observation != null && typeof (observation as { then?: unknown }).then === "function") {
      void Promise.resolve(observation).catch(() => {});
    }
  } catch {
    // Instrumentation must never mask the workflow's own terminal error.
  }
  ctx.state.runFatalController.abort();
}

/**
 * The verdict of a run whose script returned or suspended. A fatal failure the
 * script never observed fails the run, before a checkpoint pause, just as an
 * escaped error wins over a suspension the script caught. A suspension pauses
 * the run whether the script awaited, caught or ignored its checkpoint.
 */
function settleVerdict(ctx: RunContext, outcome: ScriptOutcome): ScriptOutcome {
  const { options, state } = ctx;
  const fatal = settleUnobservedFailures(ctx);
  if (fatal) {
    sealRunFatal(ctx, fatal);
    return { ok: false, error: fatal };
  }
  if (!state.checkpointSuspension) return outcome;
  // An external abort keeps priority over the suspension.
  return { ok: false, error: options.signal?.aborted ? workflowAbortedError() : state.checkpointSuspension };
}

/**
 * Log each failure the script never observed and return the first fatal one,
 * classified as parallel() classifies a branch failure: a recoverable failure
 * leaves the run going. A suspension is settled as the run's pause instead,
 * and once the run is aborted its failures are consequences of the abort.
 */
function settleUnobservedFailures(ctx: RunContext): WorkflowError | undefined {
  if (ctx.isAborted()) return undefined;
  let fatal: WorkflowError | undefined;
  for (const { global, error } of unobservedFailures(ctx.state.calls)) {
    if (error instanceof WorkflowCheckpointSuspensionError) continue;
    // A promise the script made itself failed in the script's own code: a bug, never a recoverable miss.
    const workflowError = global ? wrapError(error) : scriptRejection(error);
    ctx.log(
      global
        ? `${global}() failed and the script never awaited it: ${workflowError.message}`
        : `a promise the script created was rejected and never handled: ${workflowError.message}`,
    );
    if (!workflowError.recoverable) fatal ??= workflowError;
  }
  return fatal;
}

function scriptRejection(error: unknown): WorkflowError {
  if (isWorkflowError(error)) return error;
  const message = errorMessage(error);
  return new WorkflowError(message, WorkflowErrorCode.UNKNOWN, { recoverable: false });
}

/** Wait out every tracked promise of the run, including ones the script never awaited. */
async function drainScriptCalls(ctx: RunContext): Promise<void> {
  const { options, state } = ctx;
  const { pending } = state.calls;
  if (pending.size > 0) {
    ctx.log(`waiting for ${pending.size} outstanding workflow promise(s) to settle before this run ends`);
  }
  // A run that is not aborted waits for its outstanding calls without limit:
  // their results are wanted. Once the run is aborted the wait is bounded,
  // because abort is cooperative and a runner that ignores its signal (with no
  // agent timeout configured) would otherwise block the run from ending forever.
  const graceMs = ctx.drainAbortGraceMs;
  // Wakes the wait as soon as either abort source fires, so the grace clock starts.
  let wakeAbort: () => void = () => {};
  const abortWake = new Promise<void>((resolve) => {
    wakeAbort = resolve;
  });
  const externalWake = () => wakeAbort();
  const fatalWake = () => wakeAbort();
  options.signal?.addEventListener("abort", externalWake, { once: true });
  state.runFatalController.signal.addEventListener("abort", fatalWake, { once: true });
  try {
    // Loop: settling calls can start further calls that join the set.
    while (pending.size > 0) {
      const outstanding = Array.from(pending);
      if (!ctx.isAborted() || graceMs === Number.POSITIVE_INFINITY) {
        // Already aborted with unlimited grace: a plain wait. Racing the
        // resolved abortWake there would spin the microtask queue.
        if (ctx.isAborted()) {
          await Promise.allSettled(outstanding);
        } else {
          await Promise.race([Promise.allSettled(outstanding), abortWake]);
        }
        continue;
      }
      let timer: ReturnType<typeof setTimeout> | undefined;
      const grace = new Promise<"timeout">((resolve) => {
        // Deliberately ref'd: an unref'd timer would let the process exit
        // before the run reaches its terminal state.
        timer = setTimeout(() => resolve("timeout"), graceMs);
      });
      const winner = await Promise.race([Promise.allSettled(outstanding).then(() => "settled" as const), grace]);
      if (timer) clearTimeout(timer);
      if (winner === "timeout") {
        // Abandoned promises keep the tracker's rejection handler. Nothing they
        // do later is kept: the store is disposed, the abort check precedes
        // journaling, and the host ignores stale writes.
        ctx.log(
          `abandoning ${outstanding.length} outstanding workflow promise(s) that did not settle within ${graceMs}ms of the drain's abort grace`,
        );
        break;
      }
    }
  } finally {
    options.signal?.removeEventListener("abort", externalWake);
    state.runFatalController.signal.removeEventListener("abort", fatalWake);
  }
  // Node reports an unhandled rejection only once the current microtasks have
  // run; one more turn lets the script's own last rejections arrive.
  await new Promise<void>((resolve) => setImmediate(resolve));
}

/** An abandoned runner may still be live: record its latest usage while the host is listening, then close every callback it still holds. */
function closeAgentCallbacks(ctx: RunContext): void {
  const { state } = ctx;
  for (const finalizeUsage of state.pendingUsageFinalizers) {
    try {
      finalizeUsage();
    } catch {
      // Best-effort: one broken finalizer must not leave the others open.
    }
  }
  state.pendingUsageFinalizers.clear();
  state.agentCallbacksClosed = true;
}
