// Decides how a finished execution is reported: failure cause, final status, empty-result warning.

import { emptyFleetSummary } from "../display.js";
import {
  errorMessage,
  isProviderUsageLimit,
  WorkflowCheckpointSuspensionError,
  WorkflowError,
  WorkflowErrorCode,
  workflowAbortedError,
} from "../errors.js";
import type { ManagedRun } from "../workflow-manager.js";

/**
 * The escaped error as a WorkflowError. A checkpoint suspension during an abort
 * is just the abort. Any other foreign error is an abort only when the run was
 * aborted; otherwise it is a failure of unknown cause (a throwing toolset
 * factory, a script bug), which a resume may still get past.
 */
function asWorkflowError(error: unknown, aborted: boolean): WorkflowError {
  if (error instanceof WorkflowCheckpointSuspensionError && aborted) return workflowAbortedError();
  if (error instanceof WorkflowError) return error;
  const message = errorMessage(error);
  const code = aborted ? WorkflowErrorCode.WORKFLOW_ABORTED : WorkflowErrorCode.UNKNOWN;
  return new WorkflowError(message, code, { recoverable: true });
}

/**
 * The user pause()/stop() that aborted this execution, if one did. Its action
 * decides how the execution settles, whatever the script did afterwards.
 */
export function controllingLifecycleAction(managed: ManagedRun): "pause" | "stop" | undefined {
  const control = managed.lifecycleControl;
  return control && managed.controller.signal.reason === control.abortReason ? control.action : undefined;
}

/** How a failed execution settles its run. */
export interface FailedExecution {
  /** The error recorded on the run and rethrown to whoever awaits the execution. */
  error: WorkflowError;
  /** The new status; undefined keeps the one pause()/stop() already set. */
  status?: "paused" | "aborted" | "failed";
  /** A provider usage limit pauses the run so it can resume automatically. */
  usageLimitPaused: boolean;
  /** pause()/stop() already announced this outcome, so no error event follows. */
  announced: boolean;
}

/**
 * Classify an error that escaped runWorkflow. Several causes can race: a
 * provider limit, a user pause()/stop(), a real failure.
 * Each abort carries the reason object of whoever caused it, so the first
 * cause decides; a later one never relabels it.
 */
export function classifyFailure(managed: ManagedRun, error: unknown): FailedExecution {
  const signal = managed.controller.signal;
  const workflowError = asWorkflowError(error, signal.aborted);
  const usageLimitPaused =
    isProviderUsageLimit(workflowError) &&
    (managed.usageLimitEscapedBeforeLifecycleControl === workflowError ||
      (!signal.aborted && managed.lifecycleControl === undefined));
  const lifecycleControlOwnsExecution = controllingLifecycleAction(managed) !== undefined;
  // pause()/stop() already reported its own AbortError, and a provider result
  // that arrives after it must not revive a stop or arm quota auto-resume.
  const announced =
    lifecycleControlOwnsExecution &&
    (workflowError.code === WorkflowErrorCode.WORKFLOW_ABORTED ||
      (isProviderUsageLimit(workflowError) && !usageLimitPaused));

  let status: FailedExecution["status"];
  if (usageLimitPaused) {
    // A limit that escaped before a later pause()/stop() stays a quota pause.
    status = "paused";
  } else if (announced) {
    status = undefined;
  } else if (signal.aborted && workflowError.code === WorkflowErrorCode.WORKFLOW_ABORTED) {
    status = "aborted";
  } else {
    // A real failure wins even when a pause()/stop() arrived after it escaped.
    status = "failed";
  }
  return { error: workflowError, status, usageLimitPaused, announced };
}

/**
 * A warning when every agent of this execution returned nothing. agent()
 * resolves an exhausted recoverable failure (e.g. an empty response) to null,
 * so such a run still completes; without the warning it looks successful.
 * A call replayed from the journal took part and counts with its result; a
 * seeded row no call of this execution reused is history and does not.
 */
export function emptyFleetWarning(managed: ManagedRun): string | undefined {
  const fleet = emptyFleetSummary(managed.snapshot.agents.filter((agent) => managed.executionAgentRows.has(agent.id)));
  if (!fleet.allEmpty) return undefined;
  const labels = fleet.emptyLabels.join(", ");
  const overflow =
    fleet.emptyCount > fleet.emptyLabels.length ? ` (+${fleet.emptyCount - fleet.emptyLabels.length} more)` : "";
  return [
    `⚠  Workflow produced no usable results: all ${fleet.emptyCount} agent(s) returned nothing (${labels}${overflow}).`,
    `   agent() resolves recoverable failures (e.g. an empty model response) to null once retries are exhausted, so the run reports "completed" even though nothing was produced.`,
    `   Common fixes: set defaultAgentRetries to 1-2 in the workflow settings for models that occasionally return empty, check the model's max output tokens, and read the per-agent errors above.`,
  ].join("\n");
}
