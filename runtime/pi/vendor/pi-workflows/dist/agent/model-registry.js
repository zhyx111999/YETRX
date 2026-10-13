// Builds the disk-backed fallback ModelRegistry and reads a registry's backing runtime.
import { ModelRegistry, ModelRuntime } from "@earendil-works/pi-coding-agent";
// Built lazily on first use; synchronous readers see undefined until it resolves.
let fallbackRegistryPromise;
let fallbackRegistry;
/** The fallback registry once it has been built, else undefined. */
export function builtFallbackRegistry() {
    return fallbackRegistry;
}
/**
 * Build (once per process) a registry from the same auth.json/models.json that
 * createAgentSession uses by default, so a model resolved here carries valid
 * credentials.
 */
export function ensureFallbackRegistry() {
    if (!fallbackRegistryPromise) {
        fallbackRegistryPromise = ModelRuntime.create().then((runtime) => new ModelRegistry(runtime));
        // Don't cache a rejection: a transient failure (e.g. auth.json lock) would
        // otherwise wedge the fallback for the rest of the process.
        fallbackRegistryPromise.catch(() => {
            fallbackRegistryPromise = undefined;
        });
    }
    return fallbackRegistryPromise.then((registry) => {
        fallbackRegistry ??= registry;
        return registry;
    });
}
/**
 * The ModelRuntime behind a registry. The registry keeps it private and has no
 * getter, but a subagent session and the SDK's model resolver need it to share
 * the host session's exact catalog and auth.
 */
export function runtimeOf(registry) {
    return registry.runtime;
}
