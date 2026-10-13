/** The run view: a two-line run header over the Phases | agents panes, collapsing to one pane on narrow terminals. */
import { truncateToWidth, visibleWidth } from "@earendil-works/pi-tui";
import { fmtCompact, fmtTokenSegment } from "../display.js";
import { BX, renderTwoPaneFrame, topTitleSegment } from "./box-frame.js";
import { ELLIPSIS, pluralize } from "./format.js";
import { computeLeftWidth, computeModelColStart, LW_MIN, leftPhaseRow, moreRow, RW_MIN, rightAgentRow, } from "./pane-rows.js";
import { listPaneRows } from "./viewport.js";
/** Lines for the "phases" and "agents" views; the cursor sits in the left or right pane respectively. */
export function renderRunView(state, model, runId, width, theme, viewportRows) {
    const phases = model.phases(runId);
    state.clamp(state.kind === "agents" && state.phase !== undefined ? model.agents(runId, state.phase).length : phases.length);
    // Body cap: total height minus 2 header + 2 frame rules + blank + footer.
    const bodyCap = Math.max(1, viewportRows - 2 /*header*/ - 2 /*rules*/ - 2 /*blank+footer*/);
    return [
        ...runHeader(model, runId, phases, width, theme),
        ...renderPhasesAgents(state, model, runId, phases, width, theme, bodyCap),
    ];
}
/**
 * Two-line header above the Phases | agents frame:
 *   line 0: <name>                          (ACCENT_BOLD)
 *   line 1: <status>            <done>/<total> agent[s] · <tokens>   (DIM)
 * Right segment is built first and never truncated; the left segment is
 * truncated to the remaining width with an ellipsis.
 */
function runHeader(model, runId, phases, width, theme) {
    const name = model.runName(runId);
    const status = model.runStatus(runId);
    let done = 0;
    let total = 0;
    let fresh = 0;
    let cacheRead = 0;
    let estimated = false;
    for (const p of phases) {
        done += p.done;
        total += p.total;
        fresh += p.fresh;
        cacheRead += p.cacheRead;
        if (p.estimated)
            estimated = true;
    }
    // Line 0 — name (accent + bold), truncated to width if needed.
    const nameText = truncateToWidth(name, width, ELLIPSIS, false);
    const line0 = theme.fg("accent", theme.bold(nameText));
    // Line 1 — left status, right summary.
    const headerSegment = fmtTokenSegment({ fresh, cacheRead, estimated }, fmtCompact);
    const rightRaw = `${done}/${total} ${pluralize("agent", total)}${headerSegment ? ` · ${headerSegment}` : ""}`;
    const rightW = visibleWidth(rightRaw);
    const gap = 2;
    let line1;
    if (rightW >= width) {
        // No room for left content: right-align (truncate from the right as last resort).
        line1 = theme.fg("dim", truncateToWidth(rightRaw, width, ELLIPSIS, false));
    }
    else {
        const availL = width - rightW - gap;
        const leftText = availL > 0 ? truncateToWidth(status, availL, ELLIPSIS, false) : "";
        const leftW = visibleWidth(leftText);
        const fill = " ".repeat(Math.max(gap, width - leftW - rightW));
        line1 = theme.fg("dim", leftText) + fill + theme.fg("dim", rightRaw);
    }
    return [line0, line1];
}
function agentsTitle(phase, agentCount) {
    return `${phase ? phase.title : "(none)"} · ${agentCount} ${pluralize("agent", agentCount)}`;
}
/** The combined Phases | agents frame. The selected phase drives the right pane. */
function renderPhasesAgents(state, model, runId, phases, width, theme, bodyCap) {
    // Group agents once per frame: every visible phase row needs its agents for
    // the status colour, and the selected phase's agents fill the right pane.
    const agentsByPhase = model.agentsByPhase(runId);
    const agentsOf = (phase) => agentsByPhase.get(phase) ?? [];
    // In "phases" view the cursor selects the phase; in "agents" view it is the drilled-in phase.
    const inAgents = state.kind === "agents";
    let selPhaseIdx = inAgents ? phases.findIndex((p) => p.phase === state.phase) : state.cursor;
    if (selPhaseIdx < 0)
        selPhaseIdx = 0;
    const selPhase = phases[selPhaseIdx];
    const agents = selPhase ? agentsOf(selPhase.phase) : [];
    if (width < LW_MIN + RW_MIN - 1) {
        return renderSinglePane(state, phases, selPhaseIdx, agents, agentsOf, width, theme, bodyCap, inAgents);
    }
    const leftW = computeLeftWidth(phases, width);
    const rightW = width - leftW + 1; // shared divider overlaps 1 cell
    const leftInner = leftW - 2;
    const rightInner = rightW - 2;
    // Each pane scrolls on its own so its active item stays visible; the frame pads the shorter one.
    const left = listPaneRows(phases.length, selPhaseIdx, bodyCap, (i) => leftPhaseRow(phases[i], i, !inAgents && i === state.cursor, agentsOf(phases[i].phase), leftInner, theme), moreRow(leftInner, theme));
    const modelColStart = computeModelColStart(agents, rightInner);
    const right = agents.length === 0
        ? [truncateToWidth(theme.fg("dim", "no agents"), rightInner, "", true)]
        : listPaneRows(agents.length, inAgents ? state.cursor : 0, bodyCap, (i) => rightAgentRow(agents[i], inAgents && i === state.cursor, modelColStart, rightInner, theme), moreRow(rightInner, theme));
    return renderTwoPaneFrame({
        width,
        bodyRows: Math.max(1, left.length, right.length),
        left,
        right,
        leftTitle: "Phases",
        rightTitle: agentsTitle(selPhase, agents.length),
        leftW,
        theme,
    });
}
/** Narrow terminals show only the active pane, full width. */
function renderSinglePane(state, phases, selPhaseIdx, agents, agentsOf, width, theme, bodyCap, inAgents) {
    const innerW = Math.max(1, width - 2);
    const bc = (s) => theme.fg("muted", s);
    let title;
    let rows;
    if (inAgents) {
        title = agentsTitle(phases[selPhaseIdx], agents.length);
        const modelColStart = computeModelColStart(agents, innerW);
        rows = listPaneRows(agents.length, state.cursor, bodyCap, (i) => rightAgentRow(agents[i], i === state.cursor, modelColStart, innerW, theme), moreRow(innerW, theme));
    }
    else {
        title = "Phases";
        rows = listPaneRows(phases.length, state.cursor, bodyCap, (i) => leftPhaseRow(phases[i], i, i === state.cursor, agentsOf(phases[i].phase), innerW, theme), moreRow(innerW, theme));
    }
    if (rows.length === 0)
        rows.push(" ".repeat(innerW));
    return [
        bc(BX.tl) + topTitleSegment(title, innerW, false, theme) + bc(BX.tr),
        ...rows.map((row) => bc(BX.v) + row + bc(BX.v)),
        bc(BX.bl) + bc(BX.h.repeat(innerW)) + bc(BX.br),
    ];
}
