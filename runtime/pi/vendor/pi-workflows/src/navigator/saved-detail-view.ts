/** The saved-workflow detail page: its metadata and highlighted script. */

import type { MarkdownTheme } from "@earendil-works/pi-tui";
import type { ThemeLike } from "../display.js";
import type { NavigatorModel, NavigatorState } from "../workflow-ui.js";
import { asText } from "./as-text.js";
import type { NavigatorTextRenderCache } from "./render-cache.js";
import { renderCodeLines } from "./rich-text.js";
import { scrollableBody } from "./viewport.js";

export function renderSavedDetail(
  state: NavigatorState,
  model: NavigatorModel,
  width: number,
  theme: ThemeLike,
  viewportRows: number,
  markdownTheme: MarkdownTheme | undefined,
  renderCache: NavigatorTextRenderCache | undefined,
): string[] {
  const dim = (t: string) => theme.fg("dim", t);
  const saved = model.saved();
  const w = state.activeSaved(model.visible(state.filter), saved);
  const lines = [theme.bold(w ? w.name : "saved workflow")];
  if (!w) return lines;
  const body: string[] = [];
  if (w.description) body.push(dim("Description: ") + asText(w.description));
  body.push(dim("Location: ") + (w.location === "user" ? "user (~/.pi)" : "project (.pi)"));
  body.push(dim("Saved at: ") + asText(w.savedAt));
  if (w.parameters) body.push(dim("Parameters: ") + JSON.stringify(w.parameters));
  body.push("", theme.fg("accent", theme.bold("Script:")));
  // Corrupt saved-workflow JSON can carry a non-string script.
  body.push(...renderCodeLines(asText(w.script), "javascript", width, markdownTheme, renderCache));
  lines.push(...scrollableBody(state, body, viewportRows, dim));
  return lines;
}
