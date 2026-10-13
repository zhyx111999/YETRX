// One snapshot row per agent() call: a later execution of a call takes over the call's row.
/**
 * The id for a new row. A superseding row replaces a row rather than being
 * appended, so the row count is not a free id; ids stay above every existing one.
 */
export function nextAgentRowId(agents) {
    let max = 0;
    for (const agent of agents)
        max = Math.max(max, agent.id);
    return max + 1;
}
/**
 * `next` as the call's row, keeping `previous` (an earlier execution of the
 * same call, with its own history) as prior executions, oldest first.
 * `timestamps` are previous's, which the snapshot keeps outside the row.
 */
export function supersedeAgentRow(previous, timestamps, next) {
    return {
        ...next,
        priorExecutions: [
            ...(previous.priorExecutions ?? []),
            priorExecution(previous, timestamps),
            ...(next.priorExecutions ?? []),
        ],
    };
}
/** What stays of a superseded execution: how it ended, what it spent, and where its session is. */
function priorExecution(row, timestamps) {
    return {
        label: row.label,
        status: row.status,
        resultPreview: row.resultPreview,
        error: row.error,
        errorCode: row.errorCode,
        model: row.model,
        sessionId: row.sessionId,
        sessionFile: row.sessionFile,
        tokens: row.tokens,
        tokenUsage: row.tokenUsage,
        startedAt: timestamps?.startedAt,
        endedAt: timestamps?.endedAt,
    };
}
