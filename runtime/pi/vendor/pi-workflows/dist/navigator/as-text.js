/** Coerces values from untrusted run records into strings that are safe to measure and render. */
/**
 * Persisted runs can be corrupt, and model-written scripts are never type
 * checked, so labels, phases, prompts, and names may arrive as non-strings.
 * pi-tui's width helpers call string methods on them, which would crash the
 * whole overlay; every value that reaches the renderer goes through here.
 */
export function asText(v) {
    return typeof v === "string" ? v : String(v ?? "");
}
