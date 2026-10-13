/**
 * Model-facing reports of workflow runs: one-line run summaries, per-agent
 * status, the waiting checkpoint, and the tail of one subagent's session.
 */

import type { AgentHistoryEntry } from "./agent-history.js";
import { aggregateAgentUsage, tokenFigures, type WorkflowAgentSnapshot, type WorkflowSnapshot } from "./display.js";
import type { PauseReason, PersistedAgentState, PersistedRunState, RunStatus } from "./run-persistence.js";
import { runSummary } from "./run-record-store.js";

/** Characters of one subagent session returned by `read`. */
export const AGENT_TAIL_CHARS = 8000;
/** Agents listed by `status` before the rest are counted. */
const STATUS_AGENT_LIMIT = 100;
/** Characters of a waiting checkpoint's payload shown by `status`. */
const CHECKPOINT_PAYLOAD_CHARS = 500;

export interface RunSummaryDetails {
  runId: string;
  workflowName: string;
  status: RunStatus;
  /** Why a paused run is paused, when it recorded a reason. */
  pauseReason?: PauseReason;
  phase: string | null;
  checkpoint: Pick<NonNullable<PersistedRunState["checkpoint"]>, "checkpointId" | "kind" | "status"> | null;
  counts: { total: number; done: number; running: number; queued: number; error: number; skipped: number };
  activeLabels: string[];
  tokenTotal: number;
  /** True when tokenTotal includes character-heuristic estimates. */
  tokenTotalEstimated: boolean;
}

/** Summarize a run, preferring the live snapshot over the persisted record when one exists. */
export function summarizeRun(run: PersistedRunState, live?: WorkflowSnapshot | null): RunSummaryDetails {
  const summary = runSummary(run);
  const agents = live?.agents ?? [];
  const counts = live
    ? countAgents(agents)
    : {
        total: summary.total,
        done: summary.done,
        running: summary.running,
        queued: summary.queued,
        error: summary.error,
        skipped: summary.skipped,
      };
  const liveUsage = tokenFigures(live?.tokenUsage);
  const persistedUsage = tokenFigures(run.tokenUsage);
  const agentUsage = live ? aggregateAgentUsage(agents) : summary.usage;
  return {
    runId: run.runId,
    workflowName: live?.name ?? run.workflowName,
    status: run.status,
    ...(run.status === "paused" && run.pauseReason ? { pauseReason: run.pauseReason } : {}),
    phase: live?.currentPhase ?? run.currentPhase ?? null,
    checkpoint: summary.checkpoint,
    counts,
    activeLabels: live
      ? agents.filter((agent) => agent.status === "running").map((agent) => agent.label)
      : summary.activeLabels,
    // A live aggregate can lag its agents, so the largest available figure wins.
    tokenTotal: Math.max(
      liveUsage.fresh + liveUsage.cacheRead,
      persistedUsage.fresh + persistedUsage.cacheRead,
      agentUsage.fresh + agentUsage.cacheRead,
    ),
    tokenTotalEstimated: liveUsage.estimated || persistedUsage.estimated || agentUsage.estimated,
  };
}

function countAgents(agents: Array<Pick<WorkflowAgentSnapshot, "status">>): RunSummaryDetails["counts"] {
  return {
    total: agents.length,
    done: agents.filter((agent) => agent.status === "done").length,
    running: agents.filter((agent) => agent.status === "running").length,
    queued: agents.filter((agent) => agent.status === "queued").length,
    error: agents.filter((agent) => agent.status === "error").length,
    skipped: agents.filter((agent) => agent.status === "skipped").length,
  };
}

/** One key=value line per run, stable for the model to scan. */
export function formatRunSummary(run: RunSummaryDetails): string {
  const active = run.activeLabels.join(",") || "-";
  const checkpoint = JSON.stringify(run.checkpoint ?? null);
  const pause = run.pauseReason ? ` pause=${run.pauseReason}` : "";
  return `runId=${run.runId} name=${quote(run.workflowName)} status=${run.status}${pause} phase=${quote(run.phase ?? "-")} checkpoint=${checkpoint} total=${run.counts.total} done=${run.counts.done} running=${run.counts.running} queued=${run.counts.queued} error=${run.counts.error} skipped=${run.counts.skipped} active=${quote(active)} tokens=${run.tokenTotalEstimated ? "~" : ""}${run.tokenTotal}`;
}

export interface AgentStatusDetails {
  label: string;
  status: string;
  phase?: string;
}

/** Every agent's label and status, from the live snapshot when the run is in memory. */
export function agentStatuses(run: PersistedRunState, live?: WorkflowSnapshot | null): AgentStatusDetails[] {
  const agents: Array<WorkflowAgentSnapshot | PersistedAgentState> = live?.agents.length
    ? live.agents
    : Array.isArray(run.agents)
      ? run.agents
      : [];
  return agents
    .filter((agent) => agent && typeof agent === "object")
    .map(({ label, status, phase }) => (phase ? { label, status, phase } : { label, status }));
}

export function formatAgentStatuses(agents: readonly AgentStatusDetails[]): string {
  const lines = agents
    .slice(0, STATUS_AGENT_LIMIT)
    .map((agent) => `- ${agent.label}: ${agent.status}${agent.phase ? ` (${agent.phase})` : ""}`);
  if (agents.length > STATUS_AGENT_LIMIT) lines.push(`- … ${agents.length - STATUS_AGENT_LIMIT} more`);
  return lines.join("\n");
}

/** The checkpoint a run is waiting at, with a bounded payload preview; null when it waits for none. */
export function waitingCheckpoint(
  run: PersistedRunState,
): { checkpointId: string; kind: string; payload: string } | null {
  const checkpoint = run.checkpoint;
  if (checkpoint?.status !== "waiting") return null;
  const payload = JSON.stringify(checkpoint.payload) ?? "null";
  return {
    checkpointId: checkpoint.checkpointId,
    kind: checkpoint.kind,
    payload: payload.length <= CHECKPOINT_PAYLOAD_CHARS ? payload : `${payload.slice(0, CHECKPOINT_PAYLOAD_CHARS)}…`,
  };
}

interface TailAgent {
  id: number;
  label: string;
  phase?: string;
  status: string;
  history?: AgentHistoryEntry[];
  result?: unknown;
  error?: string;
  endedAt?: string;
}

export function asTailAgent(agent: WorkflowAgentSnapshot | PersistedAgentState): TailAgent {
  return {
    id: agent.id,
    label: agent.label,
    phase: agent.phase,
    status: agent.status,
    history: agent.history,
    result: agent.result,
    error: agent.error,
    endedAt: "endedAt" in agent ? agent.endedAt : undefined,
  };
}

function latestStamp(agent: TailAgent): number {
  let stamp = 0;
  for (const entry of agent.history ?? []) {
    if (typeof entry.timestamp === "number" && entry.timestamp > stamp) stamp = entry.timestamp;
  }
  if (agent.endedAt) {
    const ended = Date.parse(agent.endedAt);
    if (Number.isFinite(ended) && ended > stamp) stamp = ended;
  }
  return stamp;
}

/** The agent with this label, or the most recently updated one; the later row wins a tie. */
export function selectTailAgent(agents: readonly TailAgent[], label?: string): TailAgent | undefined {
  const pool = label ? agents.filter((agent) => agent.label === label) : [...agents];
  if (!pool.length) return undefined;
  return pool.reduce((best, agent) => {
    const bestStamp = latestStamp(best);
    const stamp = latestStamp(agent);
    if (stamp > bestStamp || (stamp === bestStamp && agent.id > best.id)) return agent;
    return best;
  });
}

/** The last `maxChars` of one agent's session: header, history, then result or error. */
export function renderAgentTail(agent: TailAgent, maxChars = AGENT_TAIL_CHARS): string {
  const lines = [`label: ${agent.label}`, `status: ${agent.status}`];
  if (agent.phase) lines.push(`phase: ${agent.phase}`);
  for (const entry of agent.history ?? []) {
    const who = entry.toolName ? `${entry.role}:${entry.toolName}` : entry.role;
    lines.push(`[${who}] ${entry.text}`);
  }
  if (agent.result !== undefined) {
    lines.push(`[result] ${typeof agent.result === "string" ? agent.result : JSON.stringify(agent.result)}`);
  }
  if (agent.error) lines.push(`[error] ${agent.error}`);
  const text = lines.join("\n");
  return text.length <= maxChars ? text : text.slice(-maxChars);
}

function quote(value: string): string {
  return JSON.stringify(value);
}
