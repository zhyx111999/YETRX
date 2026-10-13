import type { AgentHistoryEntry } from "./agent-history.js";
import type { AgentUsage } from "./agent-usage.js";
import type { WorkflowErrorCode } from "./errors.js";
import { asText } from "./navigator/as-text.js";
import { groupAgentsByPhase, type PhaseGroup } from "./phase-groups.js";
import type { RunStatus } from "./run-persistence.js";
import type { WorkflowMeta } from "./workflow.js";

export type WorkflowAgentStatus = "queued" | "running" | "done" | "error" | "skipped";

export interface WorkflowAgentSnapshot {
  id: number;
  /** Runtime call identity (`${runId}:${callIndex}`), used to rehydrate journaled results. */
  callId?: string;
  label: string;
  phase?: string;
  prompt: string;
  status: WorkflowAgentStatus;
  /** Full agent result, retained for the interactive detail pager. */
  result?: unknown;
  resultPreview?: string;
  error?: string;
  errorCode?: WorkflowErrorCode;
  recoverable?: boolean;
  history?: AgentHistoryEntry[];
  /** Tokens used by this agent (a scalar estimate when the provider reports no usage). */
  tokens?: number;
  /** Per-agent token usage breakdown (fresh input+output vs cached), when known. */
  tokenUsage?: AgentUsage;
  /** The model this agent ran on (provider/id), when known. */
  model?: string;
  /** Child SessionManager identity, captured before the first prompt. */
  sessionId?: string;
  /** Child session file, absent for in-memory child sessions. */
  sessionFile?: string;
  /**
   * Earlier executions of this call, oldest first. A call has one row that
   * shows its latest execution; a resume that re-runs the call keeps the
   * superseded one here, so counts stay per call while its spend still counts.
   */
  priorExecutions?: PriorAgentExecution[];
}

/** How one superseded execution of a call ended. */
export interface PriorAgentExecution {
  label: string;
  status: WorkflowAgentStatus;
  resultPreview?: string;
  error?: string;
  errorCode?: WorkflowErrorCode;
  model?: string;
  sessionId?: string;
  sessionFile?: string;
  tokens?: number;
  tokenUsage?: AgentUsage;
  startedAt?: string;
  endedAt?: string;
}

export interface WorkflowSnapshot {
  name: string;
  description?: string;
  phases: string[];
  currentPhase?: string;
  logs: string[];
  agents: WorkflowAgentSnapshot[];
  agentCount: number;
  runningCount: number;
  doneCount: number;
  errorCount: number;
  durationMs?: number;
  result?: unknown;
  tokenUsage?: AgentUsage;
  runId?: string;
}

/**
 * Displayable fresh/cached figures from a usage breakdown and/or a scalar
 * estimate. The token pipeline has two sources that don't always agree: the
 * provider-reported breakdown (input/output/cacheRead/cacheWrite) and a scalar
 * estimate (`total` at run level, `tokens` per agent) that keeps accruing even
 * when the provider reports nothing. Two rules:
 * - `fresh` counts input+output+cacheWrite: cache writes are first-time
 *   ingestion billed at full (or premium) price, so hiding them would
 *   under-report real spend; only cacheRead is the cheap reuse shown apart.
 * - `fresh` is never less than what the estimate can account for after
 *   removing cache reads, so estimate-only providers, cost-only providers
 *   (billed but zero token counts), and mixed runs keep their estimated
 *   count instead of a false "0 tok".
 */
export function tokenFigures(
  usage: Partial<AgentUsage> | undefined,
  scalarTokens?: number,
): { fresh: number; cacheRead: number; estimated: boolean } {
  const cacheRead = usage?.cacheRead ?? 0;
  const reported = (usage?.input ?? 0) + (usage?.output ?? 0) + (usage?.cacheWrite ?? 0);
  const estimate = Math.max(scalarTokens ?? 0, usage?.total ?? 0);
  return { fresh: Math.max(reported, estimate - cacheRead), cacheRead, estimated: usage?.estimated === true };
}

/**
 * Sum a set of agents into fresh vs cacheRead totals, via {@link tokenFigures}.
 * An agent's prior executions were paid for too, so they count with it.
 */
export function aggregateAgentUsage(
  agents: ReadonlyArray<Pick<WorkflowAgentSnapshot, "tokens" | "tokenUsage" | "priorExecutions">>,
): {
  fresh: number;
  cacheRead: number;
  estimated: boolean;
} {
  let fresh = 0;
  let cacheRead = 0;
  let estimated = false;
  for (const agent of agents) {
    // Records are read from disk, so a malformed history must not throw here.
    const prior = Array.isArray(agent.priorExecutions) ? agent.priorExecutions : [];
    for (const execution of [agent, ...prior]) {
      if (!execution || typeof execution !== "object") continue;
      const f = tokenFigures(execution.tokenUsage, execution.tokens);
      fresh += f.fresh;
      cacheRead += f.cacheRead;
      if (f.estimated) estimated = true;
    }
  }
  return { fresh, cacheRead, estimated };
}

/**
 * Format a token count for a display surface: "12.4K tok" on its own, or
 * "89K tok · 3.0M cached" when there were cache reads. The cache segment is shown
 * only when `cacheRead > 0`, so a non-caching provider (or a single-turn agent that
 * never re-reads its cache) reads as a plain "tok" rather than a bare, contextless
 * "fresh". `fmt` adapts the number style per surface (compact in panels, full in
 * the print view).
 */
function fmtTokenCount(fresh: number, cacheRead: number, fmt: (n: number) => string): string {
  const f = fmt(fresh) || "0";
  return cacheRead > 0 ? `${f} tok · ${fmt(cacheRead)} cached` : `${f} tok`;
}

/**
 * Like {@link fmtTokenCount}, but "" when nothing is known yet (both figures 0),
 * so surfaces omit the segment instead of rendering a false "0 tok" — e.g. for a
 * journal-replayed resume or a run whose agents were all skipped. Every surface
 * should use this rather than re-implementing the zero guard. When
 * `figures.estimated` is set the segment is prefixed with `~` so a
 * heuristic-derived total never renders as metered. The marker covers the whole
 * segment (fresh + cached) even when only one component is heuristic —
 * conservative by design.
 */
export function fmtTokenSegment(
  figures: { fresh: number; cacheRead: number; estimated?: boolean },
  fmt: (n: number) => string,
): string {
  if (figures.fresh + figures.cacheRead <= 0) return "";
  const rendered = fmtTokenCount(figures.fresh, figures.cacheRead, fmt);
  // "~" marks character-heuristic figures so an estimate never reads as a
  // metered total.
  return figures.estimated ? `~${rendered}` : rendered;
}

/**
 * "$1.23" from one cent up, four decimals below it, and "<$0.0001" for
 * anything smaller — a real cost never rounds to a zero-looking "$0.00".
 */
export function fmtCost(cost: number): string {
  if (cost > 0 && cost < 0.0001) return "<$0.0001";
  return `$${cost.toFixed(cost >= 0.01 ? 2 : 4)}`;
}

/** Full (non-compact) number style for print/text surfaces: locale-grouped digits. */
// Reuse one formatter across render calls. No locale argument means the
// runtime default, matching toLocaleString() semantics.
const FULL_NUMBER_FORMAT = new Intl.NumberFormat();
export const fmtFull = (n: number): string => FULL_NUMBER_FORMAT.format(n);

/** Compact number style for panels and narrow cells: 842, 35k, 35.7k, 1.3M (a trailing .0 is dropped). */
export function fmtCompact(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return "0";
  if (n < 1000) return String(Math.round(n));
  if (n < 1_000_000) {
    const k = n / 1000;
    return `${k >= 100 ? Math.round(k) : trimZero(k.toFixed(1))}k`;
  }
  return `${trimZero((n / 1_000_000).toFixed(1))}M`;
}

function trimZero(s: string): string {
  return s.endsWith(".0") ? s.slice(0, -2) : s;
}

/** Icon of a run's lifecycle status. Agent rows use {@link statusIcon}. */
const RUN_STATUS_ICON: Record<RunStatus, string> = {
  pending: "·",
  running: "◆",
  paused: "⏸",
  completed: "✓",
  failed: "✗",
  aborted: "⊘",
};

export function runStatusIcon(status: string): string {
  return RUN_STATUS_ICON[status as RunStatus] ?? "?";
}

export function createWorkflowSnapshot(meta: WorkflowMeta): WorkflowSnapshot {
  return {
    name: meta.name,
    description: meta.description,
    phases: meta.phases?.map((phase) => phase.title) ?? [],
    logs: [],
    agents: [],
    agentCount: 0,
    runningCount: 0,
    doneCount: 0,
    errorCount: 0,
  };
}

export function recomputeWorkflowSnapshot(snapshot: WorkflowSnapshot): WorkflowSnapshot {
  const runningCount = snapshot.agents.filter((agent) => agent.status === "running").length;
  const doneCount = snapshot.agents.filter((agent) => agent.status === "done").length;
  const errorCount = snapshot.agents.filter((agent) => agent.status === "error").length;
  return { ...snapshot, agentCount: snapshot.agents.length, runningCount, doneCount, errorCount };
}

export interface EmptyFleetSummary {
  /** True when the run launched at least one agent but every one of them returned no usable result. */
  allEmpty: boolean;
  /** Agents that ran to a terminal state and returned null (recoverable failure exhausted, e.g. AGENT_EMPTY_OUTPUT). */
  emptyCount: number;
  /** Agents that produced a real result. */
  doneCount: number;
  /** Labels of the empty agents, capped for a readable warning line. */
  emptyLabels: string[];
}

/**
 * Detect the "empty fleet" case: a run that spent on at least one agent yet got
 * zero usable results back. `agent()` resolves a recoverable failure (e.g.
 * `AGENT_EMPTY_OUTPUT` after retries are exhausted) to `null` rather than
 * throwing, so an all-null fleet still reports the run as completed — without
 * this check the host can mistake "nothing was produced" for "everything
 * succeeded". Agents still queued/running are not counted; only terminal
 * `error` (null result) and `done` (real result) states decide.
 */
export function emptyFleetSummary(agents: WorkflowAgentSnapshot[], maxLabels = 5): EmptyFleetSummary {
  const terminal = agents.filter((agent) => agent.status === "error" || agent.status === "done");
  const empty = terminal.filter((agent) => agent.status === "error");
  const doneCount = terminal.length - empty.length;
  return {
    allEmpty: terminal.length > 0 && doneCount === 0,
    emptyCount: empty.length,
    doneCount,
    emptyLabels: empty.slice(0, maxLabels).map((agent) => agent.label || `agent #${agent.id}`),
  };
}

/** Minimal theme surface the navigator renders with, so its views work without a real Theme in tests. */
export interface ThemeLike {
  fg(color: string, text: string): string;
  bold(text: string): string;
}

/** The text view lists only each phase's most recent agents, so a large fleet stays readable. */
const TEXT_AGENTS_PER_PHASE = 8;

/** Plain-text progress of a run, for hosts that can only print text (`/workflows status`, the final watch message). */
export function renderWorkflowText(snapshot: WorkflowSnapshot, completed = false): string {
  const state =
    snapshot.errorCount > 0
      ? `, ${snapshot.errorCount} errors`
      : snapshot.runningCount > 0
        ? `, ${snapshot.runningCount} running`
        : "";
  const usage = snapshot.tokenUsage;
  const segment = fmtTokenSegment(tokenFigures(usage), fmtFull);
  const tokenInfo = `${segment ? ` · ${segment}` : ""}${usage?.cost ? ` · ${fmtCost(usage.cost)}` : ""}`;
  const lines = [
    completed ? "Workflow completed" : "Workflow running",
    `◆ Workflow: ${snapshot.name} (${snapshot.doneCount}/${snapshot.agentCount} done${state}${tokenInfo})`,
  ];
  for (const group of groupAgentsByPhase(snapshot)) lines.push(...phaseLines(group, snapshot.currentPhase));
  return lines.join("\n");
}

/** One phase: a marker and tally line, then its most recent agents. */
function phaseLines({ phase, title, agents, counts }: PhaseGroup, currentPhase: string | undefined): string[] {
  const settled = agents.length > 0 && counts.done + counts.error + counts.skipped === agents.length;
  const marker = counts.running > 0 || (!settled && currentPhase === phase) ? "▶" : settled ? "✓" : " ";
  const tally = [
    `${counts.done}/${agents.length}`,
    counts.running ? `${counts.running} running` : "",
    counts.error ? `${counts.error} errors` : "",
    counts.skipped ? `${counts.skipped} skipped` : "",
  ]
    .filter(Boolean)
    .join(" · ");
  const visible = agents.slice(-TEXT_AGENTS_PER_PHASE);
  const lines = [`  ${marker} ${title} ${tally}`];
  for (const agent of visible) {
    lines.push(
      `    [${agent.id}] ${statusIcon(agent.status)} ${shorten(asText(agent.label), 48)}${agentTokenCell(agent)}`,
    );
  }
  if (agents.length > visible.length) lines.push(`    … ${agents.length - visible.length} earlier agents`);
  return lines;
}

/** The bracketed per-agent token cell (" [89 tok · 3,000 cached]"), or "" when nothing is known yet. */
function agentTokenCell(agent: WorkflowAgentSnapshot): string {
  const segment = fmtTokenSegment(tokenFigures(agent.tokenUsage, agent.tokens), fmtFull);
  return segment ? ` [${segment}]` : "";
}

export function statusIcon(status: WorkflowAgentStatus): string {
  switch (status) {
    case "queued":
      return "○";
    case "running":
      return "●";
    case "done":
      return "✓";
    case "error":
      return "✗";
    case "skipped":
      return "-";
  }
}

export function shorten(value: string, max: number): string {
  const text = value.replace(/\s+/g, " ").trim();
  return text.length > max ? `${text.slice(0, max - 1)}…` : text;
}

export function preview(value: unknown, max = 80): string {
  const text = typeof value === "string" ? value : JSON.stringify(value);
  if (!text) return "";
  return text.length > max ? `${text.slice(0, max - 1)}…` : text;
}
