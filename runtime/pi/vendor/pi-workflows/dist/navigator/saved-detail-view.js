/** The saved-workflow detail page: its metadata and highlighted script. */
import { asText } from "./as-text.js";
import { renderCodeLines } from "./rich-text.js";
import { scrollableBody } from "./viewport.js";
export function renderSavedDetail(state, model, width, theme, viewportRows, markdownTheme, renderCache) {
    const dim = (t) => theme.fg("dim", t);
    const saved = model.saved();
    const w = state.activeSaved(model.visible(state.filter), saved);
    const lines = [theme.bold(w ? w.name : "saved workflow")];
    if (!w)
        return lines;
    const body = [];
    if (w.description)
        body.push(dim("Description: ") + asText(w.description));
    body.push(dim("Location: ") + (w.location === "user" ? "user (~/.pi)" : "project (.pi)"));
    body.push(dim("Saved at: ") + asText(w.savedAt));
    if (w.parameters)
        body.push(dim("Parameters: ") + JSON.stringify(w.parameters));
    body.push("", theme.fg("accent", theme.bold("Script:")));
    // Corrupt saved-workflow JSON can carry a non-string script.
    body.push(...renderCodeLines(asText(w.script), "javascript", width, markdownTheme, renderCache));
    lines.push(...scrollableBody(state, body, viewportRows, dim));
    return lines;
}
