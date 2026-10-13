import { WorkflowErrorCode } from "./errors.js";
export const INTERRUPTED_AGENT_CAUSE = { error: "interrupted", errorCode: WorkflowErrorCode.WORKFLOW_ABORTED };
export function agentHasNonTerminalStatus(status) {
    return status === "queued" || status === "running";
}
/** Display-only settlement; replay remains keyed by the committed journal. */
export function settleInterruptedPersistedAgents(agents, cause, endedAt) {
    return agents.map((agent) => !agentHasNonTerminalStatus(agent.status)
        ? agent
        : {
            ...agent,
            status: "skipped",
            error: cause.error,
            errorCode: cause.errorCode,
            recoverable: false,
            endedAt: agent.endedAt ?? endedAt,
        });
}
