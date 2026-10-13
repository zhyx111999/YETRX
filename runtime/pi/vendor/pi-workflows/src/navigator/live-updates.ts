/** Redraws an open navigator when the workflow manager reports run progress. */

import type { WorkflowManager } from "../workflow-manager.js";
import type { NavigatorState } from "../workflow-ui.js";

const RUN_EVENTS = [
  "started",
  "deleted",
  "agentStart",
  "agentModel",
  "agentEnd",
  "phase",
  "log",
  "tokenUsage",
  "complete",
  "error",
  "stopped",
  "paused",
  "resumed",
];

/** Trailing redraw cadence for agent history updates. */
const HISTORY_REDRAW_MS = 125;

/** Subscribe to manager events; returns the function that unsubscribes. */
export function subscribeToRunUpdates(
  manager: WorkflowManager,
  state: NavigatorState,
  rerender: () => void,
): () => void {
  // Events only request a redraw. Rebuilding the visible list here would hash
  // every saved script and re-aggregate every run several times per second per
  // streaming agent; the render frame does it once and reconciles selection.
  const onEvent = () => rerender();
  for (const ev of RUN_EVENTS) manager.on(ev, onEvent);

  // Histories can update several times per second for every parallel agent.
  // Only agent detail consumes those updates, so ignore unrelated agents and
  // coalesce matching updates into a modest trailing redraw cadence.
  let historyRenderTimer: ReturnType<typeof setTimeout> | undefined;
  let historyRenderTarget: { runId: string; agentId: number } | undefined;
  const onAgentHistory = (event: { runId: string; agentId?: number }) => {
    if (
      state.kind !== "detail" ||
      event.runId !== state.runId ||
      event.agentId === undefined ||
      event.agentId !== state.agentId
    ) {
      return;
    }
    // Keep the newest matching target even while a redraw is already
    // scheduled. If navigation switches agents inside the coalescing window,
    // the pending redraw should follow the new agent rather than the event
    // that originally created the shared timer.
    historyRenderTarget = { runId: event.runId, agentId: event.agentId };
    if (historyRenderTimer) return;
    historyRenderTimer = setTimeout(() => {
      historyRenderTimer = undefined;
      const target = historyRenderTarget;
      historyRenderTarget = undefined;
      if (target && state.kind === "detail" && target.runId === state.runId && target.agentId === state.agentId) {
        rerender();
      }
    }, HISTORY_REDRAW_MS);
    (historyRenderTimer as { unref?: () => void }).unref?.();
  };
  manager.on("agentHistory", onAgentHistory);

  return () => {
    for (const ev of RUN_EVENTS) manager.off(ev, onEvent);
    manager.off("agentHistory", onAgentHistory);
    if (historyRenderTimer) clearTimeout(historyRenderTimer);
    historyRenderTimer = undefined;
    historyRenderTarget = undefined;
  };
}
