import { applyToolPolicy } from "../agent-registry.js";
import { WorkflowError, WorkflowErrorCode } from "../errors.js";
import { createStructuredOutputTool } from "../structured-output.js";
/**
 * Orchestration tools always denied to workflow subagents. The extension
 * registers `subagents` and `subagents_control` globally, so a subagent would
 * otherwise see them and start or steer background runs of its own. Those
 * nested runs fan out recursively outside the parent run's maxAgents,
 * concurrency, progress, and accounting, and can drain a shared provider quota.
 */
export const DEFAULT_EXCLUDED_SUBAGENT_TOOLS = ["subagents", "subagents_control"];
/**
 * The full denylist: the always-on defaults, then names set on the injected
 * session options, then the caller's extra names. The defaults come first so
 * no option can drop them; the SDK dedupes overlap.
 */
export function subagentExcludedTools(extra, sessionExclude) {
    return [...DEFAULT_EXCLUDED_SUBAGENT_TOOLS, ...(sessionExclude ?? []), ...(extra ?? [])];
}
/**
 * The custom tools for one call. The agent type's policy runs before
 * structured_output is added, so a restrictive allowlist never strips the
 * schema tool.
 */
export function buildSubagentTools(request) {
    const tools = applyToolPolicy([...request.codingTools, ...(request.tools ?? [])], request.toolNames, request.disallowedToolNames);
    if (request.systemTools?.length) {
        tools.push(...request.systemTools);
    }
    if (request.schema) {
        // Strict OpenAI-compatible providers (e.g. DeepSeek) reject a tool whose
        // parameters are not a JSON object with a transport-level 400, before any
        // output classification runs. Fail fast with a clear workflow error instead.
        const schemaType = request.schema.type;
        if (schemaType !== "object") {
            throw new WorkflowError(`agent() opts.schema must be a top-level JSON object schema (type: "object") — got type: ${schemaType ?? "undefined"}; wrap array/primitive results in an object, e.g. { type: "object", properties: { items: <your schema> } }`, WorkflowErrorCode.SCRIPT_VALIDATION_ERROR, { recoverable: false });
        }
        tools.push(createStructuredOutputTool({ schema: request.schema, capture: request.capture }));
    }
    return tools;
}
