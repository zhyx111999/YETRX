// Aborts a run's execution on a user pause or stop, recording which one asked.
/**
 * Abort for an explicit user lifecycle action. The fresh reason object is an
 * execution-scoped identity token. An already-aborted controller keeps its
 * original reason, so the earlier cause stays observable when the execution settles.
 */
export function abortForLifecycleControl(run, action) {
    if (run.controller.signal.aborted)
        return;
    const abortReason = {};
    run.lifecycleControl = { action, abortReason };
    run.controller.abort(abortReason);
}
