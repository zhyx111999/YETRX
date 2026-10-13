/** Lays out single phase, agent, and overflow-marker rows, and the column widths they share, inside the run view's panes. */
import { truncateToWidth, visibleWidth } from "@earendil-works/pi-tui";
import { fmtCompact, fmtTokenSegment, tokenFigures } from "../display.js";
import { ELLIPSIS, shortModel } from "./format.js";
/** Narrowest usable Phases pane, in cells. */
export const LW_MIN = 14;
/** Narrowest usable agents pane, in cells. */
export const RW_MIN = 24;
const GAP_NM = 2; // min spaces between agent name and model columns
const CARET = "›";
const DOT = "●";
/** Aggregate phase status precedence: ERR > RUN > all-done(OK) > PEND. */
function phaseStatusColor(p, agents) {
    if (agents.some((a) => a.status === "error" || a.status === "failed"))
        return "error";
    if (agents.some((a) => a.status === "running"))
        return "warning";
    if (p.total > 0 && p.done === p.total)
        return "success";
    return "dim";
}
const AGENT_DOT_COLOR = {
    running: "warning",
    queued: "dim",
    pending: "dim",
    paused: "dim",
    done: "success",
    completed: "success",
    error: "error",
    failed: "error",
    skipped: "dim",
    aborted: "dim",
};
/** The Phases box's outer width: fit the longest row, capped so the agents pane keeps most of the width. */
export function computeLeftWidth(phases, width) {
    const titleNeed = visibleWidth("Phases") + 2 /*spaces*/ + 1 /*┌*/ + 1 /*┬*/ + 3 /*min dashes*/;
    let contentMax = 0;
    phases.forEach((p, i) => {
        const idx = String(i + 1);
        const hasAgents = p.total > 0;
        const need = 2 /*marker*/ +
            visibleWidth(idx) +
            1 /*sp*/ +
            visibleWidth(p.title) +
            (hasAgents ? 1 + visibleWidth(`${p.done}/${p.total}`) : 0);
        if (need > contentMax)
            contentMax = need;
    });
    const innerNeed = Math.max(contentMax, titleNeed - 2);
    const lwNatural = innerNeed + 2; // + left │ + shared │
    const lwMax = Math.min(40, Math.floor(width * 0.45));
    return Math.max(LW_MIN, Math.min(lwNatural, Math.max(LW_MIN, lwMax)));
}
/** Build a left-pane phase row (content field, exact width = innerW). */
export function leftPhaseRow(p, i, selected, agents, innerW, theme) {
    const idx = String(i + 1);
    const hasAgents = p.total > 0;
    const progress = hasAgents ? `${p.done}/${p.total}` : "";
    const marker = selected ? `${CARET} ` : "  ";
    // Fixed parts width: marker + idx + space + (space+progress if shown)
    const fixed = 2 + visibleWidth(idx) + 1 + (progress ? 1 + visibleWidth(progress) : 0);
    const nameRoom = Math.max(0, innerW - fixed);
    const name = truncateToWidth(p.title, nameRoom, ELLIPSIS, false);
    const styleMain = (s) => (selected ? theme.fg("accent", theme.bold(s)) : hasAgents ? s : theme.fg("dim", s));
    const progStyle = (s) => selected ? theme.fg("accent", theme.bold(s)) : theme.fg(phaseStatusColor(p, agents), s);
    const caret = selected ? theme.fg("accent", theme.bold(marker)) : marker;
    let row = caret + styleMain(`${idx} ${name}`);
    if (progress)
        row += ` ${progStyle(progress)}`;
    return truncateToWidth(row, innerW, "", true); // pad to exact innerW
}
/** Build a right-pane agent row (content field, exact width = innerW). */
export function rightAgentRow(a, selected, modelColStart, innerW, theme) {
    const dotColor = AGENT_DOT_COLOR[a.status] ?? "dim";
    const stats = fmtTokenSegment(tokenFigures(a.tokenUsage, a.tokens), fmtCompact);
    const model = shortModel(a.model) ?? "";
    // Stable 2-cell marker so columns never shift on selection: "› " | "  ".
    // Layout: <marker:2><dot><sp><name> … <model> … <stats(right-aligned)>.
    const markerW = 2;
    const statsW = visibleWidth(stats);
    const nameStart = markerW + 2; // marker + dot + space
    let modelStart = Math.max(nameStart + visibleWidth(a.label) + GAP_NM, markerW + modelColStart);
    const statsStart = innerW - statsW;
    // Available room for the model block (between modelStart and stats, min 1 gap).
    let modelRoom = statsStart - 1 - modelStart;
    let nameOut = a.label;
    let modelOut = model;
    if (modelRoom < 0) {
        // The model is the first thing dropped when the row is too narrow; the name truncates after it.
        modelOut = "";
        modelStart = nameStart;
        modelRoom = 0;
        const nameRoom = Math.max(0, statsStart - 1 - nameStart);
        nameOut = truncateToWidth(a.label, nameRoom, ELLIPSIS, false);
    }
    else {
        modelOut = truncateToWidth(model, modelRoom, ELLIPSIS, false);
        const nameRoom = Math.max(0, modelStart - GAP_NM - nameStart);
        nameOut = truncateToWidth(a.label, nameRoom, ELLIPSIS, false);
    }
    const marker = selected ? theme.fg("accent", theme.bold(`${CARET} `)) : "  ";
    const dot = theme.fg(dotColor, DOT);
    const nameStyled = selected ? theme.fg("accent", theme.bold(nameOut)) : theme.fg("accent", nameOut);
    const modelStyled = modelOut ? theme.fg("dim", modelOut) : "";
    const statsStyled = theme.fg("dim", stats);
    // Assemble with explicit cell padding (visibleWidth-driven gaps).
    let out = `${marker + dot} ${nameStyled}`;
    const afterName = nameStart + visibleWidth(nameOut);
    if (modelOut) {
        out += " ".repeat(Math.max(0, modelStart - afterName)) + modelStyled;
        const afterModel = modelStart + visibleWidth(modelOut);
        out += " ".repeat(Math.max(0, statsStart - afterModel)) + statsStyled;
    }
    else {
        out += " ".repeat(Math.max(0, statsStart - afterName)) + statsStyled;
    }
    return truncateToWidth(out, innerW, "", true);
}
/** The rows marking more items above or below a pane's window (content field, exact width = innerW). */
export function moreRow(innerW, theme) {
    return (overflow) => truncateToWidth(theme.fg("dim", `  ${overflow === "above" ? "↑" : "↓"} ${ELLIPSIS}`), innerW, "", true);
}
/** Model column start aligned across agent rows, clamped to the field. */
export function computeModelColStart(agents, innerW) {
    let maxName = 0;
    for (const a of agents)
        maxName = Math.max(maxName, visibleWidth(a.label));
    const start = 2 /*dot+sp*/ + maxName + GAP_NM;
    // Keep model column from colliding with the right edge; cap at ~55% of field.
    return Math.min(start, Math.max(2, Math.floor(innerW * 0.55)));
}
