/** Hashes agent() and checkpoint() calls into the identities that decide whether a journaled result may replay. */

import { createHash } from "node:crypto";
import type { AgentOptions, WorkflowCheckpointInput } from "../workflow.js";

/** Everything that, when edited, must make a journaled agent() result stale. */
interface AgentCallIdentity {
  prompt: string;
  /** Spec-level model (explicit, definition or phase); the resolved model is not part of the key. */
  model: string | undefined;
  phase: string | undefined;
  options: AgentOptions;
  /** Resolved agentType definition (tools/model/prompt), so editing an agent .md invalidates the call. */
  agentDefinitionKey: string | null;
  cwd: string | undefined;
  isolation: "worktree" | undefined;
}

export function hashAgentCall(call: AgentCallIdentity): string {
  const { options } = call;
  // Spread-in fields are omitted when unset instead of written as null, so a
  // call that does not use them keeps its hash when an option is added.
  const identity = JSON.stringify({
    prompt: call.prompt,
    model: call.model ?? null,
    tier: options.tier ?? null,
    ...(options.thinking ? { thinking: options.thinking } : {}),
    phase: call.phase ?? null,
    agentType: options.agentType ?? null,
    ...(options.thread ? { thread: options.thread } : {}),
    agentDef: call.agentDefinitionKey,
    schema: options.schema ?? null,
    ...(call.cwd === undefined ? {} : { cwd: call.cwd }),
    ...(options.isolation !== undefined ? { isolation: options.isolation } : {}),
    ...(call.isolation === "worktree" ? { keepWorktree: options.keepWorktree !== false } : {}),
  });
  return createHash("sha256").update(identity).digest("hex");
}

export function hashCheckpointCall(input: WorkflowCheckpointInput): string {
  return createHash("sha256").update(JSON.stringify(input)).digest("hex");
}

/**
 * Identity of one agent() call: its store delta key, the id carried by every
 * onAgent* event, and its persisted agent row's callId. Prefixed with the run
 * id so it stays unique when hosts aggregate agents across runs.
 */
export function agentCallId(runId: string, callIndex: number): string {
  return `${runId}:${callIndex}`;
}
