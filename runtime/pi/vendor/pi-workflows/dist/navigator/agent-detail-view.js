/** The agent detail page: a compact summary by default and a scrollable full pager on demand. */
import { asText } from "./as-text.js";
import { shortModel } from "./format.js";
import { renderHistoryEntryLines } from "./history-view.js";
import { renderMarkdownLines, renderResultLines } from "./rich-text.js";
import { compactBody, scrollableBody } from "./viewport.js";
function statusLines(a, dim) {
    const lines = [dim("Status: ") + asText(a.status ?? "")];
    if (a.model)
        lines.push(dim("Model: ") + (shortModel(a.model) ?? ""));
    if (a.error)
        lines.push(dim("Error: ") + asText(a.error));
    if (a.errorCode) {
        lines.push(`${dim("Error code: ")}${asText(a.errorCode)}${a.recoverable ? " (recoverable)" : ""}`);
    }
    return lines;
}
export function renderAgentDetail(state, model, runId, agentId, width, theme, viewportRows, markdownTheme, renderCache) {
    const a = model.agentDetail(runId, agentId);
    const lines = [theme.bold(a ? asText(a.label) : "agent")];
    if (!a)
        return lines;
    const dim = (t) => theme.fg("dim", t);
    const heading = (t) => theme.fg("accent", theme.bold(t));
    const markdown = (text) => renderMarkdownLines(text, width, markdownTheme, renderCache);
    const history = (i) => renderHistoryEntryLines(a.history ?? [], i, width, markdownTheme, dim, renderCache);
    // Every dynamic field goes through asText: even a live run can carry a
    // non-string prompt, since agent(42) in a model-written script is never
    // type checked, and persisted records can be corrupt.
    const body = [];
    if (state.pagerOpen) {
        body.push(...statusLines(a, dim));
        body.push("", heading("Prompt:"), ...markdown(asText(a.prompt ?? "")));
        body.push("", heading("Result:"));
        body.push(...renderResultLines(a.result, a.resultPreview, width, markdownTheme, renderCache));
        if (Array.isArray(a.history) && a.history.length) {
            body.push("", heading("History:"));
            for (let i = 0; i < a.history.length; i++)
                body.push(...history(i));
        }
        lines.push(...scrollableBody(state, body, viewportRows, dim));
    }
    else if (a.status === "done") {
        // Completed agents default to their final output; prompt and history are
        // one keypress away in the full pager.
        body.push(heading("Result:"));
        body.push(...renderResultLines(a.result, a.resultPreview, width, markdownTheme, renderCache));
        lines.push(...compactBody(body, viewportRows, dim));
    }
    else {
        // Active and failed agents default to their context plus the latest two events.
        body.push(...statusLines(a, dim));
        body.push("", heading("Prompt:"));
        const promptLines = markdown(asText(a.prompt ?? ""));
        body.push(...promptLines.slice(0, 5));
        if (promptLines.length > 5)
            body.push(dim("  … prompt continues in pager"));
        body.push("", heading("Recent activity:"));
        if (a.history?.length) {
            for (let i = Math.max(0, a.history.length - 2); i < a.history.length; i++) {
                const eventLines = history(i);
                body.push(...eventLines.slice(0, 4));
                if (eventLines.length > 4)
                    body.push(dim("  … event continues in pager"));
            }
        }
        else {
            body.push(dim("  Waiting for the first agent event…"));
        }
        lines.push(...compactBody(body, viewportRows, dim));
    }
    return lines;
}
