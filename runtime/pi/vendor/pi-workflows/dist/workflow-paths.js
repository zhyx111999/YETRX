/**
 * Filesystem layout of workflow state. Everything lives under ~/.pi/workflows;
 * each project gets its own directory, named by a stable key of its path.
 */
import { createHash } from "node:crypto";
import { realpathSync } from "node:fs";
import { homedir } from "node:os";
import { basename, join, resolve } from "node:path";
export const WORKFLOW_HOME_RELATIVE_DIR = ".pi/workflows";
export const WORKFLOW_PROJECTS_SUBDIR = "projects";
export function workflowHomeDir() {
    return join(homedir(), WORKFLOW_HOME_RELATIVE_DIR);
}
export function workflowUserSavedDir() {
    return join(workflowHomeDir(), "saved");
}
/**
 * Storage key of the project at `cwd`: its folder name and a hash of its path.
 * Windows paths are case-insensitive, so one directory has many spellings
 * (drive letter and name case, 8.3 names); the hash uses the spelling stored
 * on disk, which is also how Windows normally reports the path.
 */
export function workflowProjectKey(cwd) {
    const path = projectPath(cwd);
    const slug = sanitizePathSegment(basename(path) || "project");
    const hash = createHash("sha256").update(path).digest("hex").slice(0, 12);
    return `${slug}-${hash}`;
}
function projectPath(cwd) {
    const path = resolve(cwd);
    if (process.platform !== "win32")
        return path;
    try {
        return realpathSync.native(path);
    }
    catch {
        // A directory that does not exist yet has no stored spelling.
        return path;
    }
}
export function workflowProjectPaths(cwd) {
    const key = workflowProjectKey(cwd);
    const rootDir = join(workflowHomeDir(), WORKFLOW_PROJECTS_SUBDIR, key);
    return {
        key,
        rootDir,
        runsDir: join(rootDir, "runs"),
        savedDir: join(rootDir, "saved"),
        settingsPath: join(rootDir, "settings.json"),
        modelTiersPath: join(rootDir, "model-tiers.json"),
    };
}
function sanitizePathSegment(value) {
    const sanitized = value
        .toLowerCase()
        .replace(/[^a-z0-9._-]+/g, "-")
        .replace(/^-+|-+$/g, "")
        .slice(0, 48);
    return sanitized || "project";
}
