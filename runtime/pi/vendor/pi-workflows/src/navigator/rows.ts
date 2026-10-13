/** Builds the navigator's run, phase, and agent rows from run snapshots. */

import type { AgentUsage } from "../agent-usage.js";
import type { WorkflowAgentSnapshot, WorkflowSnapshot } from "../display.js";
import { aggregateAgentUsage, tokenFigures } from "../display.js";
import { groupAgentsByPhase, type PhaseKey } from "../phase-groups.js";
import type { PersistedRunState } from "../run-persistence.js";
import { runSummary } from "../run-record-store.js";
import { asText } from "./as-text.js";

/** A run's snapshot and lifecycle status, from the live manager or rehydrated from disk. */
export interface RunSnapshot {
  snapshot: WorkflowSnapshot;
  status: string;
}

export interface RunRow {
  runId: string;
  name: string;
  status: string;
  done: number;
  total: number;
  /** Fresh tokens for the whole run (see tokenFigures for the fallback rule). */
  fresh: number;
  /** Cache-read tokens for the whole run. */
  cacheRead: number;
  /** True when the displayed figures include character-heuristic estimates. */
  estimated: boolean;
  cost: number;
}

export interface PhaseRow {
  /** Which phase the row is: the key agents are looked up by. */
  phase: PhaseKey;
  /** What the row prints. */
  title: string;
  done: number;
  total: number;
  /** Fresh tokens summed across the phase's agents. */
  fresh: number;
  /** Cache-read tokens summed across the phase's agents. */
  cacheRead: number;
  /** True when any of the phase's agents reported heuristic-estimated figures. */
  estimated: boolean;
}

export interface AgentRow {
  id: number;
  label: string;
  status: string;
  phase?: string;
  tokens?: number;
  tokenUsage?: AgentUsage;
  model?: string;
}

function toAgentRow(a: WorkflowAgentSnapshot): AgentRow {
  return {
    id: a.id,
    label: asText(a.label),
    status: a.status,
    phase: a.phase != null ? asText(a.phase) : a.phase,
    tokens: a.tokens,
    tokenUsage: a.tokenUsage,
    model: a.model,
  };
}

/** One runs-list row; the live run, when present, wins over the persisted record. */
export function runRow(record: PersistedRunState, live: RunSnapshot | undefined): RunRow {
  // A corrupt record whose agents are not an array must not stop /workflows
  // from opening at all, so guard before iterating.
  const summary = runSummary(record);
  const rawAgents = live?.snapshot.agents ?? [];
  const agents = (Array.isArray(rawAgents) ? rawAgents : []) as WorkflowAgentSnapshot[];
  const usage = live?.snapshot.tokenUsage ?? record.tokenUsage;
  // The run-level aggregate is authoritative but only lands when the run
  // ends; per-agent figures update live. Use whichever accounts for more
  // tokens, so live runs show a count in the list (agreeing with the phase
  // view) and finished runs keep the final aggregate.
  const fromUsage = tokenFigures(usage);
  const fromAgents = live ? aggregateAgentUsage(agents) : summary.usage;
  const figures =
    fromAgents.fresh + fromAgents.cacheRead > fromUsage.fresh + fromUsage.cacheRead ? fromAgents : fromUsage;
  return {
    runId: record.runId,
    name: asText(live?.snapshot.name ?? record.workflowName),
    status: live?.status ?? record.status,
    done: live ? agents.filter((a) => a.status === "done").length : summary.done,
    total: live ? agents.length : summary.total,
    fresh: figures.fresh,
    cacheRead: figures.cacheRead,
    estimated: figures.estimated,
    cost: usage?.cost ?? 0,
  };
}

/** One row per phase, in the shared phase order (see groupAgentsByPhase). */
export function phaseRows(snap: WorkflowSnapshot): PhaseRow[] {
  return groupAgentsByPhase(snap).map(({ phase, title, agents, counts }) => {
    const usage = aggregateAgentUsage(agents);
    return {
      phase,
      title,
      done: counts.done,
      total: agents.length,
      fresh: usage.fresh,
      cacheRead: usage.cacheRead,
      estimated: usage.estimated,
    };
  });
}

export function agentRowsInPhase(snap: WorkflowSnapshot, phase: PhaseKey): AgentRow[] {
  return agentRowsByPhase(snap).get(phase) ?? [];
}

/** Every agent row keyed by phase title, for views that show all phases at once. */
export function agentRowsByPhase(snap: WorkflowSnapshot): Map<PhaseKey, AgentRow[]> {
  return new Map(groupAgentsByPhase(snap).map((group) => [group.phase, group.agents.map(toAgentRow)]));
}
