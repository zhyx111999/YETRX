/** Bounded backoff retries for results whose send or marker clear did not go through. */

import { type SessionDeliveryEndpoint, sessionEndpoints, warnDelivery } from "./delivery-endpoints.js";

const DELIVERY_RETRY_DELAYS_MS = [250, 1_000, 4_000] as const;

type DeliveryRetry = {
  attempt: number;
  generation: number;
  sessionId: string;
  timer?: ReturnType<typeof setTimeout>;
};

const deliveryRetries = new Map<string, DeliveryRetry>();

/**
 * Retry a run's delivery on the same endpoint generation after the next backoff
 * delay. `flush` runs only if that endpoint is still live and unsuspended; a
 * rebind starts its own flush, so retries never cross generations.
 */
export function scheduleDeliveryRetry(
  runId: string,
  sessionId: string,
  generation: number,
  flush: (endpoint: SessionDeliveryEndpoint) => void,
): void {
  const current = deliveryRetries.get(runId);
  if (current?.timer && current.generation === generation) return;
  const attempt = current?.generation === generation ? current.attempt : 0;
  if (attempt >= DELIVERY_RETRY_DELAYS_MS.length) {
    warnDelivery(
      sessionId,
      `Workflow ${runId}: result remains pending after delivery retries. Open /workflows to inspect it.`,
    );
    return;
  }

  const retry: DeliveryRetry = { attempt: attempt + 1, generation, sessionId };
  retry.timer = setTimeout(() => {
    retry.timer = undefined;
    if (deliveryRetries.get(runId) !== retry) return;
    const endpoint = sessionEndpoints.get(sessionId);
    if (!endpoint || endpoint.suspended || endpoint.generation !== generation) return;
    flush(endpoint);
  }, DELIVERY_RETRY_DELAYS_MS[attempt]);
  retry.timer.unref?.();
  deliveryRetries.set(runId, retry);
}

export function cancelDeliveryRetry(runId: string): void {
  const retry = deliveryRetries.get(runId);
  if (retry?.timer) clearTimeout(retry.timer);
  deliveryRetries.delete(runId);
}

export function cancelSessionRetries(sessionId: string): void {
  for (const [runId, retry] of deliveryRetries) {
    if (retry.sessionId === sessionId) cancelDeliveryRetry(runId);
  }
}

/** @internal test helper — stop every pending retry timer. */
export function _resetDeliveryRetriesForTests(): void {
  for (const retry of deliveryRetries.values()) if (retry.timer) clearTimeout(retry.timer);
  deliveryRetries.clear();
}
