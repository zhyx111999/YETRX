// Aborts a run's execution on a user pause or stop, recording which one asked.

/** A user pause()/stop() that aborted one execution; `abortReason` is the AbortSignal.reason it set. */
export interface LifecycleControl {
  action: "pause" | "stop";
  abortReason: object;
}

/**
 * Abort for an explicit user lifecycle action. The fresh reason object is an
 * execution-scoped identity token. An already-aborted controller keeps its
 * original reason, so the earlier cause stays observable when the execution settles.
 */
export function abortForLifecycleControl(
  run: { controller: AbortController; lifecycleControl?: LifecycleControl },
  action: LifecycleControl["action"],
): void {
  if (run.controller.signal.aborted) return;
  const abortReason = {};
  run.lifecycleControl = { action, abortReason };
  run.controller.abort(abortReason);
}
