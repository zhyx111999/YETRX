// Runs one execution of a managed run through the workflow runtime and records how it ended.
import { WorkflowCheckpointSuspensionError } from "../errors.js";
import { INTERRUPTED_AGENT_CAUSE } from "../run-agent-settlement.js";
import { TERMINAL_RUN_STATUSES } from "../run-persistence.js";
import { isTaskListScript } from "../subagent-tasks.js";
import { runWorkflow, } from "../workflow.js";
import { runEventHooks } from "./run-events.js";
import { classifyFailure, controllingLifecycleAction, emptyFleetWarning } from "./run-outcome.js";
import { settleInterruptedAgents } from "./run-record.js";
/**
 * Execute `script` for `managed` and record the outcome on the run. Rejects with the run's error.
 * Preparing the runtime options happens inside the same failure path as the
 * script, so a failure there still records the run and releases its lease.
 */
export async function executeRun(live, managed, script, args, exec, settings) {
    try {
        const result = await runWorkflow(script, runOptions(live, managed, script, args, exec, settings));
        return settleCompleted(live, managed, result);
    }
    catch (error) {
        throw settleFailure(live, managed, error);
    }
    finally {
        live.deleteLeftovers(managed);
    }
}
/** The runWorkflow options for this execution. Throws when a toolset factory does. */
function runOptions(live, managed, script, args, exec, settings) {
    return {
        ...settings.runtime,
        args,
        // The run's own id, so result.runId is the id listRuns() and resume() use.
        runId: managed.runId,
        parentSessionFile: managed.parentSessionFile,
        signal: managed.controller.signal,
        drainAbortGraceMs: exec.drainAbortGraceMs,
        // Explicit tools win; otherwise re-resolve the run's toolset name (how a
        // resumed /deep-research keeps its web tools); otherwise the default tools.
        tools: exec.tools ?? (managed.toolset ? settings.toolsets?.[managed.toolset]?.() : undefined),
        resumeJournal: exec.resumeJournal,
        independentCalls: isTaskListScript(script),
        resumeCheckpoint: exec.resumeCheckpoint,
        initialTokenUsage: exec.initialTokenUsage,
        ...runEventHooks(managed, {
            emit: (event, payload) => live.emit(managed, event, payload),
            persist: (required) => live.persistRun(managed, required),
            schedulePersist: () => live.schedulePersist(managed),
            scheduleProgressPersist: () => live.scheduleProgressPersist(managed),
        }),
    };
}
/**
 * Record a returned script. A pause() or stop() that aborted this execution
 * keeps the status it set: the script may still return after the abort, while
 * the final drain winds down, and that is not a completion.
 */
function settleCompleted(live, managed, result) {
    const warning = emptyFleetWarning(managed);
    if (warning) {
        managed.snapshot.logs.push(warning);
        live.emit(managed, "log", { runId: managed.runId, message: warning });
    }
    managed.result = result;
    if (controllingLifecycleAction(managed) === undefined) {
        managed.status = "completed";
        live.emit(managed, "complete", { runId: managed.runId, result });
    }
    settleRecord(live, managed);
    return result;
}
/** Record an execution that threw. Returns the error to rethrow. */
function settleFailure(live, managed, error) {
    if (error instanceof WorkflowCheckpointSuspensionError && !managed.controller.signal.aborted) {
        managed.status = "paused";
        managed.error = undefined;
        live.persistRun(managed);
        if (live.isCurrent(managed)) {
            live.releaseLease(managed);
            live.emit(managed, "paused", {
                runId: managed.runId,
                reason: "workflow_checkpoint",
                checkpoint: managed.checkpoint,
            });
        }
        return error;
    }
    const outcome = classifyFailure(managed, error);
    if (outcome.status)
        managed.status = outcome.status;
    if (outcome.usageLimitPaused)
        managed.usageLimitPause = outcome.error;
    managed.error = outcome.error;
    if (outcome.usageLimitPaused) {
        live.emit(managed, "paused", {
            runId: managed.runId,
            reason: "usage_limit",
            error: outcome.error,
            resetHint: outcome.error.resetHint,
        });
    }
    else if (!outcome.announced && live.hasListeners("error")) {
        // EventEmitter throws on an unheard "error", which would skip the final
        // write, the lease release, and the rethrow.
        live.emit(managed, "error", { runId: managed.runId, error: outcome.error });
    }
    settleRecord(live, managed);
    return outcome.error;
}
/** Write the settled execution's final state and hand back its lease. */
function settleRecord(live, managed) {
    // The drain has finished, so agents still marked active no longer run.
    // A paused run keeps them as interrupted; a terminal write settles them by status.
    if (managed.status === "paused")
        settleInterruptedAgents(managed, INTERRUPTED_AGENT_CAUSE, new Date());
    live.persistRun(managed);
    if (live.isCurrent(managed)) {
        live.releaseLease(managed);
        // A paused run stays resumable, so it is never evicted.
        if (TERMINAL_RUN_STATUSES.has(managed.status))
            live.recordTerminalRun(managed);
    }
}
