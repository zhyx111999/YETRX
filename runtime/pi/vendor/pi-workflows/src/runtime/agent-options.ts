/** Validates a script's agent() prompt and options against the capability contract before the call reserves a slot. */

import { realpathSync, statSync } from "node:fs";
import { isAbsolute } from "node:path";
import { WorkflowError, WorkflowErrorCode } from "../errors.js";
import type { AgentOptions } from "../workflow.js";
import {
  AGENT_OPTIONS,
  AGENT_PROMPT,
  type OptionDescriptor,
  type OptionValueRule,
} from "../workflow-capability-contract.js";

const AGENT_OPTION_NAMES: readonly string[] = AGENT_OPTIONS.options.map((option) => option.name);

/** A call whose prompt and options match their declarations, with thread and cwd normalized. */
export interface ParsedAgentCall {
  prompt: string;
  options: AgentOptions;
}

const invalidArgument = (message: string) =>
  new WorkflowError(message, WorkflowErrorCode.SCRIPT_VALIDATION_ERROR, { recoverable: false });

/**
 * Check every argument against the contract so a misspelled key or an
 * out-of-contract value fails before dispatch instead of silently changing
 * behavior, such as an unknown isolation value skipping the worktree.
 */
export function parseAgentCall(prompt: unknown, options: unknown): ParsedAgentCall {
  const parsedPrompt = parseValue(AGENT_PROMPT, prompt) as string;
  const accepted = `accepted options: ${AGENT_OPTION_NAMES.join(", ")}`;
  if (options === null || typeof options !== "object" || Array.isArray(options)) {
    throw invalidArgument(`agent() options must be an object; ${accepted}`);
  }
  const unknown = Object.keys(options).filter((key) => !AGENT_OPTION_NAMES.includes(key));
  if (unknown.length > 0) {
    throw invalidArgument(`agent() does not accept ${unknown.map((key) => `"${key}"`).join(", ")}; ${accepted}`);
  }
  const supplied = options as Record<string, unknown>;
  const parsed: Record<string, unknown> = {};
  for (const descriptor of AGENT_OPTIONS.options) {
    if (!Object.hasOwn(supplied, descriptor.name)) continue;
    const value = parseValue(descriptor, supplied[descriptor.name]);
    if (value !== undefined) parsed[descriptor.name] = value;
  }
  // The trimmed name is the thread's identity, so " a" and "a" share one conversation.
  if (typeof parsed.thread === "string") parsed.thread = parsed.thread.trim();
  return { prompt: parsedPrompt, options: parsed as AgentOptions };
}

/** The call's isolation wins and false opts out of an agentType default; any other value fails closed. */
export function resolveIsolation(
  requested: AgentOptions["isolation"],
  agentTypeDefault: "worktree" | undefined,
): "worktree" | undefined {
  switch (requested) {
    case undefined:
      return agentTypeDefault;
    case false:
      return undefined;
    case "worktree":
      return "worktree";
    default: {
      // parseAgentCall admits only contract values; `never` keeps this switch in step with the contract type.
      const unexpected: never = requested;
      throw invalidArgument(`agent() isolation ${describe(unexpected)} is not a contract value`);
    }
  }
}

/** The value to keep (a directory becomes its real path), or undefined when the argument was omitted. */
function parseValue(descriptor: OptionDescriptor, value: unknown): unknown {
  const { name, accepts: rule } = descriptor;
  if (value === undefined) {
    if (descriptor.optional) return undefined;
    throw invalidArgument(`agent() ${name} is required and must be ${expectation(rule)}`);
  }
  const reject = () => invalidArgument(`agent() ${name} must be ${expectation(rule)}; got ${describe(value)}`);
  switch (rule.kind) {
    case "string":
      if (typeof value !== "string" || (rule.nonEmpty && !value.trim())) throw reject();
      return value;
    case "boolean":
      if (typeof value !== "boolean") throw reject();
      return value;
    case "literal":
      if (!rule.values.includes(value as string | boolean)) throw reject();
      return value;
    case "json-schema":
      // Script objects come from the VM realm, so a prototype check against this realm would reject them.
      if (value === null || typeof value !== "object" || Array.isArray(value)) throw reject();
      return value;
    case "directory":
      return realDirectory(value, reject);
  }
}

/** The real path, so coding tools and the call's resume identity agree on one directory. */
function realDirectory(value: unknown, reject: () => WorkflowError): string {
  if (typeof value !== "string" || !value.trim() || !isAbsolute(value)) throw reject();
  try {
    const resolved = realpathSync(value);
    if (statSync(resolved).isDirectory()) return resolved;
  } catch {
    // A missing or unreadable path is rejected below, like a file.
  }
  throw reject();
}

function expectation(rule: OptionValueRule): string {
  switch (rule.kind) {
    case "string":
      return rule.nonEmpty ? "a non-empty string" : "a string";
    case "boolean":
      return "true or false";
    case "literal":
      return `one of: ${rule.values.map((value) => JSON.stringify(value)).join(", ")}`;
    case "json-schema":
      return "a plain JSON Schema object";
    case "directory":
      return "an absolute directory that exists";
  }
}

function describe(value: unknown): string {
  if (typeof value === "string") return JSON.stringify(value);
  if (value === null) return "null";
  if (Array.isArray(value)) return "an array";
  if (typeof value === "object" || typeof value === "function") return `a ${typeof value}`;
  return `${typeof value} ${String(value)}`;
}
