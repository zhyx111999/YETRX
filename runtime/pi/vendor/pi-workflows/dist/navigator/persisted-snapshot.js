/** Rebuilds a display snapshot from a persisted run record so finished and other-session runs can be browsed. */
import { RunRecordUnreadableError } from "../errors.js";
import { asText } from "./as-text.js";
export function persistedToSnapshot(p) {
    try {
        return rebuildSnapshot(p);
    }
    catch (error) {
        if (!(error instanceof RunRecordUnreadableError))
            throw error;
        // A listed record whose log is damaged: show why its details are missing.
        return {
            name: asText(p.workflowName),
            phases: [],
            logs: [`${error.message}. Remove it with /workflows rm ${p.runId}`],
            agents: [],
            agentCount: 0,
            runningCount: 0,
            doneCount: 0,
            errorCount: 0,
            runId: p.runId,
        };
    }
}
function rebuildSnapshot(p) {
    // Corrupt arrays must not crash the overlay. Resumable runs keep each full
    // result only in the journal, not also in agents[], so done agents are
    // rehydrated by call identity. A row without one (an execution a later one
    // superseded) keeps only its own result.
    const agents = (Array.isArray(p.agents) ? p.agents : []).filter((agent) => agent && typeof agent === "object");
    const journalByCallId = new Map();
    for (const entry of Array.isArray(p.journal) ? p.journal : []) {
        if (entry && typeof entry === "object" && typeof entry.index === "number") {
            journalByCallId.set(`${p.runId}:${entry.index}`, entry.result);
        }
    }
    const snapshotAgents = agents.map((a) => {
        const journalResult = a.callId ? journalByCallId.get(a.callId) : undefined;
        const result = a.result === undefined && a.status === "done" ? journalResult : a.result;
        // The record's row is the display row plus its timestamps, which the snapshot keeps elsewhere.
        const { startedAt: _startedAt, endedAt: _endedAt, ...row } = a;
        return {
            ...row,
            result,
            // A string result is its own preview; anything else is stringified once
            // here, and the model caches this snapshot per record.
            resultPreview: result === undefined ? a.resultPreview : String(typeof result === "string" ? result : JSON.stringify(result)),
        };
    });
    return {
        name: asText(p.workflowName),
        phases: Array.isArray(p.phases) ? p.phases : [],
        currentPhase: p.currentPhase,
        logs: Array.isArray(p.logs) ? p.logs : [],
        agents: snapshotAgents,
        agentCount: snapshotAgents.length,
        runningCount: snapshotAgents.filter((a) => a.status === "running").length,
        doneCount: snapshotAgents.filter((a) => a.status === "done").length,
        errorCount: snapshotAgents.filter((a) => a.status === "error").length,
        tokenUsage: p.tokenUsage ? { ...p.tokenUsage } : undefined,
        runId: p.runId,
    };
}
