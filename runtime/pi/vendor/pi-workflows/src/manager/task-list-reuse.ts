// Maps the unchanged tasks of an edited task list onto the calls of the original run.

import type { PersistedAgentState, PersistedRunState } from "../run-persistence.js";
import { agentCallId } from "../runtime/call-identity.js";
import { isTaskListScript, parseSubagentTasks, type SubagentTask, unchangedTaskMoves } from "../subagent-tasks.js";

/** Unchanged tasks of an edited task list: old → new call index, and old → new agent call id. */
export interface TaskListMoves {
  indices: Map<number, number>;
  callIds: Map<string, string>;
}

/** Where each unchanged task moved. Throws when the run did not start from a task list, or a script edit is also given. */
export function taskListMoves(
  runId: string,
  persisted: PersistedRunState,
  edit: { script?: string; tasks?: SubagentTask[] },
): TaskListMoves {
  if (edit.script !== undefined) throw new Error("resume takes an edited task list or an edited script, not both");
  if (!isTaskListScript(persisted.script)) {
    throw new Error(`run ${JSON.stringify(runId)} did not start from a task list; resume it with an edited script`);
  }
  const indices = unchangedTaskMoves(parseSubagentTasks(persisted.args), parseSubagentTasks(edit.tasks));
  const callIds = new Map([...indices].map(([from, to]) => [agentCallId(runId, from), agentCallId(runId, to)]));
  return { indices, callIds };
}

/**
 * Point a reused task's agent row at its new call id so replay updates that
 * row; a changed or dropped task's row keeps its history but no call id.
 */
export function retargetAgentRow(agent: PersistedAgentState, moves: TaskListMoves): PersistedAgentState {
  if (!agent || typeof agent !== "object" || typeof agent.callId !== "string") return agent;
  const callId = moves.callIds.get(agent.callId);
  if (callId) return { ...agent, callId };
  const { callId: _dropped, ...history } = agent;
  return history;
}
