import { closeSync, existsSync, openSync, readSync } from "node:fs";
import { resolve } from "node:path";
import { StringDecoder } from "node:string_decoder";
import { createCodingTools } from "@earendil-works/pi-coding-agent";
import { loadedBuildIsStale } from "./build-identity.js";
import { registerBuiltinWorkflows } from "./builtin-commands.js";
import { claimWorkflowRuntime, discardWorkflowRuntime, handoffWorkflowRuntime, pauseStrandedWorkflowRuntime, SESSION_REPLACEMENT_REASONS, WORKFLOW_EXTENSION_BUILD, } from "./extension-reload.js";
import { bindDiagnosticSink } from "./host-diagnostics.js";
import { stopRunWatchers } from "./run-watchers.js";
import { registerAllSavedWorkflows } from "./saved-commands.js";
import { createSubagentsControlTool } from "./subagents-control-tool.js";
import { createSubagentsTool } from "./subagents-tool.js";
import { bindSessionDelivery, dropSessionDelivery, installResultDelivery, installTaskPanel, suspendResultDelivery, } from "./task-panel.js";
import { UsageLimitScheduler } from "./usage-limit-scheduler.js";
import { createWebTools } from "./web-tools.js";
import { registerWorkflowCommands } from "./workflow-commands.js";
import { installParentPromptHook, registerWorkflowProgressCommands } from "./workflow-editor.js";
import { WorkflowManager } from "./workflow-manager.js";
import { workflowProjectKey } from "./workflow-paths.js";
import { createWorkflowStorage } from "./workflow-saved.js";
import { loadWorkflowSettings, saveWorkflowSettings, workflowRunLimits } from "./workflow-settings.js";
import { registerWorkflowModelsCommand } from "./workflows-models-command.js";
/**
 * Bound for the read-only session-header probe (first line only). Independent of
 * pi's own ~1MiB session scan — we only need the header and keep the read small.
 */
const SESSION_HEADER_SCAN_BYTES = 64 * 1024;
/**
 * Read-only probe of a session JSONL file's project cwd from its header line.
 * Does NOT call SessionManager.open() — that API creates directories, may rewrite
 * empty/legacy files, and loads the full history. Used on session_shutdown for
 * resume/fork destination checks. Unreadable / oversized / non-session files
 * return undefined; callers decide fail-closed vs allow based on the shutdown reason.
 */
export function sessionFileCwd(sessionFile) {
    if (!sessionFile || !existsSync(sessionFile))
        return undefined;
    let fd;
    try {
        fd = openSync(sessionFile, "r");
        const decoder = new StringDecoder("utf8");
        const buffer = Buffer.allocUnsafe(4096);
        const chunks = [];
        let scanned = 0;
        while (scanned < SESSION_HEADER_SCAN_BYTES) {
            const n = readSync(fd, buffer, 0, Math.min(buffer.length, SESSION_HEADER_SCAN_BYTES - scanned), null);
            if (n === 0) {
                chunks.push(decoder.end());
                break;
            }
            scanned += n;
            const chunk = decoder.write(buffer.subarray(0, n));
            const nl = chunk.indexOf("\n");
            if (nl !== -1) {
                chunks.push(chunk.slice(0, nl));
                break;
            }
            chunks.push(chunk);
        }
        const line = chunks.join("").trim();
        if (!line)
            return undefined;
        const entry = JSON.parse(line);
        if (entry.type !== "session" || typeof entry.cwd !== "string" || !entry.cwd)
            return undefined;
        return resolve(entry.cwd);
    }
    catch {
        return undefined;
    }
    finally {
        if (fd !== undefined) {
            try {
                closeSync(fd);
            }
            catch {
                // ignore close errors on the probe fd
            }
        }
    }
}
/**
 * Project identity is the storage key, not the path text: on Windows one
 * directory has many spellings (letter case, drive letter, `\\?\` prefix,
 * 8.3 names), and a false mismatch would pause live runs for nothing.
 */
function isSameProject(a, b) {
    return workflowProjectKey(a) === workflowProjectKey(b);
}
/**
 * Whether the next extension generation serves `projectCwd`, so the live
 * manager can be handed to it instead of paused.
 * - quit / unknown: nothing will claim the manager.
 * - resume: fail closed. Only a target session header that reads as this
 *   project counts; a missing or unreadable one must not carry the manager
 *   into another project.
 * - fork: Pi forks stay in the project and the new session file may not exist
 *   yet, so only a header that reads as another project refuses.
 * - reload / new: always the same project.
 */
function nextGenerationKeepsProject(event, projectCwd) {
    const reason = event?.reason;
    if (!reason || !SESSION_REPLACEMENT_REASONS.has(reason))
        return false;
    if (reason !== "resume" && reason !== "fork")
        return true;
    const targetCwd = sessionFileCwd(event?.targetSessionFile);
    if (targetCwd === undefined)
        return reason === "fork";
    return isSameProject(targetCwd, projectCwd);
}
function buildManagerOptions(cwd) {
    const settings = loadWorkflowSettings({ cwd });
    return {
        toolsets: {
            "web-research": () => [...createCodingTools(cwd), ...createWebTools()],
        },
        excludeSubagentTools: settings.excludeSubagentTools,
        providerMiddlewareExtensions: settings.providerMiddlewareExtensions,
        ...workflowRunLimits(settings),
        persistAgentSessions: settings.persistAgentSessions,
        inheritMainModel: settings.inheritMainModel,
    };
}
export default function extension(pi) {
    // Process-wide handoff slot (not cwd-keyed): the previous generation may have
    // been bound to ctx.cwd while this factory only sees process.cwd(). Claim
    // whatever is staged; session_start then keeps or rebuilds it based on the
    // true session project.
    const runtimeClaim = claimWorkflowRuntime();
    const previousRuntime = runtimeClaim.compatible;
    let pausedForMismatch = runtimeClaim.buildMismatch ? pauseStrandedWorkflowRuntime(runtimeClaim.buildMismatch) : 0;
    // Mutable host state. Tools/commands resolve through getters so a
    // session_start that discovers a cross-project cwd can replace the manager
    // without leaving closed-over references pointing at the source project.
    //
    // Pi does not pass the session cwd into the factory, so a fresh manager
    // starts from process.cwd() while a claimed one keeps the project it already
    // serves. The real session cwd arrives on session_start as ctx.cwd.
    // The manager's cwd is the one project path everything here uses.
    const startCwd = resolve(previousRuntime?.manager.getCwd() ?? process.cwd());
    let manager = previousRuntime?.manager ?? new WorkflowManager({ cwd: startCwd, ...buildManagerOptions(startCwd) });
    if (previousRuntime)
        manager.reconfigureAfterReload(buildManagerOptions(manager.getCwd()));
    let storage = createWorkflowStorage(manager.getCwd());
    const getManager = () => manager;
    const getCwd = () => manager.getCwd();
    const getStorage = () => storage;
    /** The live runtime as a handoff value. Releasing it drops the delivery of the session it serves now. */
    const liveRuntime = () => {
        const servedSessionId = manager.getSessionId();
        return {
            cwd: manager.getCwd(),
            extensionBuild: WORKFLOW_EXTENSION_BUILD,
            manager,
            release: () => dropSessionDelivery(servedSessionId),
        };
    };
    // Install delivery listeners once. Delivery is fail-closed until
    // session_start binds a per-session endpoint (no "latest pi wins"). Completions
    // that race before bind leave a disk pending marker and flush on bind.
    installResultDelivery(pi, manager, { loadSettings: () => loadWorkflowSettings({ cwd: getCwd() }) });
    const subagentsTool = createSubagentsTool({ getManager, getCwd, getStorage });
    const subagentsControlTool = createSubagentsControlTool({ getManager });
    pi.registerTool(subagentsTool);
    pi.registerTool(subagentsControlTool);
    let usageLimitScheduler = new UsageLimitScheduler(manager);
    let releaseDiagnostics;
    pi.on("session_shutdown", (event) => {
        // This session's UI is about to go stale.
        releaseDiagnostics?.();
        releaseDiagnostics = undefined;
        usageLimitScheduler.dispose();
        // Watchers report through this session's context, which is about to go stale.
        stopRunWatchers();
        // Always stop live sends first so a completion racing teardown cannot
        // deliver into the outgoing session (or throw on a just-stale ctx and be
        // lost). The runtime is staged for the next generation only when that
        // generation serves the same project; otherwise in-flight runs pause onto
        // the journal path.
        suspendResultDelivery(manager);
        // A generation that takes the runtime over rebinds delivery itself.
        const runtime = liveRuntime();
        if (nextGenerationKeepsProject(event, manager.getCwd())) {
            handoffWorkflowRuntime(runtime);
            return;
        }
        pauseStrandedWorkflowRuntime(runtime);
        discardWorkflowRuntime(runtime);
    });
    registerWorkflowCommands(pi, { getManager, getStorage, getCwd });
    registerWorkflowProgressCommands(pi, {
        load: () => loadWorkflowSettings({ cwd: getCwd() }),
        save: (nextSettings) => saveWorkflowSettings(nextSettings, getCwd()),
    });
    registerWorkflowModelsCommand(pi);
    registerBuiltinWorkflows(pi, { getManager, getCwd, getStorage });
    installParentPromptHook(pi);
    // Saved project commands are registered on session_start (after the real
    // ctx.cwd is known and any cross-project rebuild has finished). Registering
    // them in the factory would stamp source-project descriptions onto slash
    // commands before a resume into another project could correct the cwd —
    // and Pi cannot unregister/replace a command's metadata once registered.
    pi.on("session_start", (_event, ctx) => {
        // True project cwd for this session. Pi keeps process.cwd() on the
        // launching directory across /resume into another project; ctx.cwd is
        // the session header's project path.
        const sessionCwd = resolve(ctx.cwd || process.cwd());
        releaseDiagnostics?.();
        releaseDiagnostics = bindDiagnosticSink((message) => ctx.ui.notify(message, "warning"));
        // Another spelling of the same project (Windows) keeps the manager and
        // its path. A different project gets a manager of its own; the old one's
        // live runs pause, and the session it served lets go of its delivery.
        if (!isSameProject(sessionCwd, manager.getCwd())) {
            pausedForMismatch += pauseStrandedWorkflowRuntime(liveRuntime());
            manager = new WorkflowManager({ cwd: sessionCwd, ...buildManagerOptions(sessionCwd) });
            storage = createWorkflowStorage(sessionCwd);
            installResultDelivery(pi, manager, { loadSettings: () => loadWorkflowSettings({ cwd: getCwd() }) });
            usageLimitScheduler.dispose();
            usageLimitScheduler = new UsageLimitScheduler(manager);
        }
        if (loadedBuildIsStale()) {
            ctx.ui.notify("pi-workflows-lean changed on disk since Pi started. Restart Pi to load it; /reload keeps running the loaded build.", "warning");
        }
        // First registration (and post-rebuild catch-up for target-only names).
        // Handlers load by name from the live storage, so a later same-session
        // overwrite picks up the new script; Pi still cannot drop source-only
        // names left over from a prior generation — those handlers notify.
        registerAllSavedWorkflows(pi, getStorage, getManager);
        if (pausedForMismatch > 0) {
            ctx.ui.notify(`Paused ${pausedForMismatch} active workflow(s) that could not safely continue in this session (another extension build or a project switch). Resume them from /workflows when ready.`, "warning");
            pausedForMismatch = 0;
        }
        manager.setMainModel(ctx.model ? `${ctx.model.provider}/${ctx.model.id}` : undefined);
        manager.setModelRegistry(ctx.modelRegistry);
        const active = pi.getActiveTools();
        const missing = [subagentsTool.name, subagentsControlTool.name].filter((name) => !active.includes(name));
        if (missing.length)
            pi.setActiveTools([...active, ...missing]);
        // Bind + adopt before binding delivery so a flushed completion is tagged
        // with this session and visible in its panel. Capture the previous id first
        // so completed-with-pending can be re-homed across /new / fork / switch.
        let sessionId;
        let sessionFile;
        try {
            sessionId = ctx.sessionManager?.getSessionId();
        }
        catch {
            // sessionManager may be unavailable — fall back to global history.
        }
        try {
            sessionFile = ctx.sessionManager?.getSessionFile();
        }
        catch {
            // An ephemeral or unavailable session has no parent file.
        }
        const previousSessionId = manager.getSessionId();
        manager.adoptLiveRunsToSession(sessionId, previousSessionId);
        manager.setSessionId(sessionId, sessionFile);
        // Runtime is bound now (session_start fires after bindCore). Register a
        // session-stable delivery endpoint for THIS session only, then flush any
        // disk/memory pending for this sessionId (parallel siblings never share it).
        if (sessionId) {
            bindSessionDelivery(sessionId, pi, {
                loadSettings: () => loadWorkflowSettings({ cwd: getCwd() }),
                reportWarning: (message) => ctx.ui.notify(message, "warning"),
                manager,
                sessionManager: ctx.sessionManager,
            });
            if (previousSessionId && previousSessionId !== sessionId) {
                dropSessionDelivery(previousSessionId);
            }
        }
        installTaskPanel(manager, ctx.ui, () => loadWorkflowSettings({ cwd: getCwd() }));
    });
    // Keep mainModel in sync with mid-session /model (and cycle/restore). Without
    // this, workflow subagents that fall through to mainModel keep the frozen
    // session_start value for the rest of the process lifetime.
    pi.on("model_select", (event) => {
        const m = event.model;
        manager.setMainModel(m ? `${m.provider}/${m.id}` : undefined);
    });
}
