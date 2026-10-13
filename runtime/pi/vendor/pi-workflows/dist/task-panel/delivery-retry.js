/** Bounded backoff retries for results whose send or marker clear did not go through. */
import { sessionEndpoints, warnDelivery } from "./delivery-endpoints.js";
const DELIVERY_RETRY_DELAYS_MS = [250, 1_000, 4_000];
const deliveryRetries = new Map();
/**
 * Retry a run's delivery on the same endpoint generation after the next backoff
 * delay. `flush` runs only if that endpoint is still live and unsuspended; a
 * rebind starts its own flush, so retries never cross generations.
 */
export function scheduleDeliveryRetry(runId, sessionId, generation, flush) {
    const current = deliveryRetries.get(runId);
    if (current?.timer && current.generation === generation)
        return;
    const attempt = current?.generation === generation ? current.attempt : 0;
    if (attempt >= DELIVERY_RETRY_DELAYS_MS.length) {
        warnDelivery(sessionId, `Workflow ${runId}: result remains pending after delivery retries. Open /workflows to inspect it.`);
        return;
    }
    const retry = { attempt: attempt + 1, generation, sessionId };
    retry.timer = setTimeout(() => {
        retry.timer = undefined;
        if (deliveryRetries.get(runId) !== retry)
            return;
        const endpoint = sessionEndpoints.get(sessionId);
        if (!endpoint || endpoint.suspended || endpoint.generation !== generation)
            return;
        flush(endpoint);
    }, DELIVERY_RETRY_DELAYS_MS[attempt]);
    retry.timer.unref?.();
    deliveryRetries.set(runId, retry);
}
export function cancelDeliveryRetry(runId) {
    const retry = deliveryRetries.get(runId);
    if (retry?.timer)
        clearTimeout(retry.timer);
    deliveryRetries.delete(runId);
}
export function cancelSessionRetries(sessionId) {
    for (const [runId, retry] of deliveryRetries) {
        if (retry.sessionId === sessionId)
            cancelDeliveryRetry(runId);
    }
}
/** @internal test helper — stop every pending retry timer. */
export function _resetDeliveryRetriesForTests() {
    for (const retry of deliveryRetries.values())
        if (retry.timer)
            clearTimeout(retry.timer);
    deliveryRetries.clear();
}
