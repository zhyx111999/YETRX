// Rebuilds a managed run from its persisted record so resume() can execute it again.

import type { AgentUsage } from "../agent-usage.js";
import { type PriorAgentExecution, recomputeWorkflowSnapshot, type WorkflowAgentSnapshot } from "../display.js";
import { agentHasNonTerminalStatus, INTERRUPTED_AGENT_CAUSE } from "../run-agent-settlement.js";
import {
  type PersistedAgentState,
  type PersistedRunState,
  type RunLease,
  sanitizeAutoResumeAttempts,
  VALID_PERSISTED_AGENT_STATUSES,
} from "../run-persistence.js";
import type { JournalEntry } from "../workflow.js";
import type { ManagedRun } from "../workflow-manager.js";
import { type AgentTimestamps, supersedeAgentRow } from "./agent-rows.js";
import { retargetAgentRow, type TaskListMoves } from "./task-list-reuse.js";

export interface ResumeInput {
  runId: string;
  /** The record, already re-read under `lease`. */
  persisted: PersistedRunState;
  /** The script and args to run: the record's own, or the edit being resumed with. */
  script: string;
  args: unknown;
  /** Where an edited task list moved its unchanged tasks. */
  taskMoves?: TaskListMoves;
  lease: RunLease;
}

export interface ResumedRun {
  managed: ManagedRun;
  /** The journal entries the new execution may replay. */
  journal: JournalEntry[];
  /** The record's token total, so the resumed run reports cumulative usage. */
  priorTokenUsage?: AgentUsage;
}

/**
 * The live run for a resume. The snapshot is seeded with the recorded agents,
 * so the record never loses its earlier agents (an edited script may never
 * replay them). Each call keeps one row: a replay updates it, and a re-run
 * takes it over with the earlier execution kept as history.
 * Rows of agents that were still queued or running settle as interrupted.
 * Every field of the record is shape-checked, because nothing may throw once
 * the lease is held.
 */
export function resumedRun(input: ResumeInput): ResumedRun {
  const { runId, persisted, taskMoves } = input;
  const recordedAgents = Array.isArray(persisted.agents) ? persisted.agents : [];
  const persistedAgents = taskMoves
    ? recordedAgents.map((agent) => retargetAgentRow(agent, taskMoves))
    : recordedAgents;
  const priorTokenUsage = persisted.tokenUsage ? { ...persisted.tokenUsage } : undefined;
  const journal = replayableJournal(persisted, taskMoves);
  const seeded = seedAgents(persistedAgents, persisted.updatedAt);
  const managed: ManagedRun = {
    runId,
    status: "running",
    snapshot: recomputeWorkflowSnapshot({
      name: persisted.workflowName,
      phases: Array.isArray(persisted.phases) ? persisted.phases : [],
      currentPhase: persisted.currentPhase,
      logs: Array.isArray(persisted.logs) ? persisted.logs : [],
      agents: seeded.agents,
      agentCount: 0,
      runningCount: 0,
      doneCount: 0,
      errorCount: 0,
      // Start from the recorded total, so a pause before the first new agent
      // finishes keeps the earlier spend.
      tokenUsage: priorTokenUsage,
    }),
    controller: new AbortController(),
    startedAt: new Date(),
    // An edited script and args become the run's own, so a later resume uses them.
    script: input.script,
    args: input.args,
    journal,
    checkpoint: persisted.checkpoint,
    // A run without an owner is claimed by the resuming session (see WorkflowManager.resume).
    sessionId: persisted.sessionId,
    parentSessionId: persisted.parentSessionId,
    parentSessionFile: persisted.parentSessionFile,
    pendingDelivery: persisted.pendingDelivery,
    lease: input.lease,
    autoResumeAttempts: sanitizeAutoResumeAttempts(persisted.autoResumeAttempts),
    // Run limits are not restored: every execution uses the manager's current ones.
    toolset: persisted.toolset,
    agentTimestamps: seeded.timestamps,
    agentsById: new Map(),
    // Replayed calls create no child session, so they keep their recorded one.
    agentSessionsByCallId: new Map(
      persistedAgents
        .filter((agent) => agent && typeof agent === "object" && agent.callId && (agent.sessionId || agent.sessionFile))
        .map(
          (agent) => [agent.callId as string, { sessionId: agent.sessionId, sessionFile: agent.sessionFile }] as const,
        ),
    ),
    replayedAgentStatesByCallId: new Map(
      persistedAgents
        .filter((agent) => agent && typeof agent === "object" && agent.callId)
        .map((agent) => [agent.callId as string, agent] as const),
    ),
    agentTimestampsByCallId: seeded.timestampsByCallId,
    replayingAgentCalls: new Set(),
    executionAgentRows: new Set(),
    steerable: new Map(),
  };
  return { managed, journal, priorTokenUsage };
}

/**
 * The record's journal entries that can replay. Malformed entries are dropped.
 * An edited task list moves each unchanged task's entry to its new index and
 * drops the rest.
 */
function replayableJournal(persisted: PersistedRunState, taskMoves: TaskListMoves | undefined): JournalEntry[] {
  return (Array.isArray(persisted.journal) ? persisted.journal : []).flatMap((entry): JournalEntry[] => {
    if (!entry || typeof entry !== "object" || typeof entry.index !== "number") return [];
    if (!taskMoves) return [entry];
    const index = taskMoves.indices.get(entry.index);
    return index === undefined ? [] : [{ ...entry, index }];
  });
}

/**
 * Snapshot rows for the recorded agents, one per call, with their timestamps
 * by row id and by call id. Only well-formed rows are kept. A later row of a
 * call is a later execution of it, so it takes over the call's row and the
 * earlier one becomes its history. Interrupted rows settle at the current
 * time, but never before the record's last write, so a clock that moved
 * backwards cannot end an agent before the record was updated.
 */
function seedAgents(
  persistedAgents: PersistedAgentState[],
  updatedAt: string,
): {
  agents: WorkflowAgentSnapshot[];
  timestamps: Map<number, AgentTimestamps>;
  timestampsByCallId: Map<string, AgentTimestamps>;
} {
  const settledAt = new Date(Math.max(Date.now(), Date.parse(updatedAt) || 0)).toISOString();
  const agents: WorkflowAgentSnapshot[] = [];
  const timestamps = new Map<number, AgentTimestamps>();
  const timestampsByCallId = new Map<string, AgentTimestamps>();
  const positionByCallId = new Map<string, number>();
  for (const agent of persistedAgents) {
    if (
      !agent ||
      typeof agent !== "object" ||
      Array.isArray(agent) ||
      !VALID_PERSISTED_AGENT_STATUSES.has(agent.status as PersistedAgentState["status"]) ||
      typeof agent.label !== "string" ||
      typeof agent.prompt !== "string"
    ) {
      continue;
    }
    const {
      startedAt: rawStartedAt,
      endedAt: rawEndedAt,
      callId: rawCallId,
      priorExecutions: rawPriorExecutions,
      ...snapshotFields
    } = agent;
    // Fields of the wrong type are dropped rather than written back.
    const startedAt = typeof rawStartedAt === "string" ? rawStartedAt : undefined;
    const endedAt = typeof rawEndedAt === "string" ? rawEndedAt : undefined;
    const callId = typeof rawCallId === "string" ? rawCallId : undefined;
    const priorExecutions = validPriorExecutions(rawPriorExecutions);
    const position = callId === undefined ? undefined : positionByCallId.get(callId);
    const id = position === undefined ? agents.length + 1 : position + 1;
    const interrupted = agentHasNonTerminalStatus(agent.status);
    const rowTimestamps = startedAt
      ? { startedAt, endedAt: endedAt ?? (interrupted ? settledAt : undefined) }
      : interrupted
        ? { startedAt: settledAt, endedAt: settledAt }
        : // A finished row with only an end time keeps it as both ends.
          endedAt
          ? { startedAt: endedAt, endedAt }
          : undefined;
    if (callId !== undefined) {
      // The last row of a call is its latest execution. When that row has no
      // timestamps, a replay must not borrow an older row's.
      if (rowTimestamps) timestampsByCallId.set(callId, rowTimestamps);
      else timestampsByCallId.delete(callId);
    }
    const row: WorkflowAgentSnapshot = {
      ...snapshotFields,
      id,
      callId,
      ...(priorExecutions ? { priorExecutions } : {}),
      // The interrupt cause replaces any earlier error, as on disk.
      ...(interrupted
        ? {
            status: "skipped",
            error: INTERRUPTED_AGENT_CAUSE.error,
            errorCode: INTERRUPTED_AGENT_CAUSE.errorCode,
            recoverable: false,
          }
        : {}),
    };
    if (position === undefined) {
      if (callId !== undefined) positionByCallId.set(callId, agents.length);
      agents.push(row);
    } else {
      agents[position] = supersedeAgentRow(agents[position], timestamps.get(id), row);
    }
    if (rowTimestamps) timestamps.set(id, rowTimestamps);
    else timestamps.delete(id);
  }
  return { agents, timestamps, timestampsByCallId };
}

/** The well-formed entries of a recorded history; anything else is dropped rather than written back. */
function validPriorExecutions(value: unknown): PriorAgentExecution[] | undefined {
  if (!Array.isArray(value)) return undefined;
  const valid = value.filter(
    (entry): entry is PriorAgentExecution =>
      !!entry &&
      typeof entry === "object" &&
      !Array.isArray(entry) &&
      VALID_PERSISTED_AGENT_STATUSES.has(entry.status) &&
      typeof entry.label === "string",
  );
  return valid.length ? valid : undefined;
}
