import { readdirSync, readFileSync, writeFileSync } from "node:fs";
import { join, relative } from "node:path";
import { createSubagentsControlTool } from "./subagents-control-tool.js";
import { createSubagentsTool } from "./subagents-tool.js";
/** Package-relative generated context-measurement artifact. */
export const WORKFLOW_CONTEXT_MEASUREMENT_PATH = "docs/workflow-context-surfaces.json";
const ROOT = join(import.meta.dirname, "..");
const SKILL_ROOT = "skills/workflow-authoring";
const SKILL_PATH = `${SKILL_ROOT}/SKILL.md`;
/** Canonical task profiles used for stable byte-based on-demand context measurements. */
const WORKFLOW_AUTHORING_PROFILES = [
    {
        name: "write",
        files: [
            SKILL_PATH,
            `${SKILL_ROOT}/references/runtime.md`,
            `${SKILL_ROOT}/references/pattern-selection.md`,
            `${SKILL_ROOT}/references/focused-recipes.md`,
            `${SKILL_ROOT}/examples/fan-out-and-synthesize.js`,
            `${SKILL_ROOT}/examples/structured-output.js`,
        ],
    },
    {
        name: "edit",
        files: [
            SKILL_PATH,
            `${SKILL_ROOT}/references/runtime.md`,
            `${SKILL_ROOT}/references/lifecycle.md`,
            `${SKILL_ROOT}/references/focused-recipes.md`,
            `${SKILL_ROOT}/examples/structured-output.js`,
            `${SKILL_ROOT}/examples/bounded-semantic-retry.js`,
        ],
    },
    {
        name: "review",
        files: [
            SKILL_PATH,
            `${SKILL_ROOT}/references/runtime.md`,
            `${SKILL_ROOT}/references/review.md`,
            `${SKILL_ROOT}/references/lifecycle.md`,
            `${SKILL_ROOT}/examples/classify-and-act.js`,
        ],
    },
    {
        name: "debug",
        files: [
            SKILL_PATH,
            `${SKILL_ROOT}/references/runtime.md`,
            `${SKILL_ROOT}/references/debugging.md`,
            `${SKILL_ROOT}/references/checkpoint.md`,
            `${SKILL_ROOT}/examples/defensive-json-parsing.js`,
        ],
    },
    {
        name: "loop",
        files: [
            SKILL_PATH,
            `${SKILL_ROOT}/references/runtime.md`,
            `${SKILL_ROOT}/references/pattern-selection.md`,
            `${SKILL_ROOT}/references/lifecycle.md`,
            `${SKILL_ROOT}/references/focused-recipes.md`,
            `${SKILL_ROOT}/examples/loop-until-done.js`,
            `${SKILL_ROOT}/examples/structured-output.js`,
        ],
    },
    {
        name: "retry",
        files: [
            SKILL_PATH,
            `${SKILL_ROOT}/references/runtime.md`,
            `${SKILL_ROOT}/references/lifecycle.md`,
            `${SKILL_ROOT}/references/focused-recipes.md`,
            `${SKILL_ROOT}/examples/bounded-semantic-retry.js`,
            `${SKILL_ROOT}/examples/structured-output.js`,
        ],
    },
];
const SOURCES = [
    "src/subagents-tool.ts",
    "src/subagents-control-tool.ts",
    SKILL_ROOT,
    "package.json#pi.skills",
];
function bytes(value) {
    return Buffer.byteLength(value, "utf8");
}
function fileBytes(root, path) {
    return bytes(readFileSync(join(root, path), "utf8"));
}
function skillFiles(root) {
    const absoluteRoot = join(root, SKILL_ROOT);
    const pending = [absoluteRoot];
    const files = [];
    while (pending.length > 0) {
        const directory = pending.pop();
        if (!directory)
            continue;
        for (const entry of readdirSync(directory, { withFileTypes: true })) {
            const absolute = join(directory, entry.name);
            if (entry.isDirectory())
                pending.push(absolute);
            else if (entry.isFile())
                files.push(relative(root, absolute).replaceAll("\\", "/"));
        }
    }
    return files.sort();
}
/**
 * Every skill root this package registers, read from package.json's
 * `pi.skills` array rather than hardcoded — so a newly added skill is picked
 * up automatically instead of silently missing from the always-on tally.
 */
function registeredSkillRoots(root) {
    const manifest = JSON.parse(readFileSync(join(root, "package.json"), "utf8"));
    const skills = manifest.pi?.skills;
    if (!Array.isArray(skills) || skills.length === 0 || !skills.every((s) => typeof s === "string")) {
        throw new Error("package.json must declare a non-empty pi.skills array of skill root paths");
    }
    return skills;
}
function skillDiscoveryEntry(root, skillRoot) {
    const skillPath = `${skillRoot}/SKILL.md`;
    const skill = readFileSync(join(root, skillPath), "utf8");
    const name = /^name:\s*(.+)$/m.exec(skill)?.[1]?.trim();
    const description = /^description:\s*(.+)$/m.exec(skill)?.[1]?.trim();
    if (!name || !description)
        throw new Error(`${skillPath} must declare name and description`);
    return [
        "<skill>",
        `  <name>${name}</name>`,
        `  <description>${description}</description>`,
        `  <location>${skillPath}</location>`,
        "</skill>",
    ].join("\n");
}
function median(values) {
    const sorted = [...values].sort((left, right) => left - right);
    const middle = sorted.length / 2;
    return sorted.length % 2 === 0
        ? ((sorted[middle - 1] ?? 0) + (sorted[middle] ?? 0)) / 2
        : (sorted[Math.floor(middle)] ?? 0);
}
/** The registered tools, built without a host because measuring never executes them. */
export function measuredTools() {
    const unused = () => {
        throw new Error("context measurement never executes a tool");
    };
    return [
        createSubagentsTool({ getManager: unused, getStorage: unused, getCwd: unused }),
        createSubagentsControlTool({ getManager: unused }),
    ];
}
/** The lines Pi renders for the tools: one Available-tools entry per snippet, then every guideline. */
export function permanentToolPromptLines(tools) {
    return [
        ...tools.flatMap((tool) => (tool.promptSnippet ? [`- ${tool.name}: ${tool.promptSnippet}`] : [])),
        ...tools.flatMap((tool) => (tool.promptGuidelines ?? []).map((guideline) => `- ${guideline}`)),
    ];
}
/** Measures permanent, discovery, corpus, and canonical on-demand workflow context surfaces. */
export function measureWorkflowContextSurfaces(root = ROOT) {
    const tools = measuredTools();
    const permanentToolPrompt = permanentToolPromptLines(tools).join("\n");
    const toolDefinitions = tools.map((tool) => ({
        name: tool.name,
        bytes: bytes(JSON.stringify({ name: tool.name, description: tool.description, parameters: tool.parameters })),
    }));
    const skillRoots = registeredSkillRoots(root);
    const skillDiscoverySurfaces = skillRoots.map((skillRoot) => ({
        root: skillRoot,
        serialization: "UTF-8 bytes of normalized Pi skill XML with package-relative location",
        bytes: bytes(skillDiscoveryEntry(root, skillRoot)),
    }));
    const registeredSkillsDiscoveryBytes = skillDiscoverySurfaces.reduce((sum, surface) => sum + surface.bytes, 0);
    const corpusFiles = skillFiles(root);
    const corpusBytes = corpusFiles.reduce((sum, path) => sum + fileBytes(root, path), 0);
    const profiles = WORKFLOW_AUTHORING_PROFILES.map((profile) => ({
        name: profile.name,
        files: [...profile.files],
        bytes: profile.files.reduce((sum, path) => sum + fileBytes(root, path), 0),
    }));
    const promptBytes = bytes(permanentToolPrompt);
    const toolBytes = toolDefinitions.reduce((sum, tool) => sum + tool.bytes, 0);
    return {
        formatVersion: 4,
        encoding: "utf8",
        sources: SOURCES,
        surfaces: {
            permanentToolPrompt: {
                serialization: "UTF-8 bytes of LF-joined Pi prompt lines",
                bytes: promptBytes,
            },
            providerVisibleToolDefinitions: {
                serialization: "sum of UTF-8 bytes of JSON.stringify({ name, description, parameters }) per tool",
                bytes: toolBytes,
                tools: toolDefinitions,
            },
            registeredSkillsDiscovery: {
                serialization: "sum of UTF-8 bytes of normalized Pi skill XML (name + description + location) across every root in package.json's pi.skills",
                bytes: registeredSkillsDiscoveryBytes,
                skills: skillDiscoverySurfaces,
            },
            ordinaryWorkflowOwnedAlwaysOn: {
                serialization: "sum of permanent prompt, provider-visible tool definitions, and every registered skill's discovery bytes",
                bytes: promptBytes + toolBytes + registeredSkillsDiscoveryBytes,
            },
            workflowAuthoringSkillCorpus: {
                serialization: `sum of UTF-8 bytes for every file under ${SKILL_ROOT}`,
                files: corpusFiles.length,
                bytes: corpusBytes,
            },
            representativeAuthoringProfiles: {
                serialization: "sum of UTF-8 bytes for each profile's declared package-relative files",
                medianBytes: median(profiles.map(({ bytes: profileBytes }) => profileBytes)),
                profiles,
            },
        },
    };
}
/** The artifact's only serialization, so writing and freshness checks compare identical bytes. */
function serializeMeasurement(measurement) {
    return `${JSON.stringify(measurement, null, 2)}\n`;
}
/** Render the measurement of the package at root as deterministic formatted JSON. */
export function renderWorkflowContextMeasurement(root = ROOT) {
    return serializeMeasurement(measureWorkflowContextSurfaces(root));
}
/** Write a measurement of the package at root (by default a fresh one) as its committed artifact. */
export function writeWorkflowContextMeasurement(root, measurement = measureWorkflowContextSurfaces(root)) {
    writeFileSync(join(root, WORKFLOW_CONTEXT_MEASUREMENT_PATH), serializeMeasurement(measurement));
}
/** Report whether the artifact committed under root matches a measurement of it (by default a fresh one). */
export function checkWorkflowContextMeasurement(root, measurement = measureWorkflowContextSurfaces(root)) {
    return readFileSync(join(root, WORKFLOW_CONTEXT_MEASUREMENT_PATH), "utf8") === serializeMeasurement(measurement);
}
