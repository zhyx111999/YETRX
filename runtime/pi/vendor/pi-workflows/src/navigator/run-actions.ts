/** Run actions started from the navigator: confirmed pause and stop, and restart from the recorded run. */

import { errorMessage } from "../errors.js";
import { restartRequest } from "../manager/run-restart.js";
import type { NavigatorSession } from "./session.js";

/** Apply a confirmed pause or stop, re-checking the run's status first. */
export function executeRunConfirmation(session: NavigatorSession, action: "pause" | "stop", runId: string): void {
  const { manager, ui, model } = session;
  const item = model.runs().find((candidate) => candidate.runId === runId);
  if (!item) return;
  const status = item.status;
  if (action === "pause") {
    if (status !== "running") {
      ui.notify(`Cannot pause ${runId}`, "warning");
      return;
    }
    ui.notify(manager.pause(runId) ? `Paused ${runId}` : `Cannot pause ${runId}`, "info");
    return;
  }
  if (status !== "running" && status !== "paused") {
    ui.notify(`Cannot stop ${runId}`, "warning");
    return;
  }
  ui.notify(manager.stop(runId) ? `Stopped ${runId}` : `Cannot stop ${runId}`, "info");
}

/** Start a new background run that repeats the selected run: its script, arguments, and execution options. */
export function restartRun(session: NavigatorSession, runId: string | undefined): void {
  const { manager, ui } = session;
  const run = runId ? manager.findRun(runId) : undefined;
  const restart = run ? restartRequest(run) : undefined;
  if (!run || !restart) {
    ui.notify(runId ? `Cannot restart ${runId} (no script saved)` : "No run selected to restart", "warning");
    return;
  }
  try {
    const { runId: newId } = manager.startInBackground(restart.script, restart.args, restart.exec);
    ui.notify(`Restarted ${run.workflowName || "workflow"} as ${newId}`, "info");
  } catch (error) {
    ui.notify(`Failed to restart ${run.workflowName || "workflow"}: ${errorMessage(error)}`, "error");
  }
}
