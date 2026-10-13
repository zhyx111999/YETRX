/** Composes one navigator frame: the active view, then the prompt and key-hint footer, clipped to the viewport. */

import type { MarkdownTheme } from "@earendil-works/pi-tui";
import type { ThemeLike } from "../display.js";
import type { NavigatorModel, NavigatorState } from "../workflow-ui.js";
import { renderAgentDetail } from "./agent-detail-view.js";
import { footerLines } from "./footer.js";
import type { NavigatorSnapshot } from "./items.js";
import type { NavigatorTextRenderCache } from "./render-cache.js";
import { renderRunView } from "./run-view.js";
import { renderRunsList } from "./runs-list-view.js";
import { renderSavedDetail } from "./saved-detail-view.js";

/** Callers wrap this in `model.withRenderFrame` so every view reads one consistent data set. */
export function renderNavigatorFrame(
  state: NavigatorState,
  model: NavigatorModel,
  width: number,
  theme: ThemeLike,
  viewportRows: number,
  markdownTheme: MarkdownTheme | undefined,
  renderCache: NavigatorTextRenderCache | undefined,
): string[] {
  const lines: string[] = [];
  let snapshot: NavigatorSnapshot | undefined;
  state.setPageSize(Math.max(1, viewportRows - 5));
  if (state.kind === "runs") {
    snapshot = model.visible(state.effectiveFilter());
    lines.push(...renderRunsList(state, snapshot, theme, viewportRows));
  } else if (state.runId && (state.kind === "phases" || (state.kind === "agents" && state.phase !== undefined))) {
    lines.push(...renderRunView(state, model, state.runId, width, theme, viewportRows));
  } else if (state.kind === "detail" && state.runId && state.agentId != null) {
    lines.push(
      ...renderAgentDetail(
        state,
        model,
        state.runId,
        state.agentId,
        width,
        theme,
        viewportRows,
        markdownTheme,
        renderCache,
      ),
    );
  } else if (state.kind === "savedDetail" && state.savedName) {
    lines.push(...renderSavedDetail(state, model, width, theme, viewportRows, markdownTheme, renderCache));
  }
  lines.push(...footerLines(state, model, theme, snapshot));
  return lines.slice(0, Math.max(0, viewportRows));
}
