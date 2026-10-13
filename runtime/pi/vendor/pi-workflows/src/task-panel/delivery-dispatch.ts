/**
 * Sends a result through its own session's endpoint, clears the pending marker
 * only after the send is confirmed, and redelivers whatever is still pending.
 * Nothing a send's settlement throws escapes: the marker stays, the session's
 * warning channel reports it, and a bounded retry follows.
 */

import { errorMessage } from "../errors.js";
import { claimRunSession } from "../manager/session-owner.js";
import type { PendingDeliveryMarker, PersistedRunState } from "../run-persistence.js";
import type { ManagedRun, WorkflowManager } from "../workflow-manager.js";
import type { WorkflowSettings } from "../workflow-settings.js";
import {
  resolveDeliverySessionId,
  type SessionDeliveryEndpoint,
  sessionEndpoints,
  warnDelivery,
} from "./delivery-endpoints.js";
import { cancelDeliveryRetry, scheduleDeliveryRetry } from "./delivery-retry.js";
import {
  abandonDeletedDelivery,
  type ClearPendingResult,
  clearRunPending,
  deliveredAwaitingClear,
  markerWithId,
  markRunPending,
  persistDiskMarker,
  persistRunPending,
} from "./pending-markers.js";

/**
 * Rebuilds the message for a pending marker. A complete result is rendered again
 * from the live run or its record instead of storing the body twice on disk.
 */
export type PendingTextRenderer = (
  manager: WorkflowManager,
  runId: string,
  marker: PendingDeliveryMarker,
  loadSettings?: () => WorkflowSettings,
  run?: ManagedRun,
  persisted?: PersistedRunState,
) => string | undefined;

let renderPendingText: PendingTextRenderer | undefined;

/** Installed once by task-panel.ts, which owns the delivered wording. */
export function setPendingTextRenderer(render: PendingTextRenderer): void {
  renderPendingText = render;
}

/** Run id → token of the send that holds its lock, so a stale settlement cannot release a newer send. */
const inFlightDeliveries = new Map<string, number>();
let inFlightSeq = 0;

/** Text for any thrown value. Never throws, so reporting a failure cannot become a failure itself. */
/**
 * Resolve true only after the endpoint's thenable send settles while the
 * session is still live. Void sends and durable appends are not an ACK: an
 * append writes history without starting a turn. Never clears markers.
 */
async function tryDeliverEndpoint(
  endpoint: SessionDeliveryEndpoint,
  content: string,
  deliveryId: string,
): Promise<boolean> {
  const { sessionId } = endpoint;
  if (endpoint.suspended || typeof endpoint.send !== "function") return false;
  // A stale endpoint object left behind by a rebind or drop must not send.
  if (sessionId && sessionEndpoints.get(sessionId) !== endpoint) return false;

  try {
    const ack = endpoint.send(
      { customType: "workflow-result", content, display: true, details: { deliveryId } },
      { triggerTurn: true, deliverAs: "followUp" },
    );
    if (typeof (ack as { then?: unknown } | null | undefined)?.then !== "function") {
      warnDelivery(
        sessionId,
        `[workflow-delivery] send for session ${sessionId} did not return a thenable; ` +
          "not treating as delivered (fail closed).",
      );
      return false;
    }
    await ack;
  } catch (error) {
    warnDelivery(sessionId, `Workflow result remains pending: ${errorMessage(error)}`);
    return false;
  }
  // Succeeded under this or a newer live endpoint for the same session.
  const current = sessionEndpoints.get(sessionId);
  return !!current && !current.suspended;
}

function releaseDelivery(runId: string, token: number): void {
  if (inFlightDeliveries.get(runId) === token) inFlightDeliveries.delete(runId);
}

function retryLater(manager: WorkflowManager, runId: string, sessionId: string, generation: number): void {
  scheduleDeliveryRetry(runId, sessionId, generation, (endpoint) =>
    flushSessionDiskPending(manager, sessionId, endpoint),
  );
}

/** Clear a confirmed delivery's marker. A failed clear is remembered, so its retry clears without sending again. */
function clearConfirmedDelivery(
  manager: WorkflowManager,
  runId: string,
  deliveryId: string,
  endpoint: SessionDeliveryEndpoint,
  run?: ManagedRun,
): ClearPendingResult {
  const clear = clearRunPending(manager, runId, deliveryId, run ?? manager.getRun(runId));
  if (clear === "failed") {
    deliveredAwaitingClear.set(runId, { deliveryId, sessionId: endpoint.sessionId, generation: endpoint.generation });
    retryLater(manager, runId, endpoint.sessionId, endpoint.generation);
  } else {
    deliveredAwaitingClear.delete(runId);
    cancelDeliveryRetry(runId);
  }
  return clear;
}

/**
 * A rebind or session handoff may have flushed while this send held the lock,
 * skipping the run. After either outcome, including a confirmed stale send,
 * flush the owner's live endpoint when it is not the one this send used.
 */
function handOffToOwner(manager: WorkflowManager, runId: string, endpoint: SessionDeliveryEndpoint): void {
  const run = manager.getRun(runId);
  const owner = run
    ? resolveDeliverySessionId(run, manager)
    : (manager.getPersistence().loadPreview(runId)?.sessionId ?? endpoint.sessionId);
  const current = owner ? sessionEndpoints.get(owner) : undefined;
  if (current && current !== endpoint && !current.suspended && current.manager) {
    flushSessionDiskPending(current.manager, current.sessionId, current);
  }
}

/**
 * Send under the run's in-flight lock, then settle the marker on the endpoint
 * that sent it: clear it after a confirmed send, otherwise retry. A replaced,
 * suspended, or dropped endpoint settles nothing; its successor's flush, or the
 * hand-off, delivers the marker instead.
 */
async function sendAndSettle(
  manager: WorkflowManager,
  runId: string,
  endpoint: SessionDeliveryEndpoint,
  content: string,
  deliveryId: string,
  token: number,
  run?: ManagedRun,
): Promise<void> {
  let delivered: boolean;
  try {
    delivered = await tryDeliverEndpoint(endpoint, content, deliveryId);
  } finally {
    // Everything after the send is synchronous, so releasing here admits no
    // concurrent send, and lets a flush below start this run's next one.
    releaseDelivery(runId, token);
  }

  const live = sessionEndpoints.get(endpoint.sessionId) === endpoint && !endpoint.suspended;
  if (!live) {
    cancelDeliveryRetry(runId);
  } else if (!delivered) {
    retryLater(manager, runId, endpoint.sessionId, endpoint.generation);
  } else if (clearConfirmedDelivery(manager, runId, deliveryId, endpoint, run) === "stale" && endpoint.manager) {
    // This send belonged to an older marker; deliver the newer one now.
    flushSessionDiskPending(endpoint.manager, endpoint.sessionId, endpoint);
  }
  handOffToOwner(manager, runId, endpoint);
}

/**
 * The last stop for anything a send's settlement throws, such as a run record
 * that has become unreadable. Only a confirmed clear removes a marker, so the
 * result is still pending: report it and retry on the session's live endpoint,
 * which may be a rebind whose own flush skipped the locked run. Never throws.
 */
function keepPending(manager: WorkflowManager, runId: string, sessionId: string, error: unknown): void {
  warnDelivery(sessionId, `Workflow ${runId}: result remains pending: ${errorMessage(error)}`);
  const endpoint = sessionEndpoints.get(sessionId);
  if (endpoint && !endpoint.suspended) retryLater(manager, runId, sessionId, endpoint.generation);
}

function deliverAndAck(
  manager: WorkflowManager,
  runId: string,
  sessionId: string,
  content: string,
  deliveryId: string,
  run?: ManagedRun,
): void {
  if (inFlightDeliveries.has(runId)) return;
  const endpoint = sessionEndpoints.get(sessionId);
  if (!endpoint || endpoint.suspended) return;

  // Already confirmed on this endpoint generation: only the marker clear is left.
  const delivered = deliveredAwaitingClear.get(runId);
  if (
    delivered?.deliveryId === deliveryId &&
    delivered.sessionId === sessionId &&
    delivered.generation === endpoint.generation
  ) {
    clearConfirmedDelivery(manager, runId, deliveryId, endpoint, run);
    return;
  }

  // Fail closed without a send, and without taking the lock: a synchronous
  // rebind + flush (e.g. the probe retry on the next session_start) must not be
  // locked out by a microtask that has not run yet.
  if (typeof endpoint.send !== "function") {
    warnDelivery(
      sessionId,
      `[workflow-delivery] delivery for ${runId} deferred: endpoint for session ${sessionId} has no thenable send function`,
    );
    return;
  }

  const token = ++inFlightSeq;
  inFlightDeliveries.set(runId, token);
  void sendAndSettle(manager, runId, endpoint, content, deliveryId, token, run).catch((error: unknown) =>
    keepPending(manager, runId, sessionId, error),
  );
}

/**
 * Deliver a live background run's result: save the marker first so a crash or
 * a missing endpoint cannot lose it, then send only through the originating
 * session's endpoint.
 */
export function routeBackgroundDelivery(
  manager: WorkflowManager,
  run: ManagedRun,
  marker: PendingDeliveryMarker,
  content: string,
): void {
  const pending = markRunPending(run, marker);
  const deliveryId = pending.deliveryId;
  if (!deliveryId) return;
  const saved = persistRunPending(manager, run, "notification");
  if (saved === "deleted") {
    abandonDeletedDelivery(run.runId, run);
    return;
  }

  const sessionId = resolveDeliverySessionId(run, manager);
  if (!sessionId) return;

  if (saved === "failed") {
    const endpoint = sessionEndpoints.get(sessionId);
    if (endpoint && !endpoint.suspended) retryLater(manager, run.runId, sessionId, endpoint.generation);
    return;
  }

  deliverAndAck(manager, run.runId, sessionId, content, deliveryId, run);
}

/** Deliver every result still pending for a session, from live runs and from disk. */
export function flushSessionDiskPending(
  manager: WorkflowManager,
  sessionId: string,
  endpoint: SessionDeliveryEndpoint,
): void {
  if (endpoint.suspended) return;

  const tryOne = (runId: string, marker: PendingDeliveryMarker, run?: ManagedRun, persisted?: PersistedRunState) => {
    if (inFlightDeliveries.has(runId)) return;
    // A live run re-persists its marker; a disk-only run only needs a missing id.
    const identified = run ? markRunPending(run, marker) : markerWithId(marker);
    const saved = run
      ? persistRunPending(manager, run, "retry")
      : identified === marker
        ? "saved"
        : persistDiskMarker(manager, runId, identified);
    if (saved === "deleted") {
      abandonDeletedDelivery(runId, run);
      return;
    }
    if (saved === "stale") {
      // A newer execution elsewhere owns the record's delivery; the disk pass
      // below delivers that one from the record.
      if (run) run.pendingDelivery = undefined;
      return;
    }
    if (saved === "failed") {
      if (run) retryLater(manager, runId, sessionId, endpoint.generation);
      else warnDelivery(sessionId, `Workflow ${runId}: delivery deferred because its marker could not be saved.`);
      return;
    }
    const deliveryId = identified.deliveryId;
    if (!deliveryId) return;
    const content = renderPendingText?.(manager, runId, identified, endpoint.loadSettings, run, persisted);
    if (content === undefined) return;
    deliverAndAck(manager, runId, sessionId, content, deliveryId, run);
  };

  // Live in-memory runs this session owns by their record. A run nobody owns is
  // claimable only from this manager's own live runs (completions before the
  // first bind), never from a foreign manager.
  // One failing run must not keep the others from going out.
  const tryEach = (deliver: () => void) => {
    try {
      deliver();
    } catch {
      // A run another process just deleted, or whose record is unreadable, is skipped.
    }
  };
  for (const run of manager.listLiveRuns()) {
    if (!run.pendingDelivery) continue;
    tryEach(() => {
      if (claimRunSession(manager.getPersistence(), run, sessionId) !== sessionId) return;
      if (run.pendingDelivery) tryOne(run.runId, run.pendingDelivery, run);
    });
  }

  // Disk runs, including terminal runs already evicted from memory. Require an
  // exact sessionId match: claiming null-sessionId rows would race a second
  // manager on the same cwd. Session handoff re-homes earlier pendings first.
  try {
    for (const state of manager.getPersistence().list()) {
      if (!state.pendingDelivery) continue;
      if (state.sessionId !== sessionId) continue;
      // Skip if the live copy still carries the marker — the loop above owns it.
      const live = manager.getRun(state.runId);
      if (live?.pendingDelivery) continue;
      // Only an execution holding the lease is newer than the record. A settled
      // copy may predate a resume another process ran, so the record renders.
      const pending = state.pendingDelivery;
      tryEach(() => tryOne(state.runId, pending, live?.lease ? live : undefined, state));
    }
  } catch {
    // best-effort
  }
}

/** @internal test helper — drop every in-flight lock. */
export function _resetDeliveryDispatchForTests(): void {
  inFlightDeliveries.clear();
  inFlightSeq = 0;
}
