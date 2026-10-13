// Which session receives a run's result. The record's sessionId is the only
// authority: processes sharing a runs directory move it with
// RunPersistence.moveSession(), and a full save never overwrites it, so a
// ManagedRun's sessionId is only this process's copy, refreshed from the record
// whenever this process moves, saves, or delivers the run.
import { reportDiagnostic } from "../host-diagnostics.js";
/**
 * Move `run`'s result to `sessionId` when `from` accepts the record's current
 * owner, and copy the owner the record has afterwards into `run`. Without a
 * record the rule applies to the copy alone, which seeds the record if it is
 * ever written. Best-effort like every progress write: a failure keeps both.
 */
export function moveRunSession(persistence, run, sessionId, from) {
    let after;
    try {
        after = persistence.moveSession(run.runId, sessionId, from);
    }
    catch (error) {
        reportDiagnostic(`Could not move workflow run ${run.runId} to session ${sessionId}`, error);
        return "failed";
    }
    if (!after) {
        if (from(run.sessionId))
            run.sessionId = sessionId;
        return "missing";
    }
    run.sessionId = after.sessionId;
    return after.moved ? "moved" : "kept";
}
/** The record's owner of `run`, copied into it; the copy's own when the record is missing or unreadable. */
export function refreshRunSession(persistence, run) {
    try {
        const record = persistence.loadPreview(run.runId);
        if (record)
            run.sessionId = record.sessionId;
    }
    catch {
        // A damaged record keeps the owner this process saw last.
    }
    return run.sessionId;
}
/**
 * The owner of a run finished in this process, claimed for `sessionId` when it
 * has none yet (it finished before the first session bound). Takes the write
 * lock only to claim, never to read.
 */
export function claimRunSession(persistence, run, sessionId) {
    if (refreshRunSession(persistence, run) === undefined) {
        moveRunSession(persistence, run, sessionId, (owner) => owner === undefined);
    }
    return run.sessionId;
}
/**
 * After a session replacement, move the work `previousSessionId` owns to
 * `sessionId`: its running and paused runs in memory, and every undelivered
 * result, in memory or only on disk. Work another session owns stays there,
 * even when this process executes it or keeps a copy. A run without an owner
 * moves only from memory, where it is this manager's own. Returns how many
 * runs moved; `onMissing` gets in-memory runs whose record is gone.
 */
export function adoptRunsToSession(persistence, runs, sessionId, previousSessionId, onMissing) {
    const fromPrevious = (owner) => owner === previousSessionId;
    let moved = 0;
    for (const run of [...runs.values()]) {
        const active = run.status === "running" || run.status === "paused";
        if (!active && run.pendingDelivery == null)
            continue;
        const move = moveRunSession(persistence, run, sessionId, fromPrevious);
        if (move === "moved")
            moved++;
        else if (move === "missing")
            onMissing(run);
    }
    if (previousSessionId === undefined)
        return moved;
    try {
        for (const state of persistence.list()) {
            if (!state.pendingDelivery || runs.has(state.runId) || state.sessionId !== previousSessionId)
                continue;
            if (persistence.moveSession(state.runId, sessionId, fromPrevious)?.moved)
                moved++;
        }
    }
    catch {
        // Best-effort: the in-memory runs are the ones that matter most.
    }
    return moved;
}
