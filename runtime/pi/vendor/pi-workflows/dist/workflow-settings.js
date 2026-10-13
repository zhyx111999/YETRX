/**
 * User-level settings for pi-dynamic-workflows.
 *
 * Stored separately from Pi's own settings.json so extension preferences remain
 * stable without depending on host-internal config shape.
 */
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { MAX_AGENT_RETRIES, MAX_CONCURRENCY, WORKFLOW_SETTINGS_FILE } from "./config.js";
import { workflowHomeDir, workflowProjectPaths } from "./workflow-paths.js";
/** Map the run-limit settings onto WorkflowManager options. */
export function workflowRunLimits(settings) {
    return {
        concurrency: settings.defaultConcurrency,
        maxAgents: settings.defaultMaxAgents,
        agentTimeoutMs: settings.defaultAgentTimeoutMs ?? null,
        agentRetries: settings.defaultAgentRetries,
    };
}
/** Path to the user-level workflow settings JSON file (~/.pi/workflows/settings.json). */
export function getWorkflowSettingsPath() {
    return join(workflowHomeDir(), "settings.json");
}
/** Path to this project's optional workflow settings override. */
export function getWorkflowProjectSettingsPath(cwd) {
    return workflowProjectPaths(cwd).settingsPath;
}
/**
 * Path to the project-local (in-repo) workflow settings file
 * (`<cwd>/.pi/workflows/settings.json`). Lets a repository or its tooling ship
 * workflow defaults with the project, the same way project-local
 * `.pi/workflows/saved/` ships saved workflows.
 */
export function getProjectLocalWorkflowSettingsPath(cwd) {
    return resolve(cwd, WORKFLOW_SETTINGS_FILE);
}
/**
 * Load settings from disk. Missing, corrupt, or invalid files resolve to {}.
 * Precedence when a cwd is provided (later wins): global user settings, then
 * the project-local in-repo file (`<cwd>/.pi/workflows/settings.json`), then
 * the per-project override under `~/.pi/workflows/projects/<key>/` — so a
 * repo can ship defaults while a user's own project override still wins.
 */
export function loadWorkflowSettings(settingsPathOrOptions) {
    const options = normalizeOptions(settingsPathOrOptions);
    const globalSettings = readSettings(options.settingsPath ?? getWorkflowSettingsPath());
    const projectLocalPath = options.projectLocalSettingsPath ?? (options.cwd ? getProjectLocalWorkflowSettingsPath(options.cwd) : undefined);
    const projectPath = options.projectSettingsPath ?? (options.cwd ? getWorkflowProjectSettingsPath(options.cwd) : undefined);
    return {
        ...globalSettings,
        ...(projectLocalPath ? readSettings(projectLocalPath) : {}),
        ...(projectPath ? readSettings(projectPath) : {}),
    };
}
/**
 * Merge the known keys of `settings` into the one settings file at `path`,
 * creating it when missing. Other keys in the file are left as they are.
 */
export function writeWorkflowSettingsFile(path, settings) {
    const dir = dirname(path);
    if (!existsSync(dir))
        mkdirSync(dir, { recursive: true });
    const existing = readObject(path);
    writeFileSync(path, `${JSON.stringify({ ...existing, ...normalizeSettings(settings) }, null, 2)}\n`, "utf-8");
}
/**
 * Save preferences so they are what `loadWorkflowSettings({ cwd })` returns.
 * Every key goes to the global file as the user's default. A key that the
 * repo-local file or the project override already sets would shadow that
 * write, so it also goes to the project override, the top layer, which is
 * created when missing. Keys no project layer sets stay out of the override,
 * so later global changes still reach this project. The repo-local file is
 * never written: it belongs to the repository and may be shared.
 */
export function saveWorkflowSettings(settings, cwd) {
    const next = normalizeSettings(settings);
    writeWorkflowSettingsFile(getWorkflowSettingsPath(), next);
    const projectPath = getWorkflowProjectSettingsPath(cwd);
    const projectLayers = { ...readSettings(getProjectLocalWorkflowSettingsPath(cwd)), ...readSettings(projectPath) };
    const shadowed = Object.fromEntries(Object.entries(next).filter(([key]) => key in projectLayers));
    if (Object.keys(shadowed).length)
        writeWorkflowSettingsFile(projectPath, shadowed);
}
function normalizeOptions(settingsPathOrOptions) {
    return typeof settingsPathOrOptions === "string"
        ? { settingsPath: settingsPathOrOptions }
        : (settingsPathOrOptions ?? {});
}
function readSettings(path) {
    if (!existsSync(path))
        return {};
    try {
        return normalizeSettings(JSON.parse(readFileSync(path, "utf-8")));
    }
    catch {
        return {};
    }
}
/**
 * Copy only known, valid keys. Files written by older versions may still hold
 * keys for removed features; they are ignored here and left untouched on disk
 * by writeWorkflowSettingsFile.
 */
function normalizeSettings(value) {
    if (!value || typeof value !== "object" || Array.isArray(value))
        return {};
    const raw = value;
    const settings = {};
    if (raw.defaultAgentTimeoutMs === null) {
        settings.defaultAgentTimeoutMs = null;
    }
    else if (typeof raw.defaultAgentTimeoutMs === "number" &&
        Number.isFinite(raw.defaultAgentTimeoutMs) &&
        raw.defaultAgentTimeoutMs > 0) {
        settings.defaultAgentTimeoutMs = raw.defaultAgentTimeoutMs;
    }
    const defaultConcurrency = normalizeInteger(raw.defaultConcurrency, 1, MAX_CONCURRENCY);
    if (defaultConcurrency !== undefined)
        settings.defaultConcurrency = defaultConcurrency;
    const defaultAgentRetries = normalizeInteger(raw.defaultAgentRetries, 0, MAX_AGENT_RETRIES);
    if (defaultAgentRetries !== undefined)
        settings.defaultAgentRetries = defaultAgentRetries;
    const defaultMaxAgents = normalizeInteger(raw.defaultMaxAgents, 1, Number.MAX_SAFE_INTEGER);
    if (defaultMaxAgents !== undefined)
        settings.defaultMaxAgents = defaultMaxAgents;
    if (raw.progressPanelMode === "compact" || raw.progressPanelMode === "detailed") {
        settings.progressPanelMode = raw.progressPanelMode;
    }
    if (typeof raw.progressPanelMaxAgents === "number" &&
        Number.isFinite(raw.progressPanelMaxAgents) &&
        raw.progressPanelMaxAgents >= 1) {
        settings.progressPanelMaxAgents = Math.min(1000, Math.floor(raw.progressPanelMaxAgents));
    }
    if (typeof raw.persistAgentSessions === "boolean") {
        settings.persistAgentSessions = raw.persistAgentSessions;
    }
    if (typeof raw.inheritMainModel === "boolean") {
        settings.inheritMainModel = raw.inheritMainModel;
    }
    const deliveredResultMaxChars = normalizeInteger(raw.deliveredResultMaxChars, 1, 1_000_000);
    if (deliveredResultMaxChars !== undefined)
        settings.deliveredResultMaxChars = deliveredResultMaxChars;
    const excludeSubagentTools = normalizeNames(raw.excludeSubagentTools);
    if (excludeSubagentTools)
        settings.excludeSubagentTools = excludeSubagentTools;
    const providerMiddlewareExtensions = normalizeNames(raw.providerMiddlewareExtensions);
    if (providerMiddlewareExtensions)
        settings.providerMiddlewareExtensions = providerMiddlewareExtensions;
    return settings;
}
/**
 * Trimmed non-blank names. Any array is kept, even one that ends up empty: an
 * explicit [] in a later layer must clear a list set by an earlier one.
 */
function normalizeNames(value) {
    if (!Array.isArray(value))
        return undefined;
    return value
        .filter((name) => typeof name === "string" && name.trim().length > 0)
        .map((name) => name.trim());
}
function normalizeInteger(value, min, max) {
    if (typeof value !== "number" || !Number.isFinite(value) || value < min)
        return undefined;
    return Math.min(max, Math.floor(value));
}
function readObject(path) {
    if (!existsSync(path))
        return {};
    try {
        const parsed = JSON.parse(readFileSync(path, "utf-8"));
        return parsed && typeof parsed === "object" && !Array.isArray(parsed) ? parsed : {};
    }
    catch {
        return {};
    }
}
