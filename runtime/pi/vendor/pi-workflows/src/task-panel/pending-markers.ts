/**
 * Durable pending-delivery markers: give each delivery a stable id, save the
 * marker before sending, and clear it only after the send is confirmed.
 */

import { randomUUID } from "node:crypto";
import { RunRecordDeletedError } from "../errors.js";
import type { PendingDeliveryMarker } from "../run-persistence.js";
import type { ManagedRun, WorkflowManager } from "../workflow-manager.js";
import { warnDelivery } from "./delivery-endpoints.js";
import { cancelDeliveryRetry } from "./delivery-retry.js";

/** Confirmed deliveries whose pending marker still needs to be cleared durably. */
export const deliveredAwaitingClear = new Map<string, { deliveryId: string; sessionId: string; generation: number }>();

export type ClearPendingResult = "cleared" | "stale" | "failed";

/**
 * Outcome of making a delivery marker durable before its send. "stale": the
 * record now carries another delivery, written by a newer execution elsewhere,
 * so this copy's marker is obsolete.
 */
export type PendingSaveResult = "saved" | "stale" | "deleted" | "failed";

/**
 * A marker with its delivery id. A retry passes the marker it is retrying and
 * keeps that id, so host history can deduplicate the resend. A new
 * notification passes a marker without one and always gets a fresh id: two
 * results of one run (say, a resume after an earlier completion) are two
 * deliveries even when they are of the same kind.
 */
export function markerWithId(marker: PendingDeliveryMarker): PendingDeliveryMarker {
  return marker.deliveryId ? marker : { ...marker, deliveryId: randomUUID() };
}

export function markRunPending(run: ManagedRun, marker: PendingDeliveryMarker): PendingDeliveryMarker {
  const identified = markerWithId(marker);
  if (run.pendingDelivery?.deliveryId !== identified.deliveryId) deliveredAwaitingClear.delete(run.runId);
  run.pendingDelivery = identified;
  return identified;
}

export function clearRunPending(
  manager: WorkflowManager,
  runId: string,
  deliveryId: string,
  run?: ManagedRun,
): ClearPendingResult {
  // Clear disk first. If that fails, retain the live marker so the delivered
  // message remains visible to the bounded clear retry instead of being lost.
  // A record deleted by another process has no marker left to clear.
  try {
    const live = run ?? manager.getRun(runId);
    if (live?.pendingDelivery?.deliveryId && live.pendingDelivery.deliveryId !== deliveryId) return "stale";

    const persistence = manager.getPersistence();
    // A refused clear on a record that still exists means another delivery owns it.
    if (
      !persistence.updateMetadata(runId, { pendingDelivery: undefined }, deliveryId) &&
      persistence.loadPreview(runId)
    )
      return "stale";
    if (live) live.pendingDelivery = undefined;
    return "cleared";
  } catch {
    warnDelivery(
      run?.sessionId ?? manager.getSessionId(),
      `Workflow ${runId}: result delivered, but its pending marker could not be cleared.`,
    );
    return "failed";
  }
}

/**
 * Make a live run's delivery marker durable before sending it. A run that has
 * never been written is seeded with a minimal record; persistence refuses to
 * recreate a record that another process deleted, which reports "deleted".
 *
 * A new notification comes from the execution this process just settled, so it
 * replaces whatever the record carries. A retry re-saves its marker only while
 * the record carries that same delivery or none: by then another process may
 * have resumed and finished the run, and its newer marker must not be
 * overwritten by this copy's older one.
 */
export function persistRunPending(
  manager: WorkflowManager,
  run: ManagedRun,
  mode: "notification" | "retry",
): PendingSaveResult {
  try {
    // Merge into an existing on-disk record so we don't clobber the
    // manager's richer write that follows the complete/error emit, nor its
    // session owner, which only a session move changes.
    const persistence = manager.getPersistence();
    const expected = mode === "retry" ? run.pendingDelivery?.deliveryId : undefined;
    if (persistence.updateMetadata(run.runId, { pendingDelivery: run.pendingDelivery }, expected)) return "saved";
    if (persistence.loadPreview(run.runId)) return "stale";
    if (run.pendingDelivery) {
      persistence.save({
        runId: run.runId,
        workflowName: run.snapshot.name,
        script: run.script ?? "",
        sessionId: run.sessionId,
        status: run.status,
        phases: run.snapshot.phases ?? [],
        agents: [],
        logs: run.snapshot.logs ?? [],
        result: run.result?.result,
        tokenUsage: run.result?.tokenUsage ?? run.snapshot.tokenUsage,
        durationMs: run.result?.durationMs,
        startedAt: run.startedAt?.toISOString?.() ?? new Date().toISOString(),
        updatedAt: new Date().toISOString(),
        pendingDelivery: run.pendingDelivery,
      });
    }
    return "saved";
  } catch (error) {
    if (error instanceof RunRecordDeletedError) return "deleted";
    warnDelivery(
      run.sessionId,
      `Workflow ${run.runId}: delivery deferred because its pending marker could not be saved.`,
    );
    return "failed";
  }
}

/** Give a disk-only pending run its delivery id. It never creates a record. */
export function persistDiskMarker(
  manager: WorkflowManager,
  runId: string,
  marker: PendingDeliveryMarker,
): PendingSaveResult {
  try {
    return manager.getPersistence().updateMetadata(runId, { pendingDelivery: marker }) ? "saved" : "deleted";
  } catch (error) {
    return error instanceof RunRecordDeletedError ? "deleted" : "failed";
  }
}

/** A run deleted by another process is no longer deliverable; drop its delivery state. */
export function abandonDeletedDelivery(runId: string, run?: ManagedRun): void {
  if (run) run.pendingDelivery = undefined;
  deliveredAwaitingClear.delete(runId);
  cancelDeliveryRetry(runId);
}

/** Forget a session's confirmed-but-uncleared deliveries; the disk markers stay for the next flush. */
export function forgetAwaitingClear(sessionId: string): void {
  for (const [runId, delivered] of deliveredAwaitingClear) {
    if (delivered.sessionId === sessionId) deliveredAwaitingClear.delete(runId);
  }
}
