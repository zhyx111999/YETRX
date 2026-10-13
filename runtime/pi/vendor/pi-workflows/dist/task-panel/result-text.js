/** Wording and size limits for the messages delivered when a background run finishes, fails, or pauses. */
/** Default cap on the JSON-dump fallback in a delivered result summary. Overridable
 *  via the `deliveredResultMaxChars` setting in ~/.pi/workflows/settings.json. */
const DEFAULT_DELIVERED_MAX_CHARS = 400;
const CHECKPOINT_PAYLOAD_PREVIEW_CHARS = 500;
/**
 * Pick a clean human-readable summary from a workflow result, in order of
 * preference: a `verdict`/`report`/`summary`/`synthesis` string field, a bare
 * string result, else a JSON dump capped at `maxChars`. When the dump is truncated
 * the dropped size is reported; the full result stays reachable through the
 * "Full result" pointer the delivered message appends.
 */
export function summarizeResult(result, maxChars = DEFAULT_DELIVERED_MAX_CHARS) {
    if (typeof result === "string")
        return result;
    if (result == null)
        return "null";
    if (typeof result === "object") {
        const obj = result;
        for (const key of ["verdict", "report", "summary", "synthesis"]) {
            const val = obj[key];
            if (typeof val === "string" && val.trim())
                return val;
        }
    }
    const json = JSON.stringify(result, null, 2);
    if (json.length <= maxChars)
        return json;
    // Slice once (the kept head); derive the dropped size by byte-length subtraction
    // so we don't also allocate the (potentially large) truncated tail to measure it.
    const kept = json.slice(0, maxChars);
    const droppedBytes = Buffer.byteLength(json, "utf8") - Buffer.byteLength(kept, "utf8");
    return `${kept}\n…(truncated ${formatBytes(droppedBytes)})`;
}
/** Absolute path to a run's exported result JSON. Undefined if the export
 *  fails — delivery must never throw in the complete handler. */
export function persistedResultPath(manager, runId, result) {
    try {
        return manager.getPersistence().exportResult(runId, result);
    }
    catch {
        return undefined;
    }
}
/** Delivered JSON-dump truncation threshold from settings (already normalized),
 *  defaulting to 400 when unset or unreadable. */
export function deliveredMaxChars(loadSettings) {
    try {
        return loadSettings?.().deliveredResultMaxChars ?? DEFAULT_DELIVERED_MAX_CHARS;
    }
    catch {
        return DEFAULT_DELIVERED_MAX_CHARS;
    }
}
export function failedRunText(runId, error) {
    return `✗ Background workflow ${runId} failed: ${error?.message ?? "unknown error"}`;
}
/** The delivered notice for a pause the model must act on, or undefined for a manual pause. */
export function pausedRunText({ runId, reason, error, resetHint, checkpoint }) {
    if (reason === "usage_limit") {
        const when = resetHint ? ` (${resetHint})` : "";
        return (`⏸ Subagents run ${runId} paused: ${error?.message ?? "provider usage limit reached"}${when}. ` +
            `Finished work is saved — continue it with subagents_control action "resume", runId "${runId}" once the limit resets.`);
    }
    if (reason === "workflow_checkpoint" && checkpoint) {
        const payload = JSON.stringify(checkpoint.payload) ?? "undefined";
        const preview = payload.length > CHECKPOINT_PAYLOAD_PREVIEW_CHARS
            ? `${payload.slice(0, CHECKPOINT_PAYLOAD_PREVIEW_CHARS)}…`
            : payload;
        return (`⏸ Subagents run ${runId} is waiting at checkpoint "${checkpoint.checkpointId}" (${checkpoint.kind}). Payload: ${preview}
` +
            `Answer it with subagents_control action "resume", runId "${runId}", checkpointId "${checkpoint.checkpointId}", and your response.`);
    }
    return undefined;
}
/** Human-readable byte size for the dropped-tail hint: 512 B, 3.2 KB, 1.4 MB. */
function formatBytes(n) {
    if (n < 1024)
        return `${n} B`;
    if (n < 1024 * 1024)
        return `${(n / 1024).toFixed(1)} KB`;
    return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}
