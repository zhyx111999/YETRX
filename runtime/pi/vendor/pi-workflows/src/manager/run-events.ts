// Applies the workflow runtime's progress events to a managed run's live snapshot.

import { type AgentUsage, createEmptyAgentUsage, sumAgentUsage } from "../agent-usage.js";
import { preview, type WorkflowAgentSnapshot } from "../display.js";
import { isProviderUsageLimit } from "../errors.js";
import type { WorkflowRunOptions } from "../workflow.js";
import type { ManagedRun } from "../workflow-manager.js";
import { nextAgentRowId, supersedeAgentRow } from "./agent-rows.js";

/** A running agent session that can take an instruction mid-run. */
export interface SteerableAgent {
  label: string;
  steer: (text: string) => Promise<void>;
}

/** What the event handlers need from the manager for one execution. */
export interface RunEventSink {
  /** Emit a manager event while this execution is still the run's current one. */
  emit(event: string, payload: unknown): void;
  /** Write the run now; with `required`, a failed write throws. */
  persist(required?: boolean): void;
  /** Write the run after a short window, for writes that come in bursts. */
  schedulePersist(): void;
  /** Write the run after the long progress window, so a process that dies mid-run keeps recent progress. */
  scheduleProgressPersist(): void;
}

export type RunEventHooks = Pick<
  WorkflowRunOptions,
  | "onWorkflowCheckpoint"
  | "onAgentJournal"
  | "onJournalPrefixEnd"
  | "onLog"
  | "onPhase"
  | "onAgentStart"
  | "onAgentModel"
  | "onAgentUsage"
  | "onAgentSession"
  | "onAgentEnd"
  | "onAgentHistory"
  | "registerAgentSteer"
  | "onRunFatal"
  | "onTokenUsage"
>;

/**
 * runWorkflow callbacks for one execution of `managed`. Per-agent events are
 * matched through `agentsById`, keyed by the call's unique id: concurrent
 * agents often share a label, so matching by label would update the wrong row.
 */
export function runEventHooks(managed: ManagedRun, sink: RunEventSink): RunEventHooks {
  return {
    onWorkflowCheckpoint: (checkpoint) => {
      const previousCheckpoint = managed.checkpoint;
      const previousStatus = managed.status;
      managed.checkpoint = checkpoint;
      if (checkpoint.status === "waiting") managed.status = "paused";
      try {
        sink.persist(true);
      } catch (error) {
        managed.checkpoint = previousCheckpoint;
        managed.status = previousStatus;
        throw error;
      }
    },
    onJournalPrefixEnd: (replayedIndices) => {
      const kept = new Set(replayedIndices);
      managed.journal = managed.journal.filter((e) => kept.has(e.index));
      sink.schedulePersist();
    },
    onAgentJournal: (entry) => {
      // Keep the latest entry per call index. This fires once per finished agent
      // and can burst, so it is the one throttled write; lifecycle writes flush it.
      managed.journal = managed.journal.filter((e) => e.index !== entry.index);
      managed.journal.push(entry);
      sink.schedulePersist();
    },
    onLog: (message) => {
      managed.snapshot.logs.push(message);
      sink.emit("log", { runId: managed.runId, message });
    },
    onPhase: (title) => {
      managed.snapshot.currentPhase = title;
      if (!managed.snapshot.phases.includes(title)) {
        managed.snapshot.phases.push(title);
      }
      sink.emit("phase", { runId: managed.runId, title });
    },
    onAgentStart: (event) => {
      const row = (event.replayed ? replayedRow(managed, event) : undefined) ?? newRow(managed, event);
      managed.agentsById.set(event.id, row);
      managed.executionAgentRows.add(row.id);
      // A replayed call launched no child, so it keeps its original timestamps.
      const priorTimestamp = event.replayed ? managed.agentTimestampsByCallId.get(event.id) : undefined;
      const timestamp = priorTimestamp ?? { startedAt: new Date().toISOString() };
      managed.agentTimestamps.set(row.id, { ...timestamp });
      // Mark every replay, even one without a recorded timestamp, so onAgentEnd
      // never treats it as live and overwrites its recorded end time and tokens.
      if (event.replayed) managed.replayingAgentCalls.add(event.id);
      managed.agentTimestampsByCallId.set(event.id, { ...timestamp });
      sink.emit("agentStart", { runId: managed.runId, ...event });
    },
    onAgentModel: (event) => {
      const agent = managed.agentsById.get(event.id);
      if (!agent) {
        return;
      }
      agent.model = event.model;
      // No write here: this fires once per attempt, and later writes carry the field.
      sink.emit("agentModel", { runId: managed.runId, agentId: agent.id, ...event });
    },
    onAgentUsage: (event) => {
      const agent = managed.agentsById.get(event.id);
      if (!agent) {
        return;
      }

      agent.tokens = event.tokenUsage.total;
      agent.tokenUsage = event.tokenUsage;
      if (event.committedUsage) {
        commitFinalizedAgentUsage(managed, event.committedUsage);
      }
      sink.emit("agentUsage", { runId: managed.runId, ...event });
      // Displays sum live per-agent usage; this refreshes them without adding
      // estimates to the persisted run total.
      sink.emit("tokenUsage", { runId: managed.runId, usage: managed.snapshot.tokenUsage });
      sink.scheduleProgressPersist();
    },
    onAgentSession: (event) => {
      const agent = managed.agentsById.get(event.callId);
      if (!agent) return;
      agent.sessionId = event.sessionId;
      agent.sessionFile = event.sessionFile;
      managed.agentSessionsByCallId.set(event.callId, {
        sessionId: event.sessionId,
        sessionFile: event.sessionFile,
      });
      // The session exists before the first prompt. Write it now so an immediate
      // pause or crash still leaves the link to the child session.
      sink.persist();
    },
    onAgentEnd: (event) => {
      const agent = managed.agentsById.get(event.id);
      if (agent) {
        agent.status = event.result === null ? "error" : "done";
        // The full value serves the detail pager; compact views use the preview.
        agent.result = event.result;
        agent.resultPreview = preview(event.result);
        agent.error = event.error;
        agent.errorCode = event.errorCode;
        agent.recoverable = event.recoverable;
        const replayed = managed.replayingAgentCalls.has(event.id);
        if (!replayed) {
          if (event.tokenUsage) {
            agent.tokenUsage = event.tokenUsage;
            agent.tokens = event.tokenUsage.total;
          } else if (event.tokens !== undefined) {
            agent.tokens = event.tokens;
          }
        }
        if (event.model) agent.model = event.model;
        // Only a live call gets a new end time; a replay keeps its original one.
        const ts = managed.agentTimestamps.get(agent.id);
        if (ts && !replayed) {
          ts.endedAt = new Date().toISOString();
          managed.agentTimestampsByCallId.set(event.id, { ...ts });
        }
        managed.replayingAgentCalls.delete(event.id);
        managed.agentsById.delete(event.id);
      }
      sink.emit("agentEnd", { runId: managed.runId, ...event });
    },
    onAgentHistory: (event) => {
      const agent = managed.agentsById.get(event.id);
      if (agent) {
        agent.history = event.history;
        sink.scheduleProgressPersist();
      }
      sink.emit("agentHistory", { runId: managed.runId, agentId: agent?.id, ...event });
    },
    registerAgentSteer: ({ id, label, steer }) => {
      const entry: SteerableAgent = { label, steer };
      managed.steerable.set(id, entry);
      // A retry registers a new session under the same call id; only remove our own.
      return () => {
        if (managed.steerable.get(id) === entry) managed.steerable.delete(id);
      };
    },
    onRunFatal: (error) => {
      // runWorkflow reports this before draining the other agents. Remember a
      // provider limit that escaped before any pause()/stop(), so a later
      // control cannot erase the quota pause.
      if (isProviderUsageLimit(error) && !managed.controller.signal.aborted && managed.lifecycleControl === undefined) {
        managed.usageLimitEscapedBeforeLifecycleControl = error;
      }
    },
    onTokenUsage: (usage) => {
      managed.snapshot.tokenUsage = usage;
      sink.emit("tokenUsage", { runId: managed.runId, usage });
    },
  };
}

type AgentStartEvent = Parameters<NonNullable<WorkflowRunOptions["onAgentStart"]>>[0];

/**
 * The row seeded from the record at resume for a replayed call, refreshed from
 * the call. A replay is the recorded execution itself, so the row stays as it
 * is. Label is not part of the call hash, so a label-only edit still replays
 * and must update the row.
 */
function replayedRow(managed: ManagedRun, event: AgentStartEvent): WorkflowAgentSnapshot | undefined {
  const seeded = managed.snapshot.agents.find((agent) => agent.callId === event.id);
  if (!seeded) return undefined;
  seeded.label = event.label;
  seeded.phase = event.phase;
  seeded.prompt = event.prompt;
  if (event.model) seeded.model = event.model;
  return seeded;
}

/**
 * A running row for the call. A call has one row: when this execution re-runs
 * a call that already has one, the new row takes its place and keeps the
 * superseded execution as history. Row ids name one execution's row (its
 * timestamps are kept by id, and executionAgentRows holds the ids of this
 * execution's rows), so the new row gets a new id. A replayed call without a
 * seeded row carries its recorded model, session, and tokens.
 */
function newRow(managed: ManagedRun, event: AgentStartEvent): WorkflowAgentSnapshot {
  const agents = managed.snapshot.agents;
  const prior = event.replayed ? managed.replayedAgentStatesByCallId.get(event.id) : undefined;
  const priorSession = event.replayed ? managed.agentSessionsByCallId.get(event.id) : undefined;
  const row: WorkflowAgentSnapshot = {
    id: nextAgentRowId(agents),
    callId: event.id,
    label: event.label,
    phase: event.phase,
    prompt: event.prompt,
    status: "running",
    model: event.model ?? prior?.model,
    sessionId: priorSession?.sessionId,
    sessionFile: priorSession?.sessionFile,
    tokens: prior?.tokens,
    tokenUsage: prior?.tokenUsage,
  };
  const position = agents.findIndex((agent) => agent.callId === event.id);
  if (position === -1) {
    agents.push(row);
    return row;
  }
  const superseded = agents[position];
  agents[position] = supersedeAgentRow(superseded, managed.agentTimestamps.get(superseded.id), row);
  managed.agentTimestamps.delete(superseded.id);
  return agents[position];
}

/** Add one settled agent's exact usage to the run total that gets persisted. */
function commitFinalizedAgentUsage(managed: ManagedRun, usage: AgentUsage): void {
  const prior = managed.snapshot.tokenUsage;
  const priorUsage: AgentUsage = prior
    ? {
        input: prior.input,
        output: prior.output,
        total: prior.total,
        cost: prior.cost ?? 0,
        cacheRead: prior.cacheRead ?? 0,
        cacheWrite: prior.cacheWrite ?? 0,
        estimated: prior.estimated,
      }
    : createEmptyAgentUsage();
  managed.snapshot.tokenUsage = sumAgentUsage(priorUsage, usage);
}
