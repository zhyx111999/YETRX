// Turns a managed run into the record written to disk, settling agents nothing will finish.
import { recomputeWorkflowSnapshot } from "../display.js";
import { TERMINAL_RUN_STATUSES, terminalRunInterruptCause } from "../run-persistence.js";
/**
 * Mark the queued and running rows of a settled execution as skipped. Display
 * only: replay is keyed by the journal, not by these rows.
 */
export function settleInterruptedAgents(managed, cause, endedAt) {
    const endedAtIso = endedAt.toISOString();
    for (const agent of managed.snapshot.agents) {
        if (agent.status !== "running" && agent.status !== "queued")
            continue;
        agent.status = "skipped";
        agent.error = cause.error;
        agent.errorCode = cause.errorCode;
        agent.recoverable = false;
        const ts = managed.agentTimestamps.get(agent.id);
        if (ts)
            ts.endedAt ??= endedAtIso;
        else
            managed.agentTimestamps.set(agent.id, { startedAt: endedAtIso, endedAt: endedAtIso });
    }
    managed.snapshot = recomputeWorkflowSnapshot(managed.snapshot);
}
/** Settle leftover rows of a run that reached a terminal status, with that status as the cause. */
export function settleAgentsForTerminalRun(managed, endedAt) {
    if (!TERMINAL_RUN_STATUSES.has(managed.status))
        return;
    settleInterruptedAgents(managed, terminalRunInterruptCause(managed.status, {
        message: managed.error?.message,
        code: managed.error?.code,
    }), endedAt);
}
/** The first record of a background run, written before it starts executing. */
export function initialRunRecord(managed) {
    return {
        runId: managed.runId,
        workflowName: managed.snapshot.name,
        script: managed.script,
        args: managed.args,
        sessionId: managed.sessionId,
        parentSessionId: managed.parentSessionId,
        parentSessionFile: managed.parentSessionFile,
        status: "running",
        phases: managed.snapshot.phases,
        agents: [],
        logs: [],
        startedAt: managed.startedAt.toISOString(),
        updatedAt: managed.startedAt.toISOString(),
        // No autoResumeAttempts yet: the scheduler counts only after a usage-limit pause.
        toolset: managed.toolset,
    };
}
/** The full record of `managed` as of `now`. */
export function runRecord(managed, now) {
    const terminal = TERMINAL_RUN_STATUSES.has(managed.status);
    // Every run keeps its journal and script, so even a finished or stopped run
    // can be rerun with an edit that reuses its unchanged calls. Finished runs
    // also keep full agent results for the navigator; live and paused runs hold
    // them in memory and in the journal.
    const keepsAgentResults = managed.status === "completed" || managed.status === "aborted";
    const persistError = terminal && managed.status !== "completed" ? managed.error : undefined;
    const tokenUsage = managed.snapshot.tokenUsage;
    return {
        runId: managed.runId,
        workflowName: managed.snapshot.name,
        script: managed.script,
        args: managed.args,
        // The run's own delivery session, never the manager's current one. It
        // only seeds a new record: save() keeps an existing record's owner.
        sessionId: managed.sessionId,
        // Parent lineage never moves, unlike the delivery session.
        parentSessionId: managed.parentSessionId,
        parentSessionFile: managed.parentSessionFile,
        // Kept until delivery succeeds, so a result survives a missing endpoint or a restart.
        pendingDelivery: managed.pendingDelivery,
        journal: managed.journal,
        status: managed.status,
        error: persistError?.message,
        errorCode: persistError?.code,
        checkpoint: managed.checkpoint,
        // Written on every save, so the usage-limit scheduler reads a correct value
        // even when it loads the record before the pause is written.
        // The scheduler's backoff counter; dropping it would reset its give-up cap.
        autoResumeAttempts: managed.autoResumeAttempts,
        toolset: managed.toolset,
        pauseReason: pauseReason(managed),
        resetHint: managed.status === "paused" && managed.usageLimitPause ? managed.usageLimitPause.resetHint : undefined,
        phases: managed.snapshot.phases,
        currentPhase: managed.snapshot.currentPhase,
        // Each agent's own timestamps; a running agent has no endedAt yet.
        agents: managed.snapshot.agents.map((a) => {
            const { result, ...summary } = a;
            const ts = managed.agentTimestamps.get(a.id);
            return {
                ...summary,
                ...(keepsAgentResults && result !== undefined ? { result } : {}),
                startedAt: ts?.startedAt,
                endedAt: ts?.endedAt,
            };
        }),
        logs: managed.snapshot.logs,
        result: managed.result?.result,
        tokenUsage: tokenUsage
            ? {
                input: tokenUsage.input,
                output: tokenUsage.output,
                total: tokenUsage.total,
                cost: tokenUsage.cost,
                cacheRead: tokenUsage.cacheRead,
                cacheWrite: tokenUsage.cacheWrite,
                estimated: tokenUsage.estimated,
            }
            : undefined,
        startedAt: managed.startedAt.toISOString(),
        updatedAt: now.toISOString(),
        completedAt: terminal ? now.toISOString() : undefined,
        durationMs: managed.result?.durationMs ?? (terminal ? now.getTime() - managed.startedAt.getTime() : undefined),
    };
}
/**
 * Why a paused run is paused. A usage-limit pause wins over any checkpoint
 * state, because the usage-limit scheduler re-arms only runs marked with it.
 */
function pauseReason(managed) {
    if (managed.status !== "paused")
        return undefined;
    if (managed.usageLimitPause)
        return "usage_limit";
    return managed.checkpoint ? "workflow_checkpoint" : undefined;
}
