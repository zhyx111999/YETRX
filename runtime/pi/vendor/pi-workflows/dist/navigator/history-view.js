/** Renders one entry of an agent's session history: messages, tool calls, written source, and edit diffs. */
import { getLanguageFromPath } from "@earendil-works/pi-coding-agent";
import { asText } from "./as-text.js";
import { renderCodeLines, renderDiffLines, renderMarkdownLines } from "./rich-text.js";
function historyLabel(entry) {
    if (entry.kind === "toolCall")
        return entry.toolName ? `assistant tool ${asText(entry.toolName)}` : "assistant tool";
    if (entry.role === "tool")
        return entry.toolName ? `tool ${asText(entry.toolName)}` : "tool";
    if (entry.kind === "error")
        return `${asText(entry.role)} error`;
    return asText(entry.role);
}
function editCallPath(entry) {
    if (entry.kind !== "toolCall" || entry.toolName !== "edit")
        return undefined;
    return typeof entry.path === "string" ? entry.path : undefined;
}
function writeCallSource(entry) {
    if (entry.kind !== "toolCall" || entry.toolName !== "write" || typeof entry.path !== "string")
        return undefined;
    return { path: entry.path, content: asText(entry.text) };
}
/** The call that produced a tool result. Matching on Pi's tool call id keeps
 * parallel calls apart; a result whose call is not in the history has none. */
function resultCall(history, result) {
    if (typeof result.toolCallId !== "string")
        return undefined;
    return history.find((entry) => entry?.kind === "toolCall" && entry.toolCallId === result.toolCallId);
}
/** Source language for history that Pi stores as raw tool text rather than
 * Markdown. Tool-call arguments are JSON; written source and read results take
 * their language from their own call's path. */
function historyEntryLanguage(history, entry) {
    if (entry.kind === "toolCall") {
        const write = writeCallSource(entry);
        return write ? (getLanguageFromPath(write.path) ?? "text") : "json";
    }
    if (entry.kind !== "toolResult" || entry.toolName !== "read")
        return undefined;
    const path = resultCall(history, entry)?.path;
    return typeof path === "string" ? getLanguageFromPath(path) : undefined;
}
export function renderHistoryEntryLines(history, index, width, markdownTheme, dim, renderCache) {
    const entry = history[index];
    // Corrupt persisted histories can hold null or primitive elements.
    if (!entry || typeof entry !== "object")
        return [];
    const write = writeCallSource(entry);
    const editPath = editCallPath(entry);
    const path = write?.path ?? editPath;
    const header = dim(`${historyLabel(entry)}:${path ? ` ${path}` : ""}`);
    // The edit result carries the same display-oriented diff used by Pi's built-in
    // edit renderer. Render it with Pi's native colors, line numbers, and
    // intra-line highlighting instead of showing the raw replacement JSON.
    if (entry.kind === "toolResult" && entry.toolName === "edit" && typeof entry.diff === "string") {
        return [header, ...renderDiffLines(entry.diff, width, renderCache)];
    }
    if (editPath)
        return [header];
    const language = historyEntryLanguage(history, entry);
    const text = write?.content ?? asText(entry.text);
    return [
        header,
        ...(language
            ? renderCodeLines(text, language, width, markdownTheme, renderCache)
            : renderMarkdownLines(text, width, markdownTheme, renderCache)),
    ];
}
