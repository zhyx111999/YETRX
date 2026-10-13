// Maps the unchanged tasks of an edited task list onto the calls of the original run.
import { agentCallId } from "../runtime/call-identity.js";
import { isTaskListScript, parseSubagentTasks, unchangedTaskMoves } from "../subagent-tasks.js";
/** Where each unchanged task moved. Throws when the run did not start from a task list, or a script edit is also given. */
export function taskListMoves(runId, persisted, edit) {
    if (edit.script !== undefined)
        throw new Error("resume takes an edited task list or an edited script, not both");
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
export function retargetAgentRow(agent, moves) {
    if (!agent || typeof agent !== "object" || typeof agent.callId !== "string")
        return agent;
    const callId = moves.callIds.get(agent.callId);
    if (callId)
        return { ...agent, callId };
    const { callId: _dropped, ...history } = agent;
    return history;
}
