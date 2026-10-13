/**
 * Runs one workflow script: the entry point of the workflow runtime.
 *
 * runWorkflow() parses the script's literal `meta`, prepares the run (limits,
 * agent registry, logger, shared store), exposes the workflow globals in a vm
 * context, runs the script, and finally drains every outstanding call and
 * settles the run. The parts live in src/runtime/:
 *   - script-meta.ts, determinism.ts        parse-time checks and the in-realm guard
 *   - run-limits.ts                         host settings turned into enforced limits
 *   - run-context.ts                        per-run state and the helpers shared by the globals
 *   - agent-call.ts                         agent(): validation, slot, journal replay
 *   - agent-execution.ts, agent-attempt.ts  live runs: worktree, retries, timeout, usage
 *   - fanout.ts, checkpoint.ts              parallel()/pipeline() and checkpoint()
 *   - script-calls.ts                       tracking of every promise the globals return
 *   - script-vm.ts                          the vm context and script execution
 *   - run-teardown.ts                       fatal seal, drain, verdict, final accounting
 */

import type { TSchema } from "typebox";
import { type AgentRunOptions, WorkflowAgent, type WorkflowAgentOptions } from "./agent.js";
import type { AgentHistoryEntry } from "./agent-history.js";
import { type AgentRegistry, loadAgentRegistry } from "./agent-registry.js";
import type { AgentUsage } from "./agent-usage.js";
import { DEFAULT_MAX_AGENTS } from "./config.js";
import { deepFreeze } from "./deep-freeze.js";
import { WorkflowError, WorkflowErrorCode } from "./errors.js";
import { createWorkflowLogger } from "./logger.js";
import { parseModelRoutingFromMeta } from "./model-routing.js";
import type { ModelThinkingLevel } from "./model-spec.js";
import { createAgent } from "./runtime/agent-call.js";
import { createCheckpoint } from "./runtime/checkpoint.js";
import { cloneDurableJsonValue } from "./runtime/durable-json.js";
import { createParallel, createPipeline } from "./runtime/fanout.js";
import { createLoopGuard, guardLoops } from "./runtime/loop-guard.js";
import { createRunContext, createRuntimeState, type RunContext } from "./runtime/run-context.js";
import { resolveAgentTimeoutMs, resolveConcurrency, resolveDrainAbortGraceMs } from "./runtime/run-limits.js";
import { finishRun, type ScriptOutcome } from "./runtime/run-teardown.js";
import { trackScriptCalls } from "./runtime/script-calls.js";
import { readMetaExport } from "./runtime/script-meta.js";
import { routeRealmRejections } from "./runtime/script-rejections.js";
import { createScriptContext, realmPromisePrototype, runScript } from "./runtime/script-vm.js";
import { SharedStore } from "./shared-store.js";
import type { WorkflowRuntimeImplementations } from "./workflow-capability-contract.js";

export interface WorkflowMetaPhase {
  title: string;
  detail?: string;
  model?: string;
}

export interface WorkflowMeta {
  name: string;
  description: string;
  phases?: WorkflowMetaPhase[];
  /** Default model for agents whose phase has no route and that set no model/tier. */
  model?: string;
}

/** One cached agent/checkpoint result, keyed by its deterministic call index. */
export interface JournalEntry {
  /** Where the agent()/checkpoint() call sits in the script, as numbered by runtime/call-position.ts. */
  index: number;
  /**
   * Identity hash of the call (runtime/call-identity.ts) over every input that
   * shapes its result: for agent() the prompt and resolved call options, for
   * checkpoint() its kind, id and payload. Replay requires an exact match.
   */
  hash: string;
  result: unknown;
  /**
   * Keys this agent set in the shared store. Resume applies the deltas in call
   * order, which rebuilds every agent's writes no matter which parallel agent
   * finished first. Absent when the agent wrote nothing.
   */
  storeDelta?: Record<string, unknown>;
  /**
   * The model this call actually ran on, so a replayed row shows what really
   * ran instead of the pre-resolution guess. Absent on checkpoints, which run
   * no model.
   */
  model?: string;
}

/** Minimal injected agent surface used by the workflow runtime and deterministic tests. */
export interface WorkflowAgentRunner {
  run(prompt: string, options: AgentRunOptions<TSchema>): Promise<unknown>;
}

export interface WorkflowCheckpointInput {
  readonly checkpointId: string;
  readonly kind: string;
  readonly payload: unknown;
}

export interface WorkflowCheckpoint extends WorkflowCheckpointInput {
  readonly version: 1;
  readonly status: "waiting" | "resuming" | "consumed";
  readonly response?: unknown;
  readonly createdAt: string;
  readonly consumedAt?: string;
}

export interface WorkflowRunOptions extends WorkflowAgentOptions {
  args?: unknown;
  agent?: WorkflowAgentRunner;
  /** The session's main model (provider/id); the pre-resolution display guess and, with inheritMainModel on, the untagged routing target. */
  mainModel?: string;
  /**
   * Named subagent definitions for `agent({ agentType })`. Snapshotted once per
   * run for determinism. Defaults to scanning `.pi/agents` (project) +
   * `~/.pi/agent/agents` (user). Injectable for tests.
   */
  agentRegistry?: AgentRegistry;
  /** Maximum concurrent agents, clamped to 1..16. Hosts pass the defaultConcurrency setting. */
  concurrency?: number;
  /** Retry attempts after a recoverable agent failure, clamped to 0..3. Default 0. */
  agentRetries?: number;
  signal?: AbortSignal;
  /** Maximum logical agent()/checkpoint() calls in this run. Default: 1000 */
  maxAgents?: number;
  /** Timeout per agent attempt in milliseconds. null/omitted means no hard timeout. */
  agentTimeoutMs?: number | null;
  /**
   * Grace period (ms) for a runner told to stop: the final drain once the run
   * is aborted (external abort or a fatal error), and an agent attempt that
   * timed out. The abort signal is cooperative: a runner that ignores it would
   * otherwise hold its concurrency slot, or keep the run from ever ending.
   * After the grace a timed-out agent is abandoned without a retry and ends
   * as a timeout failure; the drain stops waiting, the store is disposed,
   * nothing more is journaled, and the host ignores stale writes.
   *
   * Until the run is aborted the drain waits without limit, also when it
   * paused at a checkpoint, because those results are wanted. Default 10_000;
   * Infinity always waits. Finite values in [1, 2^31-1] are rounded down;
   * anything else (NaN, 0, negatives, overflow) uses the default.
   */
  drainAbortGraceMs?: number;
  /** Whether to persist logs to disk. Default: true */
  persistLogs?: boolean;
  /** Run ID for persistence. Auto-generated if not provided. */
  runId?: string;
  /** Resume: cached agent/checkpoint results keyed by call index. */
  resumeJournal?: Map<number, JournalEntry>;
  /**
   * No agent() call depends on another's result (a task-list run), so each
   * journaled call replays on its own hash match instead of only inside the
   * unchanged prefix.
   */
  independentCalls?: boolean;
  /** Called after each live agent completes so the caller can persist the journal. */
  onAgentJournal?: (entry: JournalEntry) => void;
  /**
   * A resumed script run missed its journal: only the listed entries, which
   * this execution replayed before the miss, still hold. The caller drops the
   * rest, which belong to calls now made after a changed one, so a later
   * resume cannot replay them. Never called for independent calls.
   */
  onJournalPrefixEnd?: (replayedIndices: number[]) => void;
  /** Active durable checkpoint response supplied by WorkflowManager.resume(). */
  resumeCheckpoint?: WorkflowCheckpoint;
  /** Persist a durable checkpoint transition before it becomes externally observable. */
  onWorkflowCheckpoint?: (checkpoint: WorkflowCheckpoint) => void;
  /**
   * Backoff (ms) before retry attempt N (1-based — the attempt that just
   * failed). Default: min(250 * 2^(N-1), 2000) — immediate retries let a whole
   * parallel() batch hammer the provider synchronously. The retry keeps its
   * concurrency slot during the backoff. Return 0 to disable; negative,
   * NaN, or non-finite returns fall back to the default; a throwing callback
   * is ignored (default used). Finite positive values are honored up to
   * 2000ms (above that, clamped — a multi-day park would ignore aborts for
   * its whole duration); abort latency during the wait is bounded by the
   * effective value.
   */
  agentRetryBackoffMs?: (failedAttempt: number) => number;
  /**
   * Seed the run's cumulative token usage from a previously-persisted total
   * (resume()). Replayed calls commit no usage, so without this the totals
   * reported for a resumed run would cover only what THIS execution spent.
   */
  initialTokenUsage?: AgentUsage;
  onLog?: (message: string) => void;
  onPhase?: (title: string) => void;
  onAgentStart?: (event: {
    id: string;
    label: string;
    phase?: string;
    prompt: string;
    model?: string;
    /** True when this event is a journal replay, not a new child launch. */
    replayed?: boolean;
  }) => void;
  /** Called immediately after a child SessionManager is created. */
  onAgentSession?: (event: { callId: string; sessionId: string; sessionFile?: string }) => void;
  onAgentEnd?: (event: {
    /**
     * Unique per agent() CALL (not per label — concurrent agents routinely
     * share a label, e.g. parallel()'s default `"${phase} agent N"` labels or
     * an author-supplied label reused across a fan-out). Stable across this
     * call's start/end/history events. Callers must key any per-agent
     * bookkeeping on this, never on label, to avoid misattributing a
     * concurrent same-label agent's event to the wrong entry.
     */
    id: string;
    label: string;
    phase?: string;
    result: unknown;
    tokens?: number;
    tokenUsage?: AgentUsage;
    worktree?: string;
    model?: string;
    error?: string;
    errorCode?: WorkflowErrorCode;
    recoverable?: boolean;
  }) => void;
  /** Called with cumulative display usage and an exact delta whenever usage becomes committed. */
  onAgentUsage?: (event: {
    id: string;
    label: string;
    phase?: string;
    tokenUsage: AgentUsage;
    committedUsage?: AgentUsage;
  }) => void;
  onAgentHistory?: (event: { id: string; label: string; phase?: string; history: AgentHistoryEntry[] }) => void;
  /**
   * Called when a live agent's session can take instructions, with `steer`
   * queueing one into it. Returns the function that unregisters the session
   * when it ends; it must tolerate a second call, because the runtime also
   * calls it when the attempt ends. `id` is the same per-call id as the
   * onAgent* events.
   */
  registerAgentSteer?: (agent: { id: string; label: string; steer: (text: string) => Promise<void> }) => () => void;
  /**
   * The agent's real model, pushed as soon as WorkflowAgent resolves it, long
   * before onAgentEnd. onAgentStart can only carry a guess (this call's
   * explicit/phase spec, else the session's main model), which is wrong for
   * tier-routed agents and for untagged agents that are routed through the
   * "medium" tier or inherit the main model (see routeAgentModel). When no
   * spec resolves (the settings default, or an implicit route that degraded
   * to it) it reports the model the session bound. Fires once per attempt
   * (and per turn of a named thread): treat it as idempotent. `id` is the
   * per-call id of the other onAgent* events.
   */
  onAgentModel?: (event: { id: string; label: string; phase?: string; model: string }) => void;
  onTokenUsage?: (usage: AgentUsage) => void;
  /**
   * The run's fatal error. One that escaped the script is reported before
   * runWorkflow drains in-flight agents, which preserves error provenance for
   * hosts whose own lifecycle control can race with that cooperative drain; a
   * failure the script never observed is only known after the drain.
   * Observational only: callback failures (sync or async) are ignored. A
   * durable checkpoint suspension does NOT invoke this — an intentional
   * human-in-the-loop pause is not a fatal error (in-flight siblings are
   * waited out, not aborted).
   */
  onRunFatal?: (error: unknown) => void | PromiseLike<void>;
}

export interface WorkflowRunResult<T = unknown> {
  meta: WorkflowMeta;
  result: T;
  logs: string[];
  phases: string[];
  agentCount: number;
  durationMs: number;
  runId: string;
  tokenUsage: AgentUsage;
}

export interface AgentOptions<TSchemaDef extends TSchema | undefined = TSchema | undefined> {
  label?: string;
  phase?: string;
  schema?: TSchemaDef;
  /**
   * Run this agent on a specific model (`provider/modelId` or a bare `modelId`).
   * Takes precedence over the agentType model, `tier` and the phase route; with
   * none of them set, the agent layer picks its default model.
   */
  model?: string;
  /** Pi thinking level. Used when `model` has no `:thinking` suffix. */
  thinking?: ModelThinkingLevel;
  /**
   * Coarse model tier ("small" | "medium" | "big"), resolved from the user's
   * model-tiers config (see /workflows-models). An explicit `model` takes
   * precedence; a tier takes precedence over the phase model. When the tier has
   * no configured entry it falls back to the session's main model.
   */
  tier?: string;
  isolation?: "worktree" | false;
  /** Default true. False deletes the isolation worktree after the call (test runs). */
  keepWorktree?: boolean;
  /**
   * Bind this call to an existing absolute directory. The runtime resolves it
   * to its real path before dispatch so coding tools and agent identity agree.
   * Cannot be combined with worktree isolation.
   */
  cwd?: string;
  /**
   * Re-enter a named subagent conversation during this workflow invocation.
   * Calls using the same name must be sequential. Thread state is never resumed
   * across a later run or resume.
   */
  thread?: string;
  /**
   * Name of a registered subagent definition (`.pi/agents/<name>.md`, project >
   * user). Binds that definition's tool allow/denylist, model, and body prompt
   * to this agent. An explicit `model` overrides the definition's model; the
   * definition's model overrides `tier`/phase. An unknown name logs a warning
   * and falls back to default tools/model with no role prompt.
   */
  agentType?: string;
}

export async function runWorkflow<T = unknown>(
  script: string,
  options: WorkflowRunOptions = {},
): Promise<WorkflowRunResult<T>> {
  const started = Date.now();
  const { meta, body } = parseWorkflowScript(script);

  // 1. Prepare the run.
  // Per-phase models from meta.phases[].model, with meta.model as the default.
  const routingConfig = parseModelRoutingFromMeta(meta.phases, meta.model);
  const maxAgents = options.maxAgents ?? DEFAULT_MAX_AGENTS;
  const agentTimeoutMs = resolveAgentTimeoutMs(options);
  const drainAbortGraceMs = resolveDrainAbortGraceMs(options.drainAbortGraceMs);
  const runId = options.runId ?? `run-${started.toString(36)}`;
  const baseCwd = options.cwd ?? process.cwd();
  // Read once per run so agent() calls never see a mid-run edit; a resume reads it again.
  const agentRegistry = options.agentRegistry ?? loadAgentRegistry(baseCwd);
  const logger = createWorkflowLogger({
    runId,
    cwd: baseCwd,
    persist: options.persistLogs ?? true,
    onLog: options.onLog,
  });
  const ctx = createRunContext({
    options,
    runId,
    baseCwd,
    maxAgents,
    agentTimeoutMs,
    drainAbortGraceMs,
    routingConfig,
    agentRegistry,
    logger,
    state: createRuntimeState(meta, options, resolveConcurrency(options.concurrency)),
    agentRunner: options.agent ?? new WorkflowAgent(options),
    store: new SharedStore(),
  });
  const { state, log } = ctx;

  // 2. Expose the workflow globals and run the script. The globals are built
  // inside the try, so args that cannot be persisted losslessly fail the run
  // like any script error; the teardown then drains every call the script
  // started and settles the run.
  let outcome: ScriptOutcome;
  let stopRoutingRejections = () => {};
  const loopGuard = createLoopGuard(() => ctx.throwIfAborted());
  try {
    const context = createScriptContext(createScriptGlobals(ctx), logger);
    // Promises the script makes itself must not take the host down if it leaves one rejected.
    const own = state.calls.ownRejections;
    stopRoutingRejections = routeRealmRejections(realmPromisePrototype(context), {
      rejected: (promise, reason) => own.set(promise, reason),
      handled: (promise) => own.delete(promise),
    });
    outcome = { ok: true, value: await runScript(context, body, `${meta.name || "workflow"}.js`, loopGuard.check) };
  } catch (error) {
    outcome = { ok: false, error };
  }
  // A script that held the thread too long fails even if it caught the guard's error.
  const runaway = loopGuard.tripped();
  if (runaway) outcome = { ok: false, error: runaway };
  let result: T;
  try {
    result = (await finishRun(ctx, outcome)) as T;
  } finally {
    loopGuard.dispose();
    stopRoutingRejections();
  }

  const logFile = logger.persist();
  if (logFile) log(`Logs persisted to ${logFile}`);
  return {
    meta,
    result,
    logs: state.logs,
    phases: state.phases,
    agentCount: state.agentCount,
    durationMs: Date.now() - started,
    runId,
    tokenUsage: { ...state.tokenUsage },
  };
}

/** The workflow globals declared by the capability contract, bound to this run. */
function createScriptGlobals(ctx: RunContext) {
  const { options, state, log, baseCwd } = ctx;
  const phase = (title: unknown) => {
    // A non-string or blank title would become a phase no row can be grouped under.
    if (typeof title !== "string" || !title.trim()) {
      throw new WorkflowError(
        "phase(title) needs a non-empty string title",
        WorkflowErrorCode.SCRIPT_VALIDATION_ERROR,
        {
          recoverable: false,
        },
      );
    }
    state.currentPhase = title;
    if (!state.phases.includes(title)) state.phases.push(title);
    options.onPhase?.(title);
  };
  return {
    // Every global that returns a promise goes through trackScriptCalls.
    agent: trackScriptCalls(state.calls, "agent", createAgent(ctx)),
    parallel: trackScriptCalls(state.calls, "parallel", createParallel(ctx)),
    pipeline: trackScriptCalls(state.calls, "pipeline", createPipeline(ctx)),
    checkpoint: trackScriptCalls(state.calls, "checkpoint", createCheckpoint(ctx)),
    log,
    phase,
    // The caller's args are persisted for resume. The script gets a frozen
    // copy, so an in-place edit such as args.items.sort() throws instead of
    // changing what a resumed run sees.
    args: options.args === undefined ? undefined : deepFreeze(cloneDurableJsonValue(options.args, "workflow args")),
    cwd: baseCwd,
    process: Object.freeze({ cwd: () => baseCwd }),
    console: {
      log,
      info: log,
      warn: (m: unknown) => log(`[warn] ${String(m)}`),
      error: (m: unknown) => log(`[error] ${String(m)}`),
    },
  } satisfies WorkflowRuntimeImplementations;
}

/** Split a script into its validated literal `meta` and the body that runs in the vm. */
export function parseWorkflowScript(script: string): { meta: WorkflowMeta; body: string } {
  const { meta, start, end, program } = readMetaExport(script);
  // Loop guards are inserted after the meta statement only, so its range still holds.
  const guarded = guardLoops(script, program);
  // Keep the meta statement's line breaks, so body line N is still script line N in stack traces.
  const metaLineBreaks = script.slice(start, end).replace(/[^\n\r\u2028\u2029]/g, "");
  return { meta, body: guarded.slice(0, start) + metaLineBreaks + guarded.slice(end) };
}
