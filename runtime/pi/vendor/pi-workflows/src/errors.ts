/**
 * Workflow-specific error types.
 */

/** Dependency-neutral diagnostic payload retained by capability contract failures. */
export interface CapabilityErrorDiagnostic {
  code: string;
  severity: "error" | "warning";
  subject: string;
  message: string;
}

/** Stable runtime failure codes exposed to callers and UI surfaces. */
export enum WorkflowErrorCode {
  /** Agent exceeded timeout. */
  AGENT_TIMEOUT = "AGENT_TIMEOUT",
  /** Workflow was aborted by user. */
  WORKFLOW_ABORTED = "WORKFLOW_ABORTED",
  /** Agent limit exceeded. */
  AGENT_LIMIT_EXCEEDED = "AGENT_LIMIT_EXCEEDED",
  /**
   * The provider's subscription/usage/quota/rate limit was hit. A provider limit
   * refills on its own, so the run is checkpointed (paused) and replayed by
   * resume() rather than failed.
   */
  PROVIDER_USAGE_LIMIT = "PROVIDER_USAGE_LIMIT",
  /** Script validation failed. */
  SCRIPT_VALIDATION_ERROR = "SCRIPT_VALIDATION_ERROR",
  /** A schema agent never produced valid structured_output (after repair + extraction). */
  SCHEMA_NONCOMPLIANCE = "SCHEMA_NONCOMPLIANCE",
  /** A non-schema agent completed without any assistant text output. */
  AGENT_EMPTY_OUTPUT = "AGENT_EMPTY_OUTPUT",
  /**
   * An agent()'s `model`/`tier` spec did not resolve to any known model. Never
   * silently substituted for the session default — resolution is deterministic,
   * so retrying the same spec would fail identically every time.
   */
  MODEL_NOT_FOUND = "MODEL_NOT_FOUND",
  /** Agent execution failed. */
  AGENT_EXECUTION_ERROR = "AGENT_EXECUTION_ERROR",
  /** Unknown error. */
  UNKNOWN = "UNKNOWN",
}

/** Classified workflow failure with recoverability and optional agent/provider context. */
export class WorkflowError extends Error {
  readonly code: WorkflowErrorCode;
  readonly recoverable: boolean;
  readonly agentLabel?: string;
  readonly details?: unknown;
  /** For PROVIDER_USAGE_LIMIT: the provider's human reset hint, e.g. "Resets in ~3h" (verbatim). */
  readonly resetHint?: string;

  constructor(
    message: string,
    code: WorkflowErrorCode,
    options: { recoverable?: boolean; agentLabel?: string; details?: unknown; resetHint?: string } = {},
  ) {
    super(message);
    this.name = "WorkflowError";
    this.code = code;
    this.recoverable = options.recoverable ?? false;
    this.agentLabel = options.agentLabel;
    this.details = options.details;
    this.resetHint = options.resetHint;
  }
}

/** Internal control signal used to suspend a run at a durable workflow checkpoint. */
export class WorkflowCheckpointSuspensionError extends Error {
  readonly checkpointId: string;

  constructor(checkpointId: string) {
    super(`workflow checkpoint ${JSON.stringify(checkpointId)} is waiting for a response`);
    this.name = "WorkflowCheckpointSuspensionError";
    this.checkpointId = checkpointId;
  }
}

/**
 * A run record this process has written or read was removed by another process.
 * Deletion is authoritative, so a stale writer must not recreate the record.
 */
export class RunRecordDeletedError extends Error {
  readonly runId: string;

  constructor(runId: string) {
    super(`workflow run ${JSON.stringify(runId)} was deleted`);
    this.name = "RunRecordDeletedError";
    this.runId = runId;
  }
}

/**
 * A run's record files exist but cannot be read: a damaged head and backup, or
 * a log that does not match its committed head. The files are left as they are
 * for diagnosis, and the run is never treated as missing, which would let a
 * later write replace it.
 */
export class RunRecordUnreadableError extends Error {
  readonly path: string;

  constructor(path: string, reason: string) {
    super(`workflow run record ${path} cannot be read: ${reason}`);
    this.name = "RunRecordUnreadableError";
    this.path = path;
  }
}

/** Contract failure that retains every definition or assembly diagnostic. */
export class WorkflowCapabilityContractError extends Error {
  readonly diagnostics: readonly CapabilityErrorDiagnostic[];

  constructor(message: string, diagnostics: readonly CapabilityErrorDiagnostic[]) {
    super(message);
    this.name = "WorkflowCapabilityContractError";
    this.diagnostics = diagnostics;
  }
}

/** The error of a workflow stopped by an abort. Recoverable: its journal can resume. */
export function workflowAbortedError(): WorkflowError {
  return new WorkflowError("workflow aborted", WorkflowErrorCode.WORKFLOW_ABORTED, { recoverable: true });
}

/** Narrow an unknown failure to WorkflowError. */
export function isWorkflowError(error: unknown): error is WorkflowError {
  return error instanceof WorkflowError;
}

/**
 * The text of an unknown failure, for messages and logs. Never throws: a thrown
 * value can be anything, including an object whose conversion to text throws.
 */
export function errorMessage(error: unknown): string {
  try {
    return error instanceof Error ? error.message : String(error);
  } catch {
    return "unprintable error";
  }
}

/** Report whether an unknown failure is a provider usage-limit checkpoint condition. */
export function isProviderUsageLimit(error: unknown): error is WorkflowError {
  return isWorkflowError(error) && error.code === WorkflowErrorCode.PROVIDER_USAGE_LIMIT;
}

/**
 * Detect a provider subscription/usage/quota/rate-limit exhaustion from free-form
 * error text, and extract the provider's human reset hint when present.
 *
 * The pi SDK does NOT throw these — it records them as an assistant message with
 * stopReason "error" and an errorMessage like "Codex usage limit reached (plus
 * plan). Resets in ~3h.". Callers reading message metadata MUST gate on
 * stopReason === "error" before trusting this, so a task whose own output merely
 * mentions "rate limit" is never misclassified. Patterns mirror the SDK's own
 * non-retryable-limit table. Deliberately excludes transient overloaded/5xx
 * errors, which stay recoverable and keep retrying.
 */
export function classifyProviderLimit(text: string | undefined): { matched: boolean; resetHint?: string } {
  if (!text) return { matched: false };
  const matched =
    /usage limit|limit reached|insufficient[_\s]?quota|quota exceeded|exceeded your current quota|out of budget|available balance|\bquota\b|rate.?limit|too many requests|\b429\b|GoUsageLimitError|FreeUsageLimitError|\bbilling\b/i.test(
      text,
    );
  if (!matched) return { matched: false };
  // "Resets in ~3h", "reset at 2026-09-17 13:20:54 +0800", and the pi-ai
  // Codex form "Try again in ~299 min" reach the scheduler verbatim;
  // parseResetHintMs understands all three shapes.
  const reset = text.match(/(?:resets?\s+(?:in|at)|try again\s+in)\s+[^.\n]+/i);
  return { matched: true, resetHint: reset?.[0]?.trim() };
}

/** Recognize abort-like Error messages without assuming a provider-specific class. */
export function isAbortError(error: unknown): boolean {
  if (!(error instanceof Error)) return false;
  return /\babort(?:ed)?\b/i.test(error.message);
}

/** Recognize timeout-like errors by name or message. */
export function isTimeoutError(error: unknown): boolean {
  if (!(error instanceof Error)) return false;
  return /\btimeout\b/i.test(error.message) || error.name === "TimeoutError";
}

/**
 * Wrap an unknown error into a WorkflowError with appropriate classification.
 */
export function wrapError(error: unknown, context?: { agentLabel?: string }): WorkflowError {
  if (isWorkflowError(error)) return error;

  if (isAbortError(error)) {
    return new WorkflowError(
      error instanceof Error ? error.message : "Workflow was aborted",
      WorkflowErrorCode.WORKFLOW_ABORTED,
      { recoverable: true },
    );
  }

  if (isTimeoutError(error)) {
    return new WorkflowError(
      error instanceof Error ? error.message : "Agent timed out",
      WorkflowErrorCode.AGENT_TIMEOUT,
      { recoverable: true, agentLabel: context?.agentLabel },
    );
  }

  // Defense-in-depth: today the SDK buries provider usage/quota limits in an
  // assistant message (detected in agent/provider-limit.ts), but a future SDK might throw them.
  // Classify a thrown limit here too — recoverable:false so the run checkpoints
  // (paused) instead of being retried into the same wall or silently nulled.
  if (error instanceof Error) {
    const limit = classifyProviderLimit(error.message);
    if (limit.matched) {
      return new WorkflowError(error.message, WorkflowErrorCode.PROVIDER_USAGE_LIMIT, {
        recoverable: false,
        agentLabel: context?.agentLabel,
        resetHint: limit.resetHint,
      });
    }
  }

  return new WorkflowError(errorMessage(error), WorkflowErrorCode.AGENT_EXECUTION_ERROR, {
    recoverable: true,
    agentLabel: context?.agentLabel,
    details: error,
  });
}
