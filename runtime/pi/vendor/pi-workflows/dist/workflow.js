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
import { WorkflowAgent } from "./agent.js";
import { loadAgentRegistry } from "./agent-registry.js";
import { DEFAULT_MAX_AGENTS } from "./config.js";
import { deepFreeze } from "./deep-freeze.js";
import { WorkflowError, WorkflowErrorCode } from "./errors.js";
import { createWorkflowLogger } from "./logger.js";
import { parseModelRoutingFromMeta } from "./model-routing.js";
import { createAgent } from "./runtime/agent-call.js";
import { createCheckpoint } from "./runtime/checkpoint.js";
import { cloneDurableJsonValue } from "./runtime/durable-json.js";
import { createParallel, createPipeline } from "./runtime/fanout.js";
import { createLoopGuard, guardLoops } from "./runtime/loop-guard.js";
import { createRunContext, createRuntimeState } from "./runtime/run-context.js";
import { resolveAgentTimeoutMs, resolveConcurrency, resolveDrainAbortGraceMs } from "./runtime/run-limits.js";
import { finishRun } from "./runtime/run-teardown.js";
import { trackScriptCalls } from "./runtime/script-calls.js";
import { readMetaExport } from "./runtime/script-meta.js";
import { routeRealmRejections } from "./runtime/script-rejections.js";
import { createScriptContext, realmPromisePrototype, runScript } from "./runtime/script-vm.js";
import { SharedStore } from "./shared-store.js";
export async function runWorkflow(script, options = {}) {
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
    let outcome;
    let stopRoutingRejections = () => { };
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
    }
    catch (error) {
        outcome = { ok: false, error };
    }
    // A script that held the thread too long fails even if it caught the guard's error.
    const runaway = loopGuard.tripped();
    if (runaway)
        outcome = { ok: false, error: runaway };
    let result;
    try {
        result = (await finishRun(ctx, outcome));
    }
    finally {
        loopGuard.dispose();
        stopRoutingRejections();
    }
    const logFile = logger.persist();
    if (logFile)
        log(`Logs persisted to ${logFile}`);
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
function createScriptGlobals(ctx) {
    const { options, state, log, baseCwd } = ctx;
    const phase = (title) => {
        // A non-string or blank title would become a phase no row can be grouped under.
        if (typeof title !== "string" || !title.trim()) {
            throw new WorkflowError("phase(title) needs a non-empty string title", WorkflowErrorCode.SCRIPT_VALIDATION_ERROR, {
                recoverable: false,
            });
        }
        state.currentPhase = title;
        if (!state.phases.includes(title))
            state.phases.push(title);
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
            warn: (m) => log(`[warn] ${String(m)}`),
            error: (m) => log(`[error] ${String(m)}`),
        },
    };
}
/** Split a script into its validated literal `meta` and the body that runs in the vm. */
export function parseWorkflowScript(script) {
    const { meta, start, end, program } = readMetaExport(script);
    // Loop guards are inserted after the meta statement only, so its range still holds.
    const guarded = guardLoops(script, program);
    // Keep the meta statement's line breaks, so body line N is still script line N in stack traces.
    const metaLineBreaks = script.slice(start, end).replace(/[^\n\r\u2028\u2029]/g, "");
    return { meta, body: guarded.slice(0, start) + metaLineBreaks + guarded.slice(end) };
}
