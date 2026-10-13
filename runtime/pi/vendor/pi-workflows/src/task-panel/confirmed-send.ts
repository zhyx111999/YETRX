/**
 * Wraps a captured host session's sendCustomMessage so a workflow result
 * counts as delivered only once that session has persisted it.
 */

import type { DeliverySend } from "./delivery-endpoints.js";
import { sessionFileContainsEntry } from "./session-file-scan.js";

/** The parts of an AgentSession a confirmed send relies on; optional so test doubles can supply only what they use. */
export interface HostSession {
  sendCustomMessage?: DeliverySend;
  isStreaming?: boolean;
  isIdle?: boolean;
  waitForIdle?: () => Promise<void>;
  subscribe?: (
    listener: (event: { type?: string; message?: { role?: string; customType?: string; details?: unknown } }) => void,
  ) => () => void;
  sessionManager?: {
    getSessionId?: () => string;
    getBranch?: () => Array<{ id?: string; type?: string; customType?: string; details?: unknown }>;
    getSessionFile?: () => string | undefined;
  };
}

interface ActiveStreamingWaiter {
  sessionId: string;
  deliveryId: string;
  hasStarted: () => boolean;
  cancel: (error: Error) => void;
}
const activeStreamingWaiters = new Set<ActiveStreamingWaiter>();

const DEFAULT_STREAMING_ACK_TIMEOUT_MS = 60_000;
let streamingAckTimeoutMs = DEFAULT_STREAMING_ACK_TIMEOUT_MS;

function deliveryIdFromDetails(details: unknown): string | undefined {
  if (!details || typeof details !== "object") return undefined;
  const deliveryId = (details as { deliveryId?: unknown }).deliveryId;
  return typeof deliveryId === "string" && deliveryId ? deliveryId : undefined;
}

/** Whether the session's current branch holds this delivery and the session file has it on disk. */
function sessionHasDelivery(session: HostSession, deliveryId: string): boolean | undefined {
  const sm = session.sessionManager;
  if (typeof sm?.getBranch !== "function" || typeof sm.getSessionFile !== "function") return undefined;
  try {
    const entries = sm
      .getBranch()
      .filter(
        (entry) =>
          typeof entry.id === "string" &&
          entry.type === "custom_message" &&
          entry.customType === "workflow-result" &&
          deliveryIdFromDetails(entry.details) === deliveryId,
      );
    const file = sm.getSessionFile();
    // getEntries includes abandoned branches; even getBranch can contain a
    // memory-only entry after a failed/lazy persist. Neither alone is an ACK.
    return !!file && entries.some((entry) => sessionFileContainsEntry(file, entry));
  } catch {
    return undefined;
  }
}

/**
 * A confirming send for the host session `sid`. It waits until the host is idle
 * before sending, so a result never sits in the host's unobservable follow-up
 * queue, and resolves only once the result is persisted in the session file.
 */
export function confirmedSessionSend(session: HostSession, sid: string): DeliverySend | undefined {
  const send = session.sendCustomMessage;
  if (typeof send !== "function") return undefined;

  return (message, options) => {
    const deliveryId = deliveryIdFromDetails(message.details);
    if (deliveryId && sessionHasDelivery(session, deliveryId) === true) return Promise.resolve();

    if (!deliveryId) {
      return send.call(session, message, options);
    }
    if (typeof session.subscribe !== "function" || sessionHasDelivery(session, deliveryId) === undefined) {
      return Promise.reject(new Error("host cannot confirm workflow result delivery"));
    }

    return new Promise<void>((resolve, reject) => {
      let settled = false;
      let started = false;
      let waitingForIdle = false;
      let unsubscribe = () => {};
      let timer: ReturnType<typeof setTimeout> | undefined;

      const finish = (error?: unknown) => {
        if (settled) return;
        settled = true;
        if (timer) clearTimeout(timer);
        activeStreamingWaiters.delete(waiterRecord);
        unsubscribe();
        if (error) reject(error);
        else resolve();
      };

      const waiterRecord: ActiveStreamingWaiter = {
        sessionId: sid,
        deliveryId,
        hasStarted: () => started,
        cancel: (err) => finish(err),
      };
      activeStreamingWaiters.add(waiterRecord);

      if (streamingAckTimeoutMs > 0) {
        timer = setTimeout(() => {
          queueMicrotask(() => {
            if (settled) return;
            if (sessionHasDelivery(session, deliveryId) === true) finish();
            // No host queue item exists before start, so a retry is safe. Once
            // started, never infer non-delivery from elapsed time.
            else if (!started) finish(new Error("timed out waiting for an idle host"));
          });
        }, streamingAckTimeoutMs);
        timer.unref?.();
      }

      unsubscribe =
        session.subscribe?.((event) => {
          if (event.type === "message_end") {
            // A fresh session may flush the input only when its first assistant
            // message completes. Recheck disk, not just the custom input event.
            queueMicrotask(() => {
              if (settled) return;
              const persisted = sessionHasDelivery(session, deliveryId);
              if (persisted === true) finish();
              // A persistence failure is not proof that a started prompt was
              // cancelled. Its send promise remains the failure boundary.
            });
            return;
          }

          if (event.type === "agent_settled" || event.type === "compaction_end") {
            // An unrelated queue_update only describes user text; it cannot
            // establish ownership or consumption of a custom follow-up.
            queueMicrotask(startWhenIdle);
          }
        }) ?? (() => {});

      function startWhenIdle() {
        if (!deliveryId || !send) return;
        if (settled || started) return;
        if (session.isStreaming || session.isIdle === false) {
          if (!waitingForIdle && typeof session.waitForIdle === "function") {
            waitingForIdle = true;
            void session.waitForIdle().then(() => {
              waitingForIdle = false;
              startWhenIdle();
            }, finish);
          }
          return;
        }
        if (session.sessionManager?.getSessionId?.() !== sid) {
          finish(new Error("host session identity changed before delivery"));
          return;
        }
        if (sessionHasDelivery(session, deliveryId) === true) {
          finish();
          return;
        }
        // No await between idle inspection and send: this path never installs
        // a custom message in the host's unobservable follow-up queue.
        started = true;
        if (timer) clearTimeout(timer);
        try {
          const pending = send.call(session, message, options);
          if (pending == null || typeof (pending as { then?: unknown }).then !== "function") {
            finish(new Error("workflow result send did not return a thenable"));
            return;
          }
          Promise.resolve(pending).then(
            () =>
              finish(
                sessionHasDelivery(session, deliveryId) === true
                  ? undefined
                  : new Error("host send settled without persisting workflow result"),
              ),
            (error) => finish(sessionHasDelivery(session, deliveryId) === true ? undefined : error),
          );
        } catch (error) {
          finish(error);
        }
      }
      startWhenIdle();
    });
  };
}

/**
 * Cancel a session's waits that have not sent yet. A started send can still
 * persist after shutdown or reload, so it keeps its lock until it settles.
 */
export function cancelUnsentWaiters(sessionId: string, reason: string): void {
  for (const waiter of activeStreamingWaiters) {
    if (waiter.sessionId === sessionId && !waiter.hasStarted()) {
      waiter.cancel(new Error(reason));
    }
  }
}

export function _setStreamingAckTimeoutForTests(timeoutMs: number): void {
  streamingAckTimeoutMs = timeoutMs;
}

/** @internal test helper — cancel every wait and restore the default timeout. */
export function _resetConfirmedSendsForTests(): void {
  for (const waiter of activeStreamingWaiters) {
    waiter.cancel(new Error("test reset"));
  }
  activeStreamingWaiters.clear();
  streamingAckTimeoutMs = DEFAULT_STREAMING_ACK_TIMEOUT_MS;
}
