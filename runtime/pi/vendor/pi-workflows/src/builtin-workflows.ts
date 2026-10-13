/**
 * Shared registry of the built-in workflow patterns
 * (`deep-research`). Review and audit patterns are not registered.
 *
 * This is the single place that turns a pattern's name + caller-supplied args
 * into a runnable script (and, where a pattern needs it, an exec context such
 * as web tools). Both entry points a model or user can reach a built-in
 * through — the `/deep-research`-style slash commands (builtin-commands.ts)
 * and the `subagents` tool's `name` input (subagents-tool.ts) — resolve through
 * this one registry, so the two paths can never drift apart and the
 * per-pattern generator scripts are written exactly once.
 */

import { createCodingTools, type ToolDefinition } from "@earendil-works/pi-coding-agent";
import { DEEP_RESEARCH_MAX_ANGLES, generateDeepResearchWorkflow } from "./deep-research.js";
import { createWebTools } from "./web-tools.js";
import type { WorkflowStorage } from "./workflow-saved.js";

/** A resolved, ready-to-run script plus the exec context it needs (if any). */
export interface BuiltinWorkflowInvocation {
  script: string;
  tools?: ToolDefinition[];
  toolset?: string;
}

export interface BuiltinWorkflowDescriptor {
  /** Also the slash-command name (without the leading `/`). */
  name: string;
  description: string;
  /** Build the script (and exec context) for one invocation; throws on invalid `args`. */
  resolve(cwd: string, args: unknown): BuiltinWorkflowInvocation;
}

function asRecord(args: unknown): Record<string, unknown> {
  return args && typeof args === "object" ? (args as Record<string, unknown>) : {};
}

function requireNonEmptyString(value: unknown, argName: string, patternName: string): string {
  if (typeof value !== "string" || !value.trim()) {
    throw new Error(`Built-in workflow "${patternName}" requires args.${argName} to be a non-empty string.`);
  }
  return value;
}

/**
 * An optional integer arg must be omitted or an integer in [min, max]; a null
 * counts as omitted because some providers fill every optional field. Strings,
 * fractions, NaN and Infinity are rejected rather than coerced, so a bad value
 * fails before the run starts instead of silently planning zero queries.
 */
function requireOptionalIntegerInRange(
  value: unknown,
  [min, max]: readonly [number, number],
  argName: string,
  patternName: string,
): void {
  if (value === undefined || value === null) return;
  if (typeof value !== "number" || !Number.isInteger(value) || value < min || value > max) {
    throw new Error(
      `Built-in workflow "${patternName}" requires args.${argName}, when given, to be an integer from ${min} to ${max}.`,
    );
  }
}

/** Built-in workflow patterns, keyed by their stable name. */
export const BUILTIN_WORKFLOWS: readonly BuiltinWorkflowDescriptor[] = [
  {
    name: "deep-research",
    description: `Research a question across the web and return sourced claims per query. args: { question: string, angles?: integer 1-${DEEP_RESEARCH_MAX_ANGLES} }.`,
    resolve(cwd, args) {
      const { question, angles } = asRecord(args);
      requireNonEmptyString(question, "question", "deep-research");
      requireOptionalIntegerInRange(angles, [1, DEEP_RESEARCH_MAX_ANGLES], "angles", "deep-research");
      return {
        script: generateDeepResearchWorkflow(),
        // Research agents need real web access on top of the coding tools; the
        // "web-research" tag is what a resumed run re-resolves (see
        // WorkflowManagerOptions.toolsets).
        tools: [...createCodingTools(cwd), ...createWebTools()],
        toolset: "web-research",
      };
    },
  },
];

/** Stable list of built-in workflow pattern names, in registry order. */
export const BUILTIN_WORKFLOW_NAMES: readonly string[] = BUILTIN_WORKFLOWS.map((w) => w.name);

export function findBuiltinWorkflow(name: string): BuiltinWorkflowDescriptor | undefined {
  return BUILTIN_WORKFLOWS.find((w) => w.name === name);
}

/**
 * Resolve a name to a runnable invocation, checking project/user saved
 * workflows first and falling back to the built-in patterns — the same
 * precedence `workflow-saved.ts` already uses internally (project > user), one
 * level up: saved workflows (of either scope) beat a built-in of the same name.
 */
export function resolveWorkflowInvocation(
  name: string,
  args: unknown,
  ctx: { storage: WorkflowStorage; cwd: string },
): BuiltinWorkflowInvocation | undefined {
  const saved = ctx.storage.load(name);
  if (saved) return { script: saved.script, toolset: saved.toolset };
  const builtin = findBuiltinWorkflow(name);
  if (builtin) return builtin.resolve(ctx.cwd, args);
  return undefined;
}
