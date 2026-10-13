import { createAgentSession, SettingsManager, } from "@earendil-works/pi-coding-agent";
import { pinChildCacheRetention } from "../child-cache-retention.js";
import { runtimeOf } from "./model-registry.js";
export async function openChildSession(spec) {
    const { injected, modelRegistry } = spec;
    const { session } = await createAgentSession({
        cwd: spec.cwd,
        agentDir: spec.agentDir,
        sessionManager: spec.sessionManager,
        // A real SettingsManager inherits the user's default provider/model.
        // SettingsManager.inMemory() skips ~/.pi settings, so subagents would fall
        // back to the first available model, which may lack auth and answer empty.
        settingsManager: SettingsManager.create(spec.cwd, spec.agentDir),
        // An injected resourceLoader wins and skips the shared build entirely.
        resourceLoader: injected.resourceLoader ?? (await spec.sharedResourceLoader()),
        // The registry's backing runtime, so subagents share the host catalog and auth.
        modelRuntime: runtimeOf(modelRegistry),
        ...injected,
        ...(spec.cwdIsExplicit ? { cwd: spec.cwd } : {}),
        // The injected spread above would otherwise replace a thread's retained
        // manager, breaking turn continuity and failed-turn rollback.
        ...(spec.keepSessionManager ? { sessionManager: spec.sessionManager } : {}),
        ...(spec.model ? { model: spec.model } : {}),
        ...(spec.thinkingLevel ? { thinkingLevel: spec.thinkingLevel } : {}),
        // Last, so nothing injected widens the call's tools. Without an allowlist
        // Pi enables its built-in read/bash/edit/write next to the custom tools,
        // which would bypass an agent type's tool policy.
        customTools: spec.customTools,
        tools: spec.customTools.map((tool) => tool.name),
        excludeTools: spec.excludeTools,
    });
    pinChildCacheRetention(session.agent);
    const dispose = async () => {
        try {
            // dispose() alone does not emit shutdown; give opted-in extension
            // factories a chance to release session-local listeners and resources.
            await session.extensionRunner.emit({ type: "session_shutdown", reason: "quit" });
        }
        catch {
            // Cleanup must not replace the original result or failure.
        }
        finally {
            session.dispose();
        }
    };
    // Child sessions do not auto-bind a supplied ResourceLoader. Bind middleware
    // before the first provider request (also supports injected resource loaders).
    try {
        await session.bindExtensions({});
    }
    catch (error) {
        await dispose();
        throw error;
    }
    return { session, dispose };
}
