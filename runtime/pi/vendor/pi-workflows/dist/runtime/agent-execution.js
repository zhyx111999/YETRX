/** Runs a live agent() call under the concurrency limit: its worktree, usage accounting, retries and journaling. */
import { createAgentCallUsageTracker, sumAgentUsage } from "../agent-usage.js";
import { WorkflowError, WorkflowErrorCode, workflowAbortedError, wrapError } from "../errors.js";
import { createWorktree, removeWorktree } from "../worktree.js";
import { awaitRunnerStop, dispatchAttempt, openAttemptScope, withTimeout } from "./agent-attempt.js";
import { normalizeAgentRetries, retryBackoffMs } from "./run-limits.js";
/** A string result that is blank means the subagent produced no answer at all. */
function isEmptyTextAgentResult(result, schema) {
    return schema === undefined && typeof result === "string" && result.trim().length === 0;
}
function estimateTokens(value) {
    return Math.ceil(JSON.stringify(value ?? "").length / 4);
}
export function runLiveAgent(ctx, call) {
    return ctx.state.limiter(async () => {
        // The slot can arrive long after the call was made.
        throwIfCallCancelled(ctx, call);
        if (call.isolation !== "worktree")
            return runAttempts(ctx, call, undefined);
        const worktree = await createIsolatedWorktree(ctx, call);
        try {
            // Worktree creation awaited; the run may have stopped meanwhile.
            throwIfCallCancelled(ctx, call);
            return await runAttempts(ctx, call, worktree.cwd);
        }
        finally {
            await releaseWorktree(ctx, call, worktree);
        }
    });
}
/**
 * Checked before every dispatch: an abort, a pending checkpoint suspension, or
 * this call's fan-out breaching maxAgents cancels the call. None of them is a
 * failure of this agent, so the reason propagates as-is and the call is never
 * reported as failed. Checked first as the slot arrives, so a call that never
 * ran creates no worktree, announces no agent, and runs on resume.
 */
function throwIfCallCancelled(ctx, call) {
    ctx.throwIfAborted();
    if (call.batch?.cancelled)
        throw ctx.agentLimitError();
}
/** Requested isolation is mandatory. A kept tree belongs to this execution, not to a later retry or resume. */
async function createIsolatedWorktree(ctx, call) {
    const worktree = await createWorktree(ctx.baseCwd, call.label);
    if (!worktree.isolated) {
        throw new WorkflowError(`worktree isolation failed for "${call.label}": ${worktree.reason}`, WorkflowErrorCode.SCRIPT_VALIDATION_ERROR, { recoverable: false });
    }
    return worktree;
}
async function releaseWorktree(ctx, call, worktree) {
    if (call.options.keepWorktree === false) {
        await removeWorktree(worktree);
    }
    else {
        ctx.log(`worktree kept: ${worktree.cwd}${worktree.branch ? ` (${worktree.branch})` : ""}`);
    }
}
/** Announce the agent, then run attempts until one succeeds or a failure is final. */
async function runAttempts(ctx, call, worktreePath) {
    const { state, options, store } = ctx;
    const { id, label, phase, prompt } = call;
    const maxAttempts = normalizeAgentRetries(options.agentRetries ?? 0) + 1;
    // The same grace bounds an aborted run's final drain.
    const graceMs = ctx.drainAbortGraceMs;
    // Keeps provisional estimates apart from committed usage, sums retries, and
    // ignores callbacks from attempts that already settled.
    const usageTracker = createAgentCallUsageTracker((update) => {
        if (state.agentCallbacksClosed)
            return;
        if (update.committedUsage) {
            state.tokenUsage = sumAgentUsage(state.tokenUsage, update.committedUsage);
        }
        options.onAgentUsage?.({ id, label, phase, ...update });
    });
    options.onAgentStart?.({ id, label, phase, prompt, model: call.displayModel });
    for (let attempt = 1;; attempt++) {
        const outcome = await runAttempt(ctx, call, call.cwd ?? worktreePath, usageTracker.startAttempt(), graceMs);
        const end = {
            id,
            label,
            phase,
            tokens: outcome.usage.tokens,
            tokenUsage: outcome.usage.tokenUsage,
            worktree: worktreePath,
            model: call.displayModel,
        };
        if (outcome.ok) {
            const storeDelta = store.commitDelta(id);
            // A thread's turn cannot replay, so it is not journaled.
            if (!call.options.thread) {
                // The resolved model is journaled (not hashed) so replayed rows show what really ran.
                options.onAgentJournal?.({
                    index: call.index,
                    hash: call.hash,
                    result: outcome.result,
                    model: call.displayModel,
                    storeDelta,
                });
            }
            options.onAgentEnd?.({ ...end, result: outcome.result });
            return outcome.result;
        }
        const { error } = outcome;
        ctx.logger.error(`agent ${label} attempt ${attempt}/${maxAttempts} failed: ${error.message}`);
        // Every attempt shares this call's delta key. A failed attempt's writes
        // must not stay visible to siblings or merge into a later attempt's
        // journaled delta, whether or not a retry follows.
        store.discardDelta(id);
        if (!outcome.runnerStopped) {
            // A retry would start a second session beside the one still running.
            ctx.log(`agent "${label}" ignored its abort for ${graceMs}ms after ${error.code}; abandoned without a retry`);
        }
        else if (error.recoverable && attempt < maxAttempts) {
            ctx.log(`agent "${label}" attempt ${attempt}/${maxAttempts} failed: ${error.code} ${error.message}; retrying`);
            const backoffMs = retryBackoffMs(attempt, options);
            if (backoffMs > 0)
                await new Promise((resolve) => setTimeout(resolve, backoffMs));
            throwIfCallCancelled(ctx, call);
            continue;
        }
        else if (error.recoverable) {
            ctx.log(`agent "${label}" exhausted ${maxAttempts} attempt${maxAttempts === 1 ? "" : "s"}: ${error.code} ${error.message}`);
        }
        options.onAgentEnd?.({
            ...end,
            result: null,
            error: error.message,
            errorCode: error.code,
            recoverable: error.recoverable,
        });
        if (!error.recoverable)
            throw error;
        return null;
    }
}
/**
 * Dispatch one attempt, race its deadline, and commit its usage. A failure is
 * returned; only a stopped run throws, because that is no failure of this
 * agent and must be neither retried nor reported as one.
 */
async function runAttempt(ctx, call, cwd, usage, graceMs) {
    const { state } = ctx;
    const finalizeUsage = () => usage.commitTerminalUsage();
    state.pendingUsageFinalizers.add(finalizeUsage);
    const scope = openAttemptScope(ctx);
    let runner;
    try {
        runner = dispatchAttempt(ctx, call, cwd, scope, usage);
        // A timed-out runner can reject before the failure path waits for it.
        void runner.catch(() => undefined);
        const result = await withTimeout(runner, ctx.agentTimeoutMs, call.label, scope.abort);
        // Abort only, not throwIfAborted(): this result is paid for, so a pending
        // checkpoint suspension must not discard it unjournaled. The suspension
        // is raised at the next script-level check instead.
        if (ctx.isAborted())
            throw workflowAbortedError();
        if (isEmptyTextAgentResult(result, call.options.schema)) {
            throw new WorkflowError("Subagent produced no assistant output", WorkflowErrorCode.AGENT_EMPTY_OUTPUT, {
                recoverable: true,
                agentLabel: call.label,
            });
        }
        return {
            ok: true,
            result,
            usage: usage.commitWithFallback(() => estimateTokens(result) + estimateTokens(call.prompt)),
        };
    }
    catch (error) {
        // A timed-out or aborted runner may still be unwinding.
        const runnerStopped = runner === undefined || (await awaitRunnerStop(runner, graceMs));
        if (ctx.isAborted()) {
            usage.commitTerminalUsage();
            throw error;
        }
        return {
            ok: false,
            error: wrapError(error, { agentLabel: call.label }),
            usage: usage.commitWithFallback(() => estimateTokens(call.prompt)),
            runnerStopped,
        };
    }
    finally {
        scope.close();
        state.pendingUsageFinalizers.delete(finalizeUsage);
    }
}
