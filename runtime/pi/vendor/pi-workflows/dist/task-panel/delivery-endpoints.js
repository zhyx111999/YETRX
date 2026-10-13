/** Process-wide registry of per-session delivery endpoints, and which session owns a run's result. */
import { refreshRunSession } from "../manager/session-owner.js";
/** One live endpoint per pi session id. */
export const sessionEndpoints = new Map();
/** Report the first actionable delivery problem for a session through its UI-safe channel. */
export function warnDelivery(sessionId, message) {
    const endpoint = sessionId ? sessionEndpoints.get(sessionId) : undefined;
    if (!endpoint || endpoint.warned || !endpoint.reportWarning)
        return;
    endpoint.warned = true;
    try {
        endpoint.reportWarning(message);
    }
    catch {
        // Diagnostics must never affect delivery or write raw stderr over the TUI.
    }
}
/**
 * The session that receives a run's result: its record's owner, read now
 * because another process may have moved it since this copy last wrote. The
 * manager binding covers a run nobody owns yet. Never invents one.
 */
export function resolveDeliverySessionId(run, manager) {
    return refreshRunSession(manager.getPersistence(), run) ?? manager.getSessionId();
}
