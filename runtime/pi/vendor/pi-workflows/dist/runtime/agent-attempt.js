/** One attempt of a live agent() call: its scope, the runner request, the deadline, and the bounded wait for a stopped runner. */
import { WorkflowError, WorkflowErrorCode } from "../errors.js";
import { createAgentStoreTools } from "../shared-store.js";
export function openAttemptScope(ctx) {
    const controller = new AbortController();
    const abort = () => controller.abort();
    const external = ctx.options.signal;
    const fatal = ctx.state.runFatalController.signal;
    if (ctx.isAborted()) {
        abort();
    }
    else {
        external?.addEventListener("abort", abort, { once: true });
        fatal.addEventListener("abort", abort, { once: true });
    }
    let closed = false;
    const releases = [];
    return {
        signal: controller.signal,
        abort,
        // The terminal drain closes every attempt of the run at once.
        isOpen: () => !closed && !ctx.state.agentCallbacksClosed,
        onClose: (release) => releases.push(release),
        close: () => {
            closed = true;
            external?.removeEventListener("abort", abort);
            fatal.removeEventListener("abort", abort);
            for (const release of releases)
                release();
        },
    };
}
/** Hand the call to the agent runner; every callback goes quiet once the attempt's scope closes. */
export function dispatchAttempt(ctx, call, cwd, scope, usage) {
    const { options, runId } = ctx;
    const { options: agentOptions, definition, id, label, phase } = call;
    return ctx.agentRunner.run(call.prompt, {
        label,
        // Identifiable name for persisted sessions (persistAgentSessions).
        sessionName: agentOptions.thread ? `workflow:${runId} thread:${agentOptions.thread}` : `workflow:${runId} ${label}`,
        schema: agentOptions.schema,
        signal: scope.signal,
        // The agentType body is the user's own role prompt; the runtime adds no text of its own.
        instructions: definition?.prompt || undefined,
        model: call.modelSpec,
        thinking: agentOptions.thinking ?? definition?.thinking,
        tier: agentOptions.tier,
        modelRegistry: options.modelRegistry,
        toolNames: definition?.tools,
        disallowedToolNames: definition?.disallowedTools,
        // Store tools attribute this agent's writes to its call id, so the delta can be journaled and replayed.
        systemTools: createAgentStoreTools(ctx.store, id, scope.isOpen),
        cwd,
        onModelResolved: (model) => {
            if (!scope.isOpen())
                return;
            // Replace the pre-resolution guess while the agent is still running;
            // onAgentEnd and the journal carry the same value afterwards.
            call.displayModel = model;
            options.onAgentModel?.({ id, label, phase, model });
        },
        onNotice: (message) => {
            if (scope.isOpen())
                ctx.log(message);
        },
        // The usage tracker ignores an attempt once its usage is committed.
        onUsageProgress: usage.reportProgress,
        onUsage: usage.reportTerminal,
        onSessionCreated: ({ sessionId, sessionFile }) => {
            if (!scope.isOpen())
                return;
            options.onAgentSession?.({ callId: id, sessionId, sessionFile });
        },
        onHistory: (history) => {
            if (!scope.isOpen())
                return;
            options.onAgentHistory?.({ id, label, phase, history });
        },
        registerSteer: (steer) => {
            // An ended attempt must not hand a late session to the host, and its
            // registration ends with it even if the runner never unregisters.
            if (!scope.isOpen())
                return () => { };
            const unregister = options.registerAgentSteer?.({ id, label, steer }) ?? (() => { });
            scope.onClose(unregister);
            return unregister;
        },
        thread: agentOptions.thread,
    });
}
/**
 * Race the attempt against its deadline. `onTimeout` runs before the timeout
 * error wins so the caller can abort the session and free its resources; the
 * losing promise settles later, if ever, so the caller waits for it with
 * awaitRunnerStop before committing usage or retrying.
 */
export async function withTimeout(promise, ms, label, onTimeout) {
    if (ms === null)
        return promise;
    let timeoutId;
    const timeoutPromise = new Promise((_, reject) => {
        timeoutId = setTimeout(() => {
            try {
                onTimeout?.();
            }
            catch {
                // Best-effort cleanup; never let it mask the timeout error.
            }
            reject(new WorkflowError(`Agent "${label}" timed out after ${ms}ms; raise defaultAgentTimeoutMs in the workflow settings to allow longer runs`, WorkflowErrorCode.AGENT_TIMEOUT, { recoverable: true }));
        }, ms);
    });
    try {
        return await Promise.race([promise, timeoutPromise]);
    }
    finally {
        if (timeoutId)
            clearTimeout(timeoutId);
    }
}
/**
 * Wait for a runner that was told to stop, up to `graceMs` (Infinity waits for
 * good). Its final usage arrives as it unwinds, and a thread's next turn must
 * not overlap it. False means it ignored its abort and is to be abandoned.
 */
export async function awaitRunnerStop(runner, graceMs) {
    const stopped = runner.then(() => true, () => true);
    if (graceMs === Number.POSITIVE_INFINITY)
        return stopped;
    let timer;
    const grace = new Promise((resolve) => {
        timer = setTimeout(() => resolve(false), graceMs);
    });
    try {
        return await Promise.race([stopped, grace]);
    }
    finally {
        clearTimeout(timer);
    }
}
