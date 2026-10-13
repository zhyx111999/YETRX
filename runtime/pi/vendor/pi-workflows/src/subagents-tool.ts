/**
 * The `subagents` tool: start a background run from a task list, a workflow
 * script, or a saved/built-in workflow name, and return its run ID at once.
 */

import { defineTool, type ToolDefinition } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import { BUILTIN_WORKFLOW_NAMES, resolveWorkflowInvocation } from "./builtin-workflows.js";
import { parseSubagentTasks, SUBAGENT_TASKS_SCRIPT, type SubagentTask } from "./subagent-tasks.js";
import type { WorkflowManager } from "./workflow-manager.js";
import type { WorkflowStorage } from "./workflow-saved.js";

export const SUBAGENTS_TOOL_NAME = "subagents";

/** The single always-on gate: it authorizes delegation without forcing it. */
export const SUBAGENTS_GATE_GUIDELINE =
  "Whether to call subagents is decided by the hidden note that follows the user message.";

const subagentsSchema = Type.Object({
  tasks: Type.Optional(
    Type.Array(
      Type.Object({
        label: Type.String({ description: "Unique name for read, message, and reuse." }),
        task: Type.String({ description: "Full instructions." }),
        model: Type.Optional(Type.String({ description: "Optional provider/model[:thinking]." })),
      }),
      { description: "One subagent per task, run in parallel." },
    ),
  ),
  script: Type.Optional(
    Type.String({
      description:
        "Multi-step JavaScript workflow (agent, parallel, pipeline). Load the workflow-authoring skill before writing one.",
    }),
  ),
  name: Type.Optional(
    Type.String({
      description: `Saved or built-in workflow (${BUILTIN_WORKFLOW_NAMES.join(", ")}); args per the workflow-patterns skill.`,
    }),
  ),
  // Explicitly typed: some tool-calling bridges flatten a property that has no
  // `type`, which would drop every named workflow's required args.
  args: Type.Optional(
    Type.Unsafe<Record<string, unknown>>({ type: "object", description: "args for script or name." }),
  ),
});

/** Exactly one way to start a run. */
export type SubagentsInput =
  | { tasks: SubagentTask[] }
  | { script: string; args?: Record<string, unknown> }
  | { name: string; args?: Record<string, unknown> };

/** Live host state, read at call time because the extension may rebuild it on session_start. */
export interface SubagentsToolHost {
  getManager(): WorkflowManager;
  getStorage(): WorkflowStorage;
  getCwd(): string;
}

export function createSubagentsTool(
  host: SubagentsToolHost,
): ToolDefinition<typeof subagentsSchema, { runId: string }> {
  return defineTool<typeof subagentsSchema, { runId: string }>({
    name: SUBAGENTS_TOOL_NAME,
    label: "Subagents",
    description:
      "Start background subagents with exactly one of tasks, script, or name. Returns a runId at once; the result is delivered to this conversation when the run finishes.",
    promptSnippet: "Delegate independent or multi-step work to background subagents",
    promptGuidelines: [SUBAGENTS_GATE_GUIDELINE],
    parameters: subagentsSchema,
    prepareArguments: normalizeSubagentsInput,
    async execute(_toolCallId, params) {
      const input = params as SubagentsInput;
      const manager = host.getManager();
      let runId: string;
      let started: string;
      if ("tasks" in input) {
        ({ runId } = manager.startInBackground(SUBAGENT_TASKS_SCRIPT, input.tasks));
        started = `${input.tasks.length} subagent${input.tasks.length === 1 ? "" : "s"}`;
      } else if ("name" in input) {
        const resolved = resolveWorkflowInvocation(input.name, input.args, {
          storage: host.getStorage(),
          cwd: host.getCwd(),
        });
        if (!resolved) {
          throw new Error(
            `no saved or built-in workflow named "${input.name}". Built-in names: ${BUILTIN_WORKFLOW_NAMES.join(", ")}.`,
          );
        }
        ({ runId } = manager.startInBackground(normalizeScript(resolved.script), input.args, {
          tools: resolved.tools,
          toolset: resolved.toolset,
        }));
        started = `workflow "${input.name}"`;
      } else {
        ({ runId } = manager.startInBackground(input.script, input.args));
        started = `workflow "${manager.getRun(runId)?.snapshot.name ?? runId}"`;
      }
      return {
        content: [{ type: "text", text: startedText(runId, started) }],
        details: { runId },
      };
    },
  });
}

/** Tool result for a started run: where the result goes and what the turn should do now. */
export function startedText(runId: string, started: string): string {
  return [
    `Started ${started} in the background as run ${runId}.`,
    "The result is delivered to this conversation automatically when it finishes, so this turn can end; tell the user they can wait or keep working.",
    "Use subagents_control to check, read, message, stop, or resume this run.",
  ].join("\n");
}

/** Strip one whole-script Markdown fence that models sometimes wrap code in. */
export function normalizeScript(script: string): string {
  let text = script.trim();
  const fence = text.match(/^```(?:js|javascript)?\s*\n([\s\S]*?)\n```$/i);
  if (fence) text = fence[1].trim();
  return text;
}

/** A field counts as given only when it holds a value: providers may fill unused fields with null or "". */
export function isGiven(value: unknown): boolean {
  if (value === undefined || value === null) return false;
  if (typeof value === "string") return value.trim() !== "";
  return !Array.isArray(value) || value.length > 0;
}

/** Parse optional `args`: a JSON object when given. */
function parseArgs(value: unknown): Record<string, unknown> | undefined {
  if (value === undefined || value === null) return undefined;
  if (typeof value !== "object" || Array.isArray(value)) throw new Error("subagents args must be a JSON object");
  return value as Record<string, unknown>;
}

const SUBAGENTS_FIELDS: ReadonlySet<string> = new Set(["tasks", "script", "name", "args"]);
const START_MODES = ["tasks", "script", "name"] as const;

/**
 * Validate one call. Unknown fields and a missing or ambiguous mode are errors;
 * `args` is dropped for a task list, which has no use for it.
 */
function normalizeSubagentsInput(value: unknown): SubagentsInput {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    throw new Error("subagents requires an object argument");
  }
  const input = value as Record<string, unknown>;
  const extra = Object.keys(input).find((key) => !SUBAGENTS_FIELDS.has(key));
  if (extra) throw new Error(`subagents does not accept ${extra}`);
  const modes = START_MODES.filter((key) => isGiven(input[key]));
  if (modes.length !== 1) throw new Error("subagents needs exactly one of tasks, script, or name");
  if (modes[0] === "tasks") return { tasks: parseSubagentTasks(input.tasks) };
  const args = parseArgs(input.args);
  const text = input[modes[0]];
  if (typeof text !== "string") throw new Error(`subagents ${modes[0]} must be a string`);
  const started = modes[0] === "script" ? { script: normalizeScript(text) } : { name: text.trim() };
  return args ? { ...started, args } : started;
}
