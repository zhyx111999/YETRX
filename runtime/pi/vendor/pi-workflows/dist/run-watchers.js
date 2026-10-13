// Live status-bar watchers for `/workflows watch`: one per run, never throwing into the run, gone when their session ends.
import { recomputeWorkflowSnapshot, renderWorkflowText } from "./display.js";
const PROGRESS_EVENTS = ["agentStart", "agentEnd", "phase", "log", "tokenUsage"];
const FINAL_EVENTS = ["complete", "error", "stopped", "paused", "deleted"];
/** Watchers by run id. Watching a run again replaces its watcher, so the newest context reports. */
const watchers = new Map();
function oneLineProgress(snapshot) {
    const total = snapshot.agents.length;
    const done = snapshot.agents.filter((a) => a.status === "done").length;
    const running = snapshot.agents.filter((a) => a.status === "running").length;
    const errs = snapshot.agents.filter((a) => a.status === "error").length;
    const phase = snapshot.currentPhase ? ` · ${snapshot.currentPhase}` : "";
    return `◆ ${snapshot.name}: ${done}/${total} done${running ? `, ${running} running` : ""}${errs ? `, ${errs} err` : ""}${phase}`;
}
/**
 * Stream a running run's progress to the status bar and print its final
 * snapshot when it ends. Returns false when the run is not running here. The
 * listeners run inside the manager's event emission, i.e. inside the run, so
 * they never throw: a context that went stale (its session was replaced)
 * ends the watcher instead.
 */
export function watchRun(manager, pi, ctx, id) {
    if (manager.getRun(id)?.status !== "running")
        return false;
    watchers.get(id)?.();
    const key = `wf:${id}`;
    const onProgress = (event) => {
        if (event && event.runId !== id)
            return;
        const run = manager.getRun(id);
        if (!run)
            return;
        try {
            ctx.ui.setStatus(key, oneLineProgress(run.snapshot));
        }
        catch {
            stop();
        }
    };
    const onFinal = (event) => {
        if (event && event.runId !== id)
            return;
        const run = manager.getRun(id);
        stop();
        if (!run)
            return;
        try {
            pi.sendMessage({
                customType: "workflows",
                content: renderWorkflowText(recomputeWorkflowSnapshot(run.snapshot), true),
                display: true,
            });
        }
        catch {
            // The session this watcher reported to is gone; the run itself is unaffected.
        }
    };
    const stop = () => {
        if (watchers.get(id) !== stop)
            return;
        watchers.delete(id);
        for (const event of PROGRESS_EVENTS)
            manager.off(event, onProgress);
        for (const event of FINAL_EVENTS)
            manager.off(event, onFinal);
        try {
            ctx.ui.setStatus(key, undefined);
        }
        catch {
            // A stale context has no status bar left to clear.
        }
    };
    watchers.set(id, stop);
    for (const event of PROGRESS_EVENTS)
        manager.on(event, onProgress);
    for (const event of FINAL_EVENTS)
        manager.on(event, onFinal);
    onProgress(undefined);
    return true;
}
/** End every watcher; their contexts belong to a session that is shutting down. */
export function stopRunWatchers() {
    for (const stop of [...watchers.values()])
        stop();
}
