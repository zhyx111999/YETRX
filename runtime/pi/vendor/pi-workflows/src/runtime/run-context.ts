/** Holds one run's mutable state and the shared services that every workflow global uses. */

import type { AgentRegistry } from "../agent-registry.js";
import type { AgentUsage } from "../agent-usage.js";
import {
  type WorkflowCheckpointSuspensionError,
  WorkflowError,
  WorkflowErrorCode,
  workflowAbortedError,
} from "../errors.js";
import type { WorkflowLogger } from "../logger.js";
import type { ModelRoutingConfig } from "../model-routing.js";
import type { SharedStore } from "../shared-store.js";
import type {
  JournalEntry,
  WorkflowAgentRunner,
  WorkflowCheckpoint,
  WorkflowMeta,
  WorkflowRunOptions,
} from "../workflow.js";
import { CallSequence } from "./call-position.js";
import { createLimiter } from "./run-limits.js";
import { createScriptCalls, type ScriptCalls } from "./script-calls.js";

export interface RuntimeState {
  currentPhase?: string;
  logs: string[];
  phases: string[];
  /** Numbers the script body's agent()/checkpoint() calls by position; fan-out branches fork from it. */
  scriptCalls: CallSequence;
  /**
   * Whether a call has missed the resume journal (changed, new or unusable).
   * A cached result replays only while none has: every call made after a miss
   * runs live, since it may depend on the missed one, instead of serving a
   * stale result.
   */
  journalMissed: boolean;
  /** Journal indices this execution replayed, so a miss can tell the caller which entries remain valid. */
  replayedIndices: Set<number>;
  limiter: <T>(fn: () => Promise<T>) => Promise<T>;
  /** Logical agent()/checkpoint() slots reserved so far; capped by maxAgents. */
  agentCount: number;
  tokenUsage: AgentUsage;
  /**
   * Set after the terminal drain: abandoned agents can no longer report, and an
   * agent() call that reaches the runtime after the run ended is rejected.
   */
  agentCallbacksClosed: boolean;
  /** Active attempts whose usage must be finalized when an abort drain abandons them. */
  pendingUsageFinalizers: Set<() => void>;
  /** Exact durable checkpoint currently waiting or resuming. */
  activeCheckpointId: string | null;
  /** Persisted response currently being consumed by exactly one checkpoint. */
  activeCheckpointResponse: WorkflowCheckpoint | null;
  /** Every durable checkpoint ID encountered in this run; IDs must be unique. */
  seenCheckpointIds: Set<string>;
  /** A durably accepted suspension cannot be swallowed into a successful run. */
  checkpointSuspension?: WorkflowCheckpointSuspensionError;
  /**
   * Aborted once an error escapes the script uncaught, i.e. the run is lost.
   * Not on the first fan-out rejection: parallel() turns recoverable failures
   * into null and a script may catch agent() errors itself. Every attempt links
   * its AbortController to this signal as well as to options.signal, so
   * in-flight siblings stop instead of finishing a doomed run. A controller is
   * needed because the runtime cannot abort the caller-owned options.signal.
   */
  runFatalController: AbortController;
  /**
   * Every promise the workflow globals handed to the script. The run drains
   * them before disposing the SharedStore, so a call the script forgot to
   * await cannot touch state after teardown, and then settles the failures
   * the script never observed.
   */
  calls: ScriptCalls;
  /** Named conversations currently executing. */
  activeThreads: Set<string>;
  /** Whether a threaded call has invalidated journal replay for the rest of the run. */
  resumeBarrierReached: boolean;
}

/** Fixed inputs of a run, resolved once by runWorkflow. */
interface RunSetup {
  readonly options: WorkflowRunOptions;
  readonly runId: string;
  readonly baseCwd: string;
  readonly maxAgents: number;
  readonly agentTimeoutMs: number | null;
  /** How long an agent told to stop (by an abort or its timeout) may take to settle. Infinity waits. */
  readonly drainAbortGraceMs: number;
  readonly routingConfig: ModelRoutingConfig;
  /** Snapshotted once per run so two agent() calls never observe a mid-run edit. */
  readonly agentRegistry: AgentRegistry;
  readonly agentRunner: WorkflowAgentRunner;
  readonly store: SharedStore;
  readonly logger: WorkflowLogger;
  readonly state: RuntimeState;
}

/** A run's setup plus the helpers shared by agent(), parallel(), pipeline() and checkpoint(). */
export interface RunContext extends RunSetup {
  /** Record a line in the run's logs and the persisted log file. */
  readonly log: (message: string) => void;
  /** True after an external abort (pause/stop/Esc) or once the run is sealed as failed. */
  readonly isAborted: () => boolean;
  /** Throw the pending checkpoint suspension or an abort error, if either applies. */
  readonly throwIfAborted: () => void;
  readonly agentLimitError: () => WorkflowError;
  /** Every logical agent() call and checkpoint consumes one slot of the run's maxAgents cap. */
  readonly ensureAgentCapacity: () => void;
  /**
   * The journaled entry the call at `index` replays, or undefined to run it
   * live. Longest-unchanged-prefix resume: an entry replays only while no
   * earlier call has missed (independent calls replay on their own match), so
   * an edited upstream call never leaves stale downstream results. A missing
   * or different entry is a miss; one `passOver` names is neither replayed
   * nor a miss.
   */
  readonly replayable: (
    index: number,
    hash: string,
    passOver?: (entry: JournalEntry) => boolean,
  ) => JournalEntry | undefined;
}

export function createRuntimeState(meta: WorkflowMeta, options: WorkflowRunOptions, concurrency: number): RuntimeState {
  // The first declared phase is the default, so agents started before any
  // phase() call group under it instead of an orphan "(no phase)" bucket.
  const firstPhase = meta.phases?.[0]?.title;
  return {
    logs: [],
    phases: firstPhase ? [firstPhase] : [],
    currentPhase: firstPhase,
    scriptCalls: CallSequence.forScript(),
    journalMissed: false,
    replayedIndices: new Set<number>(),
    limiter: createLimiter(concurrency),
    // Not seeded on resume: the script replays from call 0 and every replayed
    // call counts again, so maxAgents stays a cumulative cap on its own.
    agentCount: 0,
    // Seeded on resume because replayed calls deliberately commit no usage.
    tokenUsage: options.initialTokenUsage
      ? { ...options.initialTokenUsage }
      : { input: 0, output: 0, total: 0, cost: 0, cacheRead: 0, cacheWrite: 0 },
    agentCallbacksClosed: false,
    pendingUsageFinalizers: new Set<() => void>(),
    activeCheckpointId: options.resumeCheckpoint?.checkpointId ?? null,
    activeCheckpointResponse: options.resumeCheckpoint ?? null,
    seenCheckpointIds: new Set<string>(),
    runFatalController: new AbortController(),
    calls: createScriptCalls(),
    activeThreads: new Set<string>(),
    resumeBarrierReached: false,
  };
}

export function createRunContext(setup: RunSetup): RunContext {
  const { options, state, logger, maxAgents, runId } = setup;
  const log = (message: string) => {
    const text = String(message);
    state.logs.push(text);
    logger.log(text);
  };
  // Both abort sources go through this one check, so no call site can forget the run-fatal seal.
  const isAborted = () => Boolean(options.signal?.aborted || state.runFatalController.signal.aborted);
  const throwIfAborted = () => {
    if (!options.signal?.aborted && state.checkpointSuspension) throw state.checkpointSuspension;
    if (isAborted()) throw workflowAbortedError();
  };
  const agentLimitError = () =>
    new WorkflowError(
      `Agent limit exceeded (${state.agentCount}/${maxAgents}). Raise defaultMaxAgents in the workflow settings, reload, then resume run "${runId}" — the journaled prefix replays free.`,
      WorkflowErrorCode.AGENT_LIMIT_EXCEEDED,
      { recoverable: false },
    );
  const ensureAgentCapacity = () => {
    if (state.agentCount >= maxAgents) throw agentLimitError();
  };
  // In a script run the first miss ends the unchanged prefix: entries not
  // replayed by then describe calls that now run after a changed one, so the
  // caller drops them instead of keeping them for a later resume to replay.
  const noteJournalMiss = () => {
    if (state.journalMissed) return;
    state.journalMissed = true;
    if (!options.independentCalls && options.resumeJournal?.size) {
      options.onJournalPrefixEnd?.([...state.replayedIndices]);
    }
  };
  const replayable = (
    index: number,
    hash: string,
    passOver?: (entry: JournalEntry) => boolean,
  ): JournalEntry | undefined => {
    // A thread's conversation cannot be replayed, so a thread call ends replay for the run.
    if (state.resumeBarrierReached) return undefined;
    const entry = options.resumeJournal?.get(index);
    if (entry?.hash !== hash) {
      noteJournalMiss();
      return undefined;
    }
    if (passOver?.(entry)) return undefined;
    if (state.journalMissed && !options.independentCalls) return undefined;
    state.replayedIndices.add(index);
    return entry;
  };
  return {
    ...setup,
    log,
    isAborted,
    throwIfAborted,
    agentLimitError,
    ensureAgentCapacity,
    replayable,
  };
}
