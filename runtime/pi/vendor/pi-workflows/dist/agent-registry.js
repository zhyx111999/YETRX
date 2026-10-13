/**
 * Named workflow subagent definitions ("agentType" registry).
 *
 * A workflow script can route an agent() call to a reusable, named definition:
 *
 *   agent("audit this dir", { agentType: "security-auditor" })
 *
 * Definitions live as Markdown files under `.pi/agents/*.md` (project, cwd-relative)
 * and `~/.pi/agent/agents/*.md` (user — `getAgentDir() + "agents"`, honoring the
 * `PI_CODING_AGENT_DIR` override), matching pi-coding-agent's own agent discovery
 * convention. A project definition wins over a user one on a name collision.
 * Frontmatter binds the subagent's tools, model, and a body prompt, so agentType
 * is a real binding of tools+model+system-prompt, not a prose hint.
 *
 * Bound: `tools` (allowlist), `disallowedTools` (denylist), `model`, `thinking`,
 * `isolation` ("worktree"), and the markdown body (`prompt`).
 */
import { existsSync, readdirSync, readFileSync } from "node:fs";
import { basename, join } from "node:path";
import { getAgentDir, parseFrontmatter } from "@earendil-works/pi-coding-agent";
import { AGENTS_DIR } from "./config.js";
import { isThinkingLevel } from "./model-spec.js";
function toStringArray(value) {
    if (value == null)
        return undefined;
    // YAML list form: ["read", "grep"]
    if (Array.isArray(value)) {
        const arr = value.filter((v) => typeof v === "string" && v.trim().length > 0).map((v) => v.trim());
        return arr.length ? arr : undefined;
    }
    // Comma-separated string form: "read, grep, find" — the form pi-coding-agent's
    // parseFrontmatter returns and the form the official subagent example uses.
    if (typeof value === "string" && value.trim().length > 0) {
        const arr = value
            .split(",")
            .map((s) => s.trim())
            .filter(Boolean);
        return arr.length ? arr : undefined;
    }
    return undefined;
}
function toThinkingLevel(value) {
    if (typeof value !== "string")
        return undefined;
    const trimmed = value.trim().toLowerCase();
    return isThinkingLevel(trimmed) ? trimmed : undefined;
}
/**
 * Parse one agent-definition markdown file. Returns null only when there is no
 * usable content (no name derivable and an empty body).
 */
export function parseAgentDefinition(content, source, fileName) {
    let parsed;
    try {
        parsed = parseFrontmatter(content);
    }
    catch {
        // Malformed frontmatter: treat the whole file as a body, name from filename.
        parsed = { frontmatter: {}, body: content };
    }
    const fm = parsed.frontmatter;
    const fmName = typeof fm.name === "string" ? fm.name.trim() : "";
    const name = fmName || basename(fileName).replace(/\.md$/i, "").trim();
    const prompt = parsed.body.trim();
    if (!name && !prompt)
        return null;
    return {
        name,
        tools: toStringArray(fm.tools),
        disallowedTools: toStringArray(fm.disallowedTools),
        model: typeof fm.model === "string" ? fm.model.trim() || undefined : undefined,
        thinking: toThinkingLevel(fm.thinking),
        isolation: typeof fm.isolation === "string" && fm.isolation.toLowerCase().trim() === "worktree" ? "worktree" : undefined,
        prompt,
        source,
    };
}
function readDefsFromDir(dir, source) {
    if (!existsSync(dir))
        return [];
    let files;
    try {
        files = readdirSync(dir).filter((f) => f.toLowerCase().endsWith(".md"));
    }
    catch {
        return [];
    }
    const defs = [];
    for (const file of files.sort()) {
        try {
            const def = parseAgentDefinition(readFileSync(join(dir, file), "utf-8"), source, file);
            if (def)
                defs.push(def);
        }
        catch {
            // Skip unreadable/invalid files; never let one bad file break the registry.
        }
    }
    return defs;
}
/**
 * Load the agent registry once for a run. Scans the project dir, then the user
 * dir; the FIRST definition for a name wins (project > user, then filename
 * order), so a name collision is resolved deterministically and silently.
 *
 * `opts` overrides the scanned directories (used by tests).
 */
export function loadAgentRegistry(cwd, opts) {
    const projectDir = opts?.projectDir ?? join(cwd, AGENTS_DIR);
    // User-level definitions live under the agent dir (e.g. ~/.pi/agent/agents/),
    // matching the convention used by pi-coding-agent's built-in agent discovery
    // and the official subagent extension example. Reading getAgentDir() also
    // honors the PI_CODING_AGENT_DIR env override.
    const userDir = opts?.userDir ?? join(getAgentDir(), "agents");
    const registry = new Map();
    for (const def of readDefsFromDir(projectDir, "project")) {
        if (def.name && !registry.has(def.name))
            registry.set(def.name, def);
    }
    if (userDir !== projectDir) {
        for (const def of readDefsFromDir(userDir, "user")) {
            if (def.name && !registry.has(def.name))
                registry.set(def.name, def);
        }
    }
    return registry;
}
/** Resolve an agentType name to its definition, or undefined if not registered. */
export function resolveAgentType(name, registry) {
    if (!name)
        return undefined;
    return registry.get(name);
}
/**
 * Apply a definition's tool policy to a tool list: keep only allowlisted names
 * (when an allowlist is given), then drop any denylisted names. Generic over any
 * object with a `name` so it is unit-testable without real ToolDefinitions.
 */
export function applyToolPolicy(tools, allow, deny) {
    let out = tools;
    if (allow?.length) {
        const allowSet = new Set(allow);
        out = out.filter((t) => allowSet.has(t.name));
    }
    if (deny?.length) {
        const denySet = new Set(deny);
        out = out.filter((t) => !denySet.has(t.name));
    }
    return out;
}
/**
 * A stable identity string for a resolved definition, folded into the resume
 * call-hash so editing an agent `.md` invalidates that call's cached result.
 */
export function agentDefinitionKey(def) {
    if (!def)
        return null;
    return JSON.stringify({
        tools: def.tools ?? null,
        disallowedTools: def.disallowedTools ?? null,
        model: def.model ?? null,
        ...(def.thinking !== undefined ? { thinking: def.thinking } : {}),
        isolation: def.isolation ?? null,
        prompt: def.prompt,
    });
}
