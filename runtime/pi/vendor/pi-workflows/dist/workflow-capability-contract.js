/**
 * Workflow capability contract: the single declaration of every workflow runtime
 * global and `subagents` tool input. runWorkflow assembles script globals from it
 * and the docs generator publishes it, so runtime and documentation cannot drift.
 */
import packageJson from "../package.json" with { type: "json" };
import { deepFreeze } from "./deep-freeze.js";
import { CapabilityClassification, CapabilityOrigin, CapabilitySupport, DiagnosticSeverity } from "./enums.js";
import { WorkflowCapabilityContractError } from "./errors.js";
import { THINKING_LEVELS } from "./model-spec.js";
/** Package-relative exhaustive generated capability reference; every fact's `reference` links into it. */
export const CAPABILITY_DETAIL_PATH = "skills/workflow-authoring/references/capability-details.md";
const NONE = [];
function typeText(rule) {
    switch (rule.kind) {
        case "string":
            return rule.nonEmpty ? "non-empty string" : "string";
        case "boolean":
            return "boolean";
        case "literal":
            return rule.values.map((value) => JSON.stringify(value)).join(" | ");
        case "json-schema":
            return "plain JSON Schema object";
        case "directory":
            return "absolute directory path";
    }
}
const option = (name, accepts, optional, defaultValue = null, constraints = NONE, dynamicReference = null) => ({
    name,
    type: typeText(accepts),
    accepts,
    optional,
    default: defaultValue,
    constraints,
    dynamicReference,
});
const ANY_STRING = { kind: "string", nonEmpty: false };
const NON_EMPTY_STRING = { kind: "string", nonEmpty: true };
/** The first agent() argument: the task text the subagent receives. */
export const AGENT_PROMPT = option("prompt", NON_EMPTY_STRING, false);
/** The agent() prompt and complete option set; the runtime rejects any other key or value. */
export const AGENT_OPTIONS = {
    id: "agent-options",
    positional: [AGENT_PROMPT],
    options: [
        option("label", ANY_STRING, true, "derived from phase and call count", ["blank uses the default"]),
        option("phase", NON_EMPTY_STRING, true, "current phase"),
        option("schema", { kind: "json-schema" }, true),
        option("model", NON_EMPTY_STRING, true, null, ["highest-priority exact model selector"]),
        option("thinking", { kind: "literal", values: THINKING_LEVELS }, true, null, [
            "call-site value overrides agentType thinking; selected model suffix takes precedence",
        ]),
        option("tier", NON_EMPTY_STRING, true, null, ["configured route name"], "model-routes"),
        option("isolation", { kind: "literal", values: ["worktree", false] }, true, null, [
            "false opts out of an agentType worktree default",
        ]),
        option("keepWorktree", { kind: "boolean" }, true, "true"),
        option("cwd", { kind: "directory" }, true, null, [
            "must exist; resolved to its real path",
            "coding tools and session cwd use the target directory; settings and AGENTS/skill resources also use it unless explicitly injected by the embedding host",
            "cannot combine with worktree isolation",
            "on Windows a path segment that ends with a dot or space is rejected, because processes would start in a different directory",
        ]),
        option("thread", NON_EMPTY_STRING, true, null, ["trimmed; same-name calls must be sequential"]),
        option("agentType", NON_EMPTY_STRING, true, null, ["must come from provided context", "a name the agent registry does not define fails the call"], "agent-types"),
    ],
};
const runtimeGlobal = (name, options = {}) => ({
    id: `workflow.runtime.${name}`,
    label: name,
    classification: CapabilityClassification.RUNTIME_GLOBAL,
    support: options.support ?? CapabilitySupport.SUPPORTED,
    origin: CapabilityOrigin.PROJECT,
    signature: options.signature ?? name,
    optionShape: options.optionShape ?? null,
    constraints: options.constraints ?? NONE,
    runtimeBinding: {
        global: name,
        implementation: name,
        ...(options.allowsUndefined ? { allowsUndefined: true } : {}),
    },
    detailsAnchor: name.toLowerCase(),
    dynamicReference: null,
});
const subagentsInput = (name, signature, constraints) => ({
    id: `workflow.subagents-input.${name}`,
    label: name,
    classification: CapabilityClassification.SUBAGENTS_TOOL_INPUT,
    support: CapabilitySupport.SUPPORTED,
    origin: CapabilityOrigin.TOOL_ADAPTER,
    signature,
    optionShape: null,
    constraints,
    runtimeBinding: null,
    detailsAnchor: `subagents-input-${name.toLowerCase()}`,
    dynamicReference: null,
});
const ONE_START_MODE = "exactly one of tasks, script, or name per call";
const capabilities = [
    runtimeGlobal("agent", {
        signature: "agent(prompt, options?) => Promise<string | structured value | null>",
        optionShape: "agent-options",
        constraints: [
            "prompt must be a non-empty string; options must be an object using only the declared option names, each value matching its declared type (undefined means omitted); anything else fails before dispatch with SCRIPT_VALIDATION_ERROR listing the accepted names or values",
            "recoverable failures return null after retries; nonrecoverable failures throw",
            "schema noncompliance after bounded structured-output repair is nonrecoverable and bypasses agent retries",
            "run limits come only from settings: recoverable failures retry defaultAgentRetries times (0..3), each attempt is bounded by defaultAgentTimeoutMs (unset means no hard timeout), and defaultConcurrency agents run at once (1..16)",
            "every agent() and checkpoint() call consumes one of the run's defaultMaxAgents slots (default 1000); execution retries consume none",
            "resume replays only the longest unchanged prefix; the first miss and every later call execute live",
            "a named thread retains its full Pi transcript and session identity only within one uninterrupted workflow invocation",
            "threaded calls are live-execution resume barriers and are never journaled",
            "same-thread calls must be sequential; threads cannot use worktree isolation",
            "a named thread's canonical cwd is fixed by its first call; a later call using that name with a different cwd fails validation instead of reusing the prior transcript",
            "an explicit cwd must be a non-empty absolute existing directory; its real path determines coding tools and session cwd; default settings and AGENTS/skill resources follow it while embedding-host dependency overrides remain authoritative; it participates in resume identity and cannot combine with worktree isolation",
            "selector priority is explicit model > agentType model > tier > phase model > metadata model > inherited main model (inheritMainModel on) > implicit medium > session default",
            "an explicit model, agentType model, tier, or phase model that resolves to an unavailable model throws MODEL_NOT_FOUND naming the source (e.g. the tier and what it resolved to) instead of falling back",
            "a tier that a saved model-tiers.json does not define throws MODEL_NOT_FOUND listing the defined tiers; with no model-tiers.json saved, the session's main model stands in for every tier and the run log says so once",
            "only the implicit route an untagged agent falls into — the implicit default medium tier, or the inherited main model when inheritMainModel is on (no explicit model, tier, agentType, or phase model requested) — degrades to the session default when unavailable, logging a one-time run-visible warning instead of throwing",
            "with the inheritMainModel setting on, an untagged agent (no explicit model, tier, agentType model, or phase model) inherits the session's main model (the model in effect when the run starts) instead of the implicit medium tier or the settings default; an unavailable inherited model degrades to the settings default with a one-time run-visible warning instead of throwing",
            "requested worktree isolation fails closed before agent execution if git cannot create the worktree; it never falls back to the shared checkout",
            "keepWorktree defaults true (worktree kept for merge); false deletes after the call",
            "each live execution creates a uniquely owned worktree; retries or resume never reuse or overwrite a retained worktree",
        ],
    }),
    runtimeGlobal("parallel", {
        signature: "parallel(thunks) => Promise<Array<unknown | null>>",
        constraints: [
            "requires functions rather than promises",
            "result order matches input order",
            "a thunk's recoverable workflow failure becomes null, as with agent(); nonrecoverable failures and errors thrown by the thunk's own code reject parallel()",
        ],
    }),
    runtimeGlobal("pipeline", {
        signature: "pipeline(items, ...stages) => Promise<Array<unknown | null>>",
        constraints: [
            "items run concurrently while stages per item run sequentially",
            "each stage receives previousValue, originalItem, and zero-based index",
            "a null stage result is passed to the next stage; authors must guard missing coverage explicitly",
            "a stage's recoverable workflow failure becomes null, as with agent(); nonrecoverable failures and errors thrown by the stage's own code reject pipeline()",
        ],
    }),
    runtimeGlobal("checkpoint", {
        signature: "checkpoint({ kind, checkpointId, payload }) => Promise<unknown>",
        constraints: [
            "consumes one agent slot and no tokens",
            "journaled answers replay only within an unchanged resume prefix",
            "accepts an open kind identifier, persists its JSON payload, and pauses the run",
            "subagents_control resume answers a waiting checkpoint with its exact checkpointId and a lossless-JSON response, attached durably before the run continues",
            "subagents_control status shows the waiting checkpoint's ID, kind, and a bounded payload preview, never its response",
            "durable checkpoint responses resume the same run ID, are journaled before continuation, and reject stale or conflicting delivery",
        ],
    }),
    runtimeGlobal("log", { signature: "log(message) => void" }),
    runtimeGlobal("phase", {
        signature: "phase(title) => void",
        constraints: ["title must be a nonblank string"],
    }),
    runtimeGlobal("args", { signature: "args: unknown", allowsUndefined: true }),
    runtimeGlobal("cwd", { signature: "cwd: string" }),
    runtimeGlobal("process", { signature: "process: { cwd(): string }" }),
    runtimeGlobal("console", {
        signature: "console: { log, info, warn, error }",
        support: CapabilitySupport.COMPATIBILITY,
        constraints: ["new workflows should use log()"],
    }),
    subagentsInput("tasks", "tasks?: Array<{ label: string; task: string; model?: string }>", [
        ONE_START_MODE,
        "one subagent per task, all in parallel; labels must be unique and non-blank",
        "returns one text report with a section per label, in task order",
        "subagents_control resume with an edited list reuses every task whose label, task, and model are unchanged, wherever it moved; edited and new tasks run",
    ]),
    subagentsInput("script", "script?: string", [
        ONE_START_MODE,
        "raw JavaScript workflow source",
        "subagents_control resume with an edited script replays unchanged positional agent calls until the first changed or inserted call",
    ]),
    subagentsInput("name", "name?: string", [
        ONE_START_MODE,
        "resolves a project/user saved workflow first, then a built-in pattern",
    ]),
    subagentsInput("args", "args?: object", ["the args global of a script or named workflow; ignored with tasks"]),
    {
        id: "workflow.script.metadata",
        label: "export const meta",
        classification: CapabilityClassification.SCRIPT_CONTRACT,
        support: CapabilitySupport.SUPPORTED,
        origin: CapabilityOrigin.PROJECT,
        signature: "export const meta = { name: string, description: string, phases?: Array<{ title: string; detail?: string; model?: string }>, model?: string }",
        optionShape: null,
        constraints: [
            "must be the first statement",
            "name and description must be nonblank strings",
            "each phase title and every model must be a nonblank string; a phase detail must be a string",
            "metadata must use literal values; expressions such as string concatenation and template interpolation are rejected",
            "the meta declaration is the only legal export because the remaining body executes inside an async function",
        ],
        runtimeBinding: null,
        detailsAnchor: "metadata",
        dynamicReference: null,
    },
    {
        id: "workflow.script.return-value",
        label: "workflow return value",
        classification: CapabilityClassification.SCRIPT_CONTRACT,
        support: CapabilitySupport.SUPPORTED,
        origin: CapabilityOrigin.PROJECT,
        signature: "return JSON-serializable data",
        optionShape: null,
        constraints: ["do not return functions, promises, cyclic objects, BigInt, or runtime handles"],
        runtimeBinding: null,
        detailsAnchor: "return-value",
        dynamicReference: null,
    },
    {
        id: "workflow.script.determinism",
        label: "deterministic script execution",
        classification: CapabilityClassification.SCRIPT_CONTRACT,
        support: CapabilitySupport.SUPPORTED,
        origin: CapabilityOrigin.PROJECT,
        signature: null,
        optionShape: null,
        constraints: [
            "Date.now(), Date(), no-argument new Date(), Math.random(), and Intl.DateTimeFormat formatting without an explicit date are unavailable",
            "pass timestamps and randomness through args",
        ],
        runtimeBinding: null,
        detailsAnchor: "determinism",
        dynamicReference: null,
    },
    {
        id: "workflow.compat.markdown-fences",
        label: "whole-script Markdown fence stripping",
        classification: CapabilityClassification.COMPATIBILITY_BEHAVIOR,
        support: CapabilitySupport.COMPATIBILITY,
        origin: CapabilityOrigin.TOOL_ADAPTER,
        signature: null,
        optionShape: null,
        constraints: ["accepted for compatibility but not recommended"],
        runtimeBinding: null,
        detailsAnchor: "compatibility",
        dynamicReference: null,
    },
    {
        id: "workflow.vm.realm-substrate",
        label: "VM realm JavaScript substrate",
        classification: CapabilityClassification.INTERNAL_SUBSTRATE,
        support: CapabilitySupport.INTERNAL,
        origin: CapabilityOrigin.VM_REALM,
        signature: null,
        optionShape: null,
        constraints: ["Node-version-dependent globals are not project-owned workflow API", "VM is not a security sandbox"],
        runtimeBinding: null,
        detailsAnchor: "vm-realm",
        dynamicReference: null,
    },
    {
        id: "workflow.dynamic.model-routes",
        label: "model routes",
        classification: CapabilityClassification.DYNAMIC_REFERENCE,
        support: CapabilitySupport.SUPPORTED,
        origin: CapabilityOrigin.LIVE_CONFIGURATION,
        signature: null,
        optionShape: null,
        constraints: ["live values must not be copied into static contract data"],
        runtimeBinding: null,
        detailsAnchor: "model-routes",
        dynamicReference: "model-routes",
    },
    {
        id: "workflow.dynamic.agent-types",
        label: "agent types",
        classification: CapabilityClassification.DYNAMIC_REFERENCE,
        support: CapabilitySupport.SUPPORTED,
        origin: CapabilityOrigin.LIVE_CONFIGURATION,
        signature: null,
        optionShape: null,
        constraints: ["live values must not be copied into static contract data"],
        runtimeBinding: null,
        detailsAnchor: "agent-types",
        dynamicReference: "agent-types",
    },
];
/** Authoritative versioned inventory used by runtime assembly and every static projection. */
export const WORKFLOW_CAPABILITY_DEFINITION = {
    versions: {
        format: { kind: "present-at", version: "2.1.0" },
        content: { kind: "present-at", version: packageJson.version },
    },
    optionShapes: [AGENT_OPTIONS],
    capabilities,
    dynamicReferences: [
        {
            id: "model-routes",
            owner: "model-tier-config",
            itemShape: "{ name: string; description?: string }",
            connection: "loadModelTierConfig",
        },
        {
            id: "agent-types",
            owner: "agent-registry",
            itemShape: "{ name: string; description?: string }",
            connection: "loadAgentRegistry",
        },
    ],
};
/** Validate and freeze a definition, throwing with diagnostics when its identities or references conflict. */
export function defineWorkflowCapabilityContract(definition) {
    deepFreeze(definition);
    const definitionDiagnostics = validateDefinition(definition);
    if (definitionDiagnostics.length > 0) {
        throw new WorkflowCapabilityContractError("invalid workflow capability definition", definitionDiagnostics);
    }
    const optionShapes = new Map(definition.optionShapes.map((shape) => [shape.id, shape]));
    const dynamicReferences = new Map(definition.dynamicReferences.map((reference) => [reference.id, reference]));
    const bindings = definition.capabilities.flatMap((capability) => capability.runtimeBinding ? [{ ...capability.runtimeBinding }] : []);
    const implementations = new Set(bindings.map((binding) => binding.implementation));
    const globals = new Set(bindings.map((binding) => binding.global));
    const diagnoseAlignment = (evidence) => {
        const diagnostics = [];
        if (evidence.suppliedImplementations) {
            for (const binding of bindings) {
                if (!Object.hasOwn(evidence.suppliedImplementations, binding.implementation) ||
                    (evidence.suppliedImplementations[binding.implementation] === undefined && !binding.allowsUndefined)) {
                    diagnostics.push({
                        code: "MISSING_RUNTIME_IMPLEMENTATION",
                        severity: DiagnosticSeverity.ERROR,
                        subject: binding.implementation,
                        message: `Declared workflow global "${binding.global}" has no supplied implementation "${binding.implementation}".`,
                    });
                }
            }
            for (const name of Object.keys(evidence.suppliedImplementations)) {
                if (!implementations.has(name)) {
                    diagnostics.push({
                        code: "UNDECLARED_RUNTIME_IMPLEMENTATION",
                        severity: DiagnosticSeverity.WARNING,
                        subject: name,
                        message: `Supplied runtime implementation "${name}" is undeclared and was ignored.`,
                    });
                }
            }
        }
        if (evidence.observedProjectGlobals) {
            const observed = new Set(evidence.observedProjectGlobals);
            for (const name of globals) {
                if (!observed.has(name)) {
                    diagnostics.push({
                        code: "DECLARED_GLOBAL_UNOBSERVED",
                        severity: DiagnosticSeverity.ERROR,
                        subject: name,
                        message: `Declared workflow global "${name}" was not observed in the assembled context.`,
                    });
                }
            }
            for (const name of observed) {
                if (!globals.has(name)) {
                    diagnostics.push({
                        code: "OBSERVED_GLOBAL_UNDECLARED",
                        severity: DiagnosticSeverity.ERROR,
                        subject: name,
                        message: `Observed project-owned workflow global "${name}" is undeclared.`,
                    });
                }
            }
        }
        return diagnostics;
    };
    return {
        definition,
        assembleRuntimeBindings(supplied) {
            const diagnostics = diagnoseAlignment({ suppliedImplementations: supplied });
            const missing = diagnostics.filter((diagnostic) => diagnostic.code === "MISSING_RUNTIME_IMPLEMENTATION");
            if (missing.length > 0) {
                throw new WorkflowCapabilityContractError(`missing declared runtime implementation: ${missing.map((diagnostic) => diagnostic.subject).join(", ")}`, diagnostics);
            }
            const assembled = {};
            for (const binding of bindings)
                assembled[binding.global] = supplied[binding.implementation];
            return { globals: assembled, diagnostics };
        },
        projectStaticReferenceFacts() {
            return definition.capabilities.map((capability) => ({
                id: capability.id,
                label: capability.label,
                classification: capability.classification,
                support: capability.support,
                signature: capability.signature,
                options: capability.optionShape ? (optionShapes.get(capability.optionShape) ?? null) : null,
                constraints: capability.constraints,
                reference: `${CAPABILITY_DETAIL_PATH}#${capability.detailsAnchor}`,
                dynamicReference: capability.dynamicReference
                    ? (dynamicReferences.get(capability.dynamicReference) ?? null)
                    : null,
            }));
        },
        diagnoseAlignment,
    };
}
function validateDefinition(definition) {
    const diagnostics = [];
    const ids = new Set();
    const anchors = new Set();
    const globals = new Set();
    const runtimeImplementations = new Set();
    const optionShapes = new Set();
    const dynamicReferences = new Set();
    const invalid = (subject, message) => diagnostics.push({ code: "INVALID_CAPABILITY_DEFINITION", severity: DiagnosticSeverity.ERROR, subject, message });
    for (const shape of definition.optionShapes) {
        if (optionShapes.has(shape.id))
            invalid(shape.id, `Duplicate option shape "${shape.id}".`);
        optionShapes.add(shape.id);
    }
    for (const reference of definition.dynamicReferences) {
        if (dynamicReferences.has(reference.id))
            invalid(reference.id, `Duplicate dynamic reference "${reference.id}".`);
        dynamicReferences.add(reference.id);
    }
    for (const capability of definition.capabilities) {
        if (ids.has(capability.id))
            invalid(capability.id, `Duplicate capability id "${capability.id}".`);
        ids.add(capability.id);
        // Generated docs link to these anchors, so each must name exactly one section.
        if (anchors.has(capability.detailsAnchor)) {
            invalid(capability.id, `Duplicate details anchor "${capability.detailsAnchor}".`);
        }
        anchors.add(capability.detailsAnchor);
        if (capability.classification === CapabilityClassification.RUNTIME_GLOBAL && !capability.runtimeBinding) {
            invalid(capability.id, "Runtime-global capabilities require a runtime binding.");
        }
        if (capability.runtimeBinding) {
            if (globals.has(capability.runtimeBinding.global)) {
                invalid(capability.runtimeBinding.global, `Duplicate runtime global "${capability.runtimeBinding.global}".`);
            }
            globals.add(capability.runtimeBinding.global);
            if (runtimeImplementations.has(capability.runtimeBinding.implementation)) {
                invalid(capability.runtimeBinding.implementation, `Duplicate runtime implementation identity "${capability.runtimeBinding.implementation}".`);
            }
            runtimeImplementations.add(capability.runtimeBinding.implementation);
            if (capability.classification !== CapabilityClassification.RUNTIME_GLOBAL ||
                capability.origin !== CapabilityOrigin.PROJECT) {
                invalid(capability.id, "Runtime bindings require runtime-global classification and project origin.");
            }
        }
        if (capability.optionShape && !optionShapes.has(capability.optionShape)) {
            invalid(capability.id, `Unknown option shape "${capability.optionShape}".`);
        }
        if (capability.dynamicReference && !dynamicReferences.has(capability.dynamicReference)) {
            invalid(capability.id, `Unknown dynamic reference "${capability.dynamicReference}".`);
        }
    }
    return diagnostics;
}
/** Installed validated workflow capability contract. */
export const WORKFLOW_CAPABILITY_CONTRACT = defineWorkflowCapabilityContract(WORKFLOW_CAPABILITY_DEFINITION);
