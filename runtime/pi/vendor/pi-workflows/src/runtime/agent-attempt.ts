/** One attempt of a live agent() call: its scope, the runner request, the deadline, and the bounded wait for a stopped runner. */

import type { AgentHistoryEntry } from "../agent-history.js";
import type { createAgentCallUsageTracker } from "../agent-usage.js";
import { WorkflowError, WorkflowErrorCode } from "../errors.js";
import { createAgentStoreTools } from "../shared-store.js";
import type { AgentCall } from "./agent-execution.js";
import type { RunContext } from "./run-context.js";

export type AttemptUsage = ReturnType<ReturnType<typeof createAgentCallUsageTracker>["startAttempt"]>;

/**
 * The lifetime of one attempt. Each attempt gets its own controller so a
 * timeout can dispose that session (and release its messages) instead of
 * leaving it streaming while a retry starts another. It also follows the
 * caller's signal (pause/stop/Esc) and the run-fatal seal, so in-flight
 * siblings of a lost run wind down. Abort is cooperative and a runner may
 * outlive its attempt, so closing the scope also shuts out everything the
 * runner can still report. close() must run when the attempt ends, or
 * listeners pile up on the run's signals.
 */
export interface AttemptScope {
  readonly signal: AbortSignal;
  /** Ask the runner to stop. */
  readonly abort: () => void;
  /** Whether the runner's callbacks and store writes still reach the run. */
  readonly isOpen: () => boolean;
  /** Run `release` on close: an abandoned runner never withdraws what it registered with the host. */
  readonly onClose: (release: () => void) => void;
  /** End the attempt: detach from the run's signals and ignore the runner from now on. */
  readonly close: () => void;
}

export function openAttemptScope(ctx: RunContext): AttemptScope {
  const controller = new AbortController();
  const abort = () => controller.abort();
  const external = ctx.options.signal;
  const fatal = ctx.state.runFatalController.signal;
  if (ctx.isAborted()) {
    abort();
  } else {
    external?.addEventListener("abort", abort, { once: true });
    fatal.addEventListener("abort", abort, { once: true });
  }
  let closed = false;
  const releases: Array<() => void> = [];
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
      for (const release of releases) release();
    },
  };
}

/** Hand the call to the agent runner; every callback goes quiet once the attempt's scope closes. */
export function dispatchAttempt(
  ctx: RunContext,
  call: AgentCall,
  cwd: string | undefined,
  scope: AttemptScope,
  usage: AttemptUsage,
): Promise<unknown> {
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
    onModelResolved: (model: string) => {
      if (!scope.isOpen()) return;
      // Replace the pre-resolution guess while the agent is still running;
      // onAgentEnd and the journal carry the same value afterwards.
      call.displayModel = model;
      options.onAgentModel?.({ id, label, phase, model });
    },
    onNotice: (message: string) => {
      if (scope.isOpen()) ctx.log(message);
    },
    // The usage tracker ignores an attempt once its usage is committed.
    onUsageProgress: usage.reportProgress,
    onUsage: usage.reportTerminal,
    onSessionCreated: ({ sessionId, sessionFile }: { sessionId: string; sessionFile?: string }) => {
      if (!scope.isOpen()) return;
      options.onAgentSession?.({ callId: id, sessionId, sessionFile });
    },
    onHistory: (history: AgentHistoryEntry[]) => {
      if (!scope.isOpen()) return;
      options.onAgentHistory?.({ id, label, phase, history });
    },
    registerSteer: (steer: (text: string) => Promise<void>) => {
      // An ended attempt must not hand a late session to the host, and its
      // registration ends with it even if the runner never unregisters.
      if (!scope.isOpen()) return () => {};
      const unregister = options.registerAgentSteer?.({ id, label, steer }) ?? (() => {});
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
export async function withTimeout<T>(
  promise: Promise<T>,
  ms: number | null,
  label: string,
  onTimeout?: () => void,
): Promise<T> {
  if (ms === null) return promise;

  let timeoutId: NodeJS.Timeout | undefined;
  const timeoutPromise = new Promise<never>((_, reject) => {
    timeoutId = setTimeout(() => {
      try {
        onTimeout?.();
      } catch {
        // Best-effort cleanup; never let it mask the timeout error.
      }
      reject(
        new WorkflowError(
          `Agent "${label}" timed out after ${ms}ms; raise defaultAgentTimeoutMs in the workflow settings to allow longer runs`,
          WorkflowErrorCode.AGENT_TIMEOUT,
          { recoverable: true },
        ),
      );
    }, ms);
  });

  try {
    return await Promise.race([promise, timeoutPromise]);
  } finally {
    if (timeoutId) clearTimeout(timeoutId);
  }
}

/**
 * Wait for a runner that was told to stop, up to `graceMs` (Infinity waits for
 * good). Its final usage arrives as it unwinds, and a thread's next turn must
 * not overlap it. False means it ignored its abort and is to be abandoned.
 */
export async function awaitRunnerStop(runner: Promise<unknown>, graceMs: number): Promise<boolean> {
  const stopped = runner.then(
    () => true,
    () => true,
  );
  if (graceMs === Number.POSITIVE_INFINITY) return stopped;
  let timer: NodeJS.Timeout | undefined;
  const grace = new Promise<false>((resolve) => {
    timer = setTimeout(() => resolve(false), graceMs);
  });
  try {
    return await Promise.race([stopped, grace]);
  } finally {
    clearTimeout(timer);
  }
}
