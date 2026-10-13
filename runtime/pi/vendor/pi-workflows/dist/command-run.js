import { errorMessage } from "./errors.js";
/**
 * Start a command's workflow without waiting for it: progress shows in the
 * task panel and /workflows, and result delivery posts the outcome when it
 * finishes. Awaiting the run here would block the whole session. A synchronous
 * start failure (script parse, run lease) becomes an error notice.
 */
export function startCommandRun(manager, ctx, run) {
    try {
        const { runId } = manager.startInBackground(run.script, run.args, run.exec ?? {});
        ctx.ui.notify(backgroundStartNotice(run.name, runId, ctx.mode, run.deliverable), "info");
    }
    catch (error) {
        ctx.ui.notify(`/${run.name} failed to start: ${errorMessage(error)}`, "error");
    }
}
/**
 * The notice points at a progress surface the current host has. The task panel
 * and /workflows navigator are TUI components that no-op in RPC hosts even
 * though `ctx.hasUI` is true there, so other modes are pointed at
 * `/workflows status <id>`, which prints plain text any host can display.
 */
function backgroundStartNotice(name, runId, mode, deliverable) {
    const where = mode === "tui" ? "watch the task panel or /workflows" : `check progress with /workflows status ${runId}`;
    return `/${name} running in the background (${runId}) — ${where}; the ${deliverable} is posted here when it finishes.`;
}
