// Checkpoint responses: attaching a reply to a waiting checkpoint, and checking a resume past one.

import { isDeepStrictEqual } from "node:util";
import type { PersistedRunState } from "../run-persistence.js";
import { cloneDurableJsonValue } from "../runtime/durable-json.js";
import type { WorkflowCheckpoint } from "../workflow.js";
import type { ManagedRun } from "../workflow-manager.js";
import { waitForPausedExecutionSettlement } from "./execution-settlement.js";
import type { LiveRuns } from "./live-runs.js";

function staleCheckpointError(expected: string, received: string): Error {
  return new Error(
    `stale checkpoint response: expected ${JSON.stringify(expected)}, received ${JSON.stringify(received)}`,
  );
}

/**
 * The checkpoint with `response` attached, or undefined when that same response
 * is already attached. Throws for another checkpoint or a conflicting response.
 */
function withCheckpointResponse(
  checkpoint: WorkflowCheckpoint,
  checkpointId: string,
  response: unknown,
): WorkflowCheckpoint | undefined {
  if (checkpoint.checkpointId !== checkpointId) throw staleCheckpointError(checkpoint.checkpointId, checkpointId);
  if (checkpoint.status !== "waiting") {
    if (isDeepStrictEqual(checkpoint.response, response)) return undefined;
    throw new Error(`conflicting response for checkpoint ${JSON.stringify(checkpointId)}`);
  }
  return { ...checkpoint, status: "resuming", response };
}

/**
 * Attach a response to a waiting checkpoint; it is on disk before this
 * resolves, so it survives a restart. While the suspended execution still
 * waits for its other agents, the response is written through the live run,
 * whose lease that execution holds; otherwise through the record under a
 * fresh lease.
 */
export async function attachCheckpointResponse(
  live: LiveRuns,
  executionOf: (run: ManagedRun) => Promise<unknown> | undefined,
  runId: string,
  checkpointId: string,
  responseValue: unknown,
): Promise<void> {
  const response = cloneDurableJsonValue(responseValue, "checkpoint response");
  const active = live.get(runId);
  const settlingExecution = active ? executionOf(active) : undefined;
  // A .then on a settled promise runs within one microtask; on a pending one
  // it does not. That tells a still-draining execution from a settled one.
  let executionPending = false;
  if (settlingExecution) {
    let settled = false;
    void settlingExecution.then(
      () => (settled = true),
      () => (settled = true),
    );
    await Promise.resolve();
    executionPending = !settled;
  }
  if (settlingExecution && executionPending) {
    // The draining execution writes the run again after the drain and reads
    // the live checkpoint then, so the response reaches disk with it.
    if (active?.lease && live.isCurrent(active)) {
      if (!active.checkpoint) throw new Error("run has no durable checkpoint");
      const next = withCheckpointResponse(active.checkpoint, checkpointId, response);
      if (next === undefined) return;
      const previousCheckpoint = active.checkpoint;
      active.checkpoint = next;
      try {
        // Also write now, so the response survives a crash during the drain.
        live.persistRun(active, true);
      } catch (error) {
        // The caller is told the attach failed, so the live run must not keep it.
        active.checkpoint = previousCheckpoint;
        throw error;
      }
      return;
    }
    if (!(await waitForPausedExecutionSettlement(settlingExecution))) {
      throw new Error(`workflow run ${JSON.stringify(runId)} is still settling`);
    }
  }

  const persistence = live.persistence;
  const lease = persistence.acquireRunLease(runId);
  if (!lease) throw new Error(`workflow run ${JSON.stringify(runId)} is busy`);
  try {
    const persisted = persistence.load(runId);
    if (!persisted?.checkpoint) throw new Error("run has no durable checkpoint");
    const next = withCheckpointResponse(persisted.checkpoint, checkpointId, response);
    if (next === undefined) return;
    persistence.save({ ...persisted, checkpoint: next });
    if (active && live.isCurrent(active)) {
      active.checkpoint = next;
    }
  } finally {
    persistence.releaseRunLease(lease);
  }
}

/**
 * Check that a resume may continue past the run's checkpoint. A run waiting at
 * a checkpoint resumes only with its exact ID and an attached response. Returns
 * true when that checkpoint was already consumed by a completed run, so there
 * is nothing left to resume.
 */
export function checkpointAlreadyConsumed(persisted: PersistedRunState, checkpointId: string | undefined): boolean {
  const checkpoint = persisted.checkpoint;
  if (checkpointId !== undefined) {
    if (!checkpoint) throw new Error("run has no durable checkpoint");
    if (checkpoint.checkpointId !== checkpointId) throw staleCheckpointError(checkpoint.checkpointId, checkpointId);
    if (checkpoint.status === "waiting") {
      throw new Error(`checkpoint ${JSON.stringify(checkpointId)} has no attached response`);
    }
    return checkpoint.status === "consumed" && persisted.status === "completed";
  }
  if (checkpoint?.status === "waiting" || checkpoint?.status === "resuming") {
    throw new Error(
      `run is waiting at checkpoint ${JSON.stringify(checkpoint.checkpointId)}; resume requires its exact checkpoint ID`,
    );
  }
  return false;
}
