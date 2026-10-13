// Shows diagnostics that belong to no single run: storage, session ownership, and auto-resume problems.
import { errorMessage } from "./errors.js";
let sink;
/**
 * Route diagnostics to `next` until the returned release runs or a later bind
 * replaces it. Pi binds its session UI here: raw console output would draw
 * over the TUI.
 */
export function bindDiagnosticSink(next) {
    sink = next;
    return () => {
        if (sink === next)
            sink = undefined;
    };
}
/**
 * Report a diagnostic, with the failure that caused it when there is one.
 * Without a bound sink (the SDK, tests, print mode), or when the sink itself
 * fails because its session went stale, it goes to stderr instead.
 */
export function reportDiagnostic(message, cause) {
    const text = cause === undefined ? message : `${message}: ${errorMessage(cause)}`;
    const current = sink;
    if (current) {
        try {
            current(text);
            return;
        }
        catch {
            // A sink whose session ended; stderr still records it.
        }
    }
    console.warn(`[workflow] ${text}`);
}
