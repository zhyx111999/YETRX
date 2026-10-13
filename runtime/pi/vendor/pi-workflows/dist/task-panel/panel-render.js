/** Renders the rows of the live workflows panel, compact or detailed with a rolling token/s rate. */
import { truncateToWidth, visibleWidth } from "@earendil-works/pi-tui";
import { aggregateAgentUsage, fmtCompact, fmtCost, fmtTokenSegment, shorten, statusIcon, tokenFigures, } from "../display.js";
import { asText } from "../navigator/as-text.js";
import { shortModel } from "../navigator/format.js";
import { groupAgentsByPhase, phaseTitle } from "../phase-groups.js";
import { runSummary } from "../run-record-store.js";
function isActive(run) {
    return run.status === "running" || run.status === "paused";
}
/**
 * Split the run list into what the panel shows: active runs, finished runs still
 * waiting for result delivery, and the count of finished runs kept only in history.
 */
function panelRuns(manager) {
    const all = manager.listRuns();
    return {
        active: all.filter(isActive),
        pending: all.filter((r) => !isActive(r) && r.pendingDelivery),
        finished: all.filter((r) => !isActive(r) && !r.pendingDelivery).length,
    };
}
function panelHeader(theme, active, pending) {
    return active ? theme.bold(`Workflows running (${active}):`) : theme.bold(`Workflows pending delivery (${pending}):`);
}
/** Finished runs leave the live panel but stay in the navigator; say so, so they don't seem to vanish. */
function navigatorHint(theme, finished) {
    return theme.fg("dim", finished > 0
        ? `  /workflows — open navigator (${finished} finished kept in history)`
        : "  /workflows — open navigator");
}
function fitLine(line, width) {
    if (typeof width !== "number" || !Number.isFinite(width))
        return line;
    const maxWidth = Math.max(0, Math.floor(width));
    if (visibleWidth(line) <= maxWidth)
        return line;
    return truncateToWidth(line, maxWidth);
}
/** Compact panel: one line per active or delivery-pending run. */
export function renderPanel(manager, theme, width) {
    const { active, pending, finished } = panelRuns(manager);
    if (!active.length && !pending.length)
        return [];
    const rows = active.map((r) => {
        const live = manager.getRun(r.runId);
        const summary = runSummary(r);
        const agents = live?.snapshot.agents;
        const done = agents ? agents.filter((a) => a.status === "done").length : summary.done;
        const icon = r.status === "paused" ? "⏸" : "◆";
        const current = live?.snapshot.currentPhase;
        const phase = current !== undefined ? ` · ${phaseTitle(current)}` : "";
        return `  ${icon} ${r.workflowName}  ${done}/${agents?.length ?? summary.total} agents${phase}`;
    });
    const pendingRows = pending.map((r) => `  ⏳ ${r.workflowName}  ${r.status}, result delivery pending`);
    return [
        panelHeader(theme, active.length, pending.length),
        ...rows,
        ...pendingRows,
        navigatorHint(theme, finished),
    ].map((line) => fitLine(line, width));
}
// ─── Detailed mode: live token rate ────────────────────────────────────────────
/** Rolling window for the token/s rate. Older samples age out so a stall decays to 0. */
const RATE_WINDOW_MS = 10_000;
/** Per-run (timestamp, cumulative total) samples, keyed by the persisted runId so
 *  the rolling rate survives pause→resume. Cleared when a run ends. */
const tokenSamples = new Map();
/** Record a token-total sample for `runId` at time `now` (ms). */
export function sampleTokens(runId, total, now, estimated = false) {
    const samples = tokenSamples.get(runId) ?? [];
    const last = samples[samples.length - 1];
    // Collapse repeat renders within the same instant (e.g. width recalcs).
    if (last && last.ts === now && last.total === total) {
        last.estimated = estimated;
        return;
    }
    samples.push({ ts: now, total, estimated });
    // Drop samples beyond the rolling window, always keeping ≥2 so a rate is computable.
    while (samples.length > 2 && now - samples[0].ts > RATE_WINDOW_MS)
        samples.shift();
    tokenSamples.set(runId, samples);
}
/** Tokens/second over the rolling window; 0 when too few samples or totals plateau. */
export function tokensPerSecond(runId) {
    const samples = tokenSamples.get(runId);
    if (!samples || samples.length < 2)
        return 0;
    const oldest = samples[0];
    const newest = samples[samples.length - 1];
    const elapsedMs = newest.ts - oldest.ts;
    if (elapsedMs <= 0)
        return 0;
    const delta = newest.total - oldest.total;
    if (delta <= 0)
        return 0;
    return (delta / elapsedMs) * 1000;
}
/** Whether the two samples that define the current positive token rate include a heuristic estimate. */
function tokenRateIsEstimated(runId) {
    const samples = tokenSamples.get(runId);
    if (!samples || samples.length < 2)
        return false;
    const oldest = samples[0];
    const newest = samples[samples.length - 1];
    return oldest.estimated || newest.estimated;
}
/** Forget a run's samples (call when it finishes) so the map can't grow unbounded. */
export function clearTokenSamples(runId) {
    tokenSamples.delete(runId);
}
/** Normalize the configured per-phase agent cap to a sane integer (default 8). */
export function clampMaxAgents(value) {
    if (typeof value !== "number" || !Number.isFinite(value) || value < 1)
        return 8;
    return Math.min(1000, Math.floor(value));
}
/** Per-phase + per-agent body for one run in detailed mode, in the shared phase order (see groupAgentsByPhase). */
function renderRunBody(snap, agents, maxAgents, theme) {
    const dim = (t) => theme.fg("dim", t);
    const lines = [];
    for (const { phase, title, agents: phaseAgents, counts } of groupAgentsByPhase({ phases: snap.phases, agents })) {
        if (!phaseAgents.length)
            continue;
        const complete = counts.done + counts.error + counts.skipped === phaseAgents.length;
        const marker = counts.running > 0 || (!complete && snap.currentPhase === phase) ? "▶" : complete ? "✓" : " ";
        const phaseMeta = [
            `${counts.done}/${phaseAgents.length} agents`,
            counts.running ? `${counts.running} running` : "",
            counts.error ? `${counts.error} errors` : "",
            fmtTokenSegment(aggregateAgentUsage(phaseAgents), fmtCompact),
        ]
            .filter(Boolean)
            .join(" · ");
        lines.push(theme.fg("accent", `  ${marker} ${title}`) + dim(`  ${phaseMeta}`));
        const visible = phaseAgents.slice(-maxAgents);
        for (const a of visible) {
            const segment = fmtTokenSegment(tokenFigures(a.tokenUsage, a.tokens), fmtCompact);
            const tok = segment ? dim(` ${segment}`) : "";
            const mdl = shortModel(a.model);
            const model = mdl ? dim(` · ${mdl}`) : "";
            lines.push(`    [${a.id}] ${statusIcon(a.status)} ${shorten(asText(a.label), 40)}${tok}${model}`);
        }
        if (phaseAgents.length > visible.length) {
            lines.push(dim(`    … ${phaseAgents.length - visible.length} earlier agents`));
        }
    }
    return lines;
}
/**
 * Detailed variant of {@link renderPanel}: per-run header with aggregate tokens,
 * cost, and a live token/s rate, followed by per-phase progress and per-agent rows
 * (capped at `maxAgents` per phase). `now` is injected for testability.
 */
export function renderPanelDetailed(manager, theme, width, maxAgents, now) {
    const { active, pending, finished } = panelRuns(manager);
    if (!active.length && !pending.length)
        return [];
    const dim = (t) => theme.fg("dim", t);
    const out = [panelHeader(theme, active.length, pending.length)];
    for (const r of active) {
        const live = manager.getRun(r.runId);
        const snap = live?.snapshot;
        const summary = runSummary(r);
        const agents = (snap?.agents ?? []);
        const done = snap ? agents.filter((a) => a.status === "done").length : summary.done;
        const icon = r.status === "paused" ? "⏸" : "◆";
        const usage = snap?.tokenUsage ?? r.tokenUsage;
        // Per-agent figures stream while agents run, so aggregate them for the same
        // fresh+cacheRead sum the header displays. A flat rate now indicates a real
        // lull rather than merely waiting for a long-running agent to return. Paused
        // runs do not accrue tokens, so their rate is suppressed.
        const runUsage = snap ? aggregateAgentUsage(agents) : summary.usage;
        sampleTokens(r.runId, runUsage.fresh + runUsage.cacheRead, now, runUsage.estimated);
        const rate = r.status === "running" ? tokensPerSecond(r.runId) : 0;
        const meta = [
            `${done}/${snap ? agents.length : summary.total} agents`,
            snap?.currentPhase !== undefined ? phaseTitle(snap.currentPhase) : "",
            fmtTokenSegment(runUsage, fmtCompact),
            // Cost is known only once the run finalizes its usage.
            usage?.cost ? fmtCost(usage.cost) : "",
            rate > 0 ? `${tokenRateIsEstimated(r.runId) ? "~" : ""}${Math.round(rate)} tok/s` : "",
        ]
            .filter(Boolean)
            .join(" · ");
        out.push(`  ${icon} ${theme.bold(r.workflowName)}  ${dim(meta)}`);
        if (snap)
            out.push(...renderRunBody(snap, agents, maxAgents, theme));
    }
    for (const r of pending) {
        out.push(`  ⏳ ${theme.bold(r.workflowName)}  ${dim(`${r.status}, result delivery pending`)}`);
    }
    out.push(navigatorHint(theme, finished));
    return out.map((line) => fitLine(line, width));
}
