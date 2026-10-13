/** Turns host-supplied run settings into the bounded limits the runtime enforces, including the concurrency gate. */

import { DEFAULT_AGENT_TIMEOUT_MS, MAX_AGENT_RETRIES, MAX_CONCURRENCY } from "../config.js";
import type { WorkflowRunOptions } from "../workflow.js";

/** Largest delay setTimeout accepts; larger values fire immediately. */
const MAX_TIMER_MS = 2_147_483_647;
const DEFAULT_DRAIN_ABORT_GRACE_MS = 10_000;
/** Retry backoff cap: a long park would ignore pause/stop for its whole duration. */
const MAX_RETRY_BACKOFF_MS = 2_000;

/**
 * Per-attempt timeout. An embedding host's invalid value must not fail the run
 * (or spawn and instantly abort sessions), so it falls back to the default and logs.
 */
export function resolveAgentTimeoutMs(options: Pick<WorkflowRunOptions, "agentTimeoutMs" | "onLog">): number | null {
  const timeoutMs = options.agentTimeoutMs !== undefined ? options.agentTimeoutMs : DEFAULT_AGENT_TIMEOUT_MS;
  if (
    timeoutMs !== null &&
    (typeof timeoutMs !== "number" || !Number.isFinite(timeoutMs) || timeoutMs < 1 || timeoutMs > MAX_TIMER_MS)
  ) {
    options.onLog?.(
      `ignoring invalid agentTimeoutMs (${String(options.agentTimeoutMs)}); using the configured default instead`,
    );
    return DEFAULT_AGENT_TIMEOUT_MS;
  }
  return timeoutMs;
}

/** Concurrent agents, clamped to 1..MAX_CONCURRENCY; defaults to the core count minus two. */
export function resolveConcurrency(value: number | undefined): number {
  const requested = value ?? Math.max(1, (globalThis.navigator?.hardwareConcurrency ?? 8) - 2);
  if (typeof requested !== "number" || !Number.isFinite(requested) || requested < 1) return 1;
  return Math.min(MAX_CONCURRENCY, Math.floor(requested));
}

/** Retries after a recoverable failure, clamped to 0..MAX_AGENT_RETRIES. */
export function normalizeAgentRetries(value: unknown): number {
  if (typeof value !== "number" || !Number.isFinite(value) || value < 0) return 0;
  return Math.min(MAX_AGENT_RETRIES, Math.floor(value));
}

/** Grace for a runner told to stop (aborted drain, timed-out attempt): Infinity waits forever; NaN, 0, negatives and overflow use the default. */
export function resolveDrainAbortGraceMs(value: number | undefined): number {
  if (value === undefined) return DEFAULT_DRAIN_ABORT_GRACE_MS;
  if (value === Number.POSITIVE_INFINITY) return Number.POSITIVE_INFINITY;
  return typeof value === "number" && value >= 1 && value <= MAX_TIMER_MS
    ? Math.floor(value)
    : DEFAULT_DRAIN_ABORT_GRACE_MS;
}

/**
 * Wait before retrying after `failedAttempt`. Immediate retries let a whole
 * parallel() batch hammer the provider together, so the default backs off
 * exponentially. A host override of 0 disables the wait; negative, non-finite
 * or throwing overrides use the default.
 */
export function retryBackoffMs(
  failedAttempt: number,
  options: Pick<WorkflowRunOptions, "agentRetryBackoffMs">,
): number {
  const defaultMs = Math.min(250 * 2 ** (failedAttempt - 1), MAX_RETRY_BACKOFF_MS);
  if (!options.agentRetryBackoffMs) return defaultMs;
  try {
    const requested = options.agentRetryBackoffMs(failedAttempt);
    if (requested === 0) return 0;
    return typeof requested === "number" && Number.isFinite(requested) && requested > 0
      ? Math.min(requested, MAX_RETRY_BACKOFF_MS)
      : defaultMs;
  } catch {
    return defaultMs;
  }
}

/** Run at most `limit` tasks at once; later callers queue in arrival order. */
export function createLimiter(limit: number) {
  let active = 0;
  const queue: Array<() => void> = [];
  const next = () => {
    active--;
    queue.shift()?.();
  };
  return async <T>(fn: () => Promise<T>): Promise<T> => {
    if (active >= limit) await new Promise<void>((resolve) => queue.push(resolve));
    active++;
    try {
      return await fn();
    } finally {
      next();
    }
  };
}
