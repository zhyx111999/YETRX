/**
 * The `subagents_control` tool: list, inspect, read, stop, resume, and message
 * background runs started by `subagents`.
 */
import { defineTool } from "@earendil-works/pi-coding-agent";
import { Type } from "typebox";
import { errorMessage } from "./errors.js";
import { AGENT_TAIL_CHARS, agentStatuses, asTailAgent, formatAgentStatuses, formatRunSummary, renderAgentTail, selectTailAgent, summarizeRun, waitingCheckpoint, } from "./run-report.js";
import { parseSubagentTasks } from "./subagent-tasks.js";
import { isGiven, normalizeScript } from "./subagents-tool.js";
import { parseWorkflowScript } from "./workflow.js";
export const SUBAGENTS_CONTROL_TOOL_NAME = "subagents_control";
const ACTIONS = ["list", "status", "read", "stop", "resume", "message"];
const subagentsControlSchema = Type.Object({
    // A string enum, not a union of literals: the same check in a fraction of the bytes.
    action: Type.Unsafe({ type: "string", enum: [...ACTIONS] }),
    // The description documents each action's fields, so most fields carry none.
    runId: Type.Optional(Type.String()),
    label: Type.Optional(Type.String()),
    text: Type.Optional(Type.String()),
    tasks: Type.Optional(Type.Unsafe({ type: "array", items: { type: "object" }, description: "Edited subagents tasks." })),
    script: Type.Optional(Type.String({ description: "Edited script." })),
    checkpointId: Type.Optional(Type.String()),
    response: Type.Optional(Type.Unknown({ description: "Any JSON answer." })),
}, { additionalProperties: false });
export function createSubagentsControlTool(host) {
    return defineTool({
        name: SUBAGENTS_CONTROL_TOOL_NAME,
        label: "Subagents control",
        description: [
            "Manage subagents runs. Actions: list;",
            "status runId (each subagent's state, the phase, a waiting checkpoint);",
            `read [runId] [label] (last ${AGENT_TAIL_CHARS} chars of one subagent; default: latest run, latest updated subagent);`,
            "stop runId;",
            "resume runId (continue a paused, failed, or stopped run; rerun any run with edited tasks or script, reusing unchanged work; answer a checkpoint with checkpointId and response);",
            "message runId label text (instruct a running subagent).",
        ].join(" "),
        parameters: subagentsControlSchema,
        prepareArguments: normalizeControlInput,
        async execute(_toolCallId, params) {
            const input = params;
            const manager = host.getManager();
            if (input.action === "list")
                return listRuns(manager);
            if (input.action === "read")
                return readAgent(manager, input.runId, input.label);
            const run = manager.findRun(input.runId);
            if (!run)
                return controlError(input.action, input.runId, "run not found", ["list"]);
            try {
                switch (input.action) {
                    case "status":
                        return statusOf(manager, run);
                    case "stop":
                        if (!manager.stop(run.runId))
                            return refused(manager, "stop", run);
                        return actionResult("stop", "stopped", currentSummary(manager, run));
                    case "resume":
                        return await resumeRun(manager, run, input);
                    case "message":
                        return await messageAgent(manager, run, input.label, input.text);
                }
            }
            catch (error) {
                // A manager or persistence failure is reported in the tool's own
                // result shape rather than as a raw stack trace.
                const message = errorMessage(error);
                return controlError(input.action, run.runId, message, allowedActions(run.status));
            }
        },
    });
}
function listRuns(manager) {
    const runs = manager.listRuns().map((run) => summarizeRun(run, manager.getSnapshot(run.runId)));
    const lines = runs.map(formatRunSummary);
    return result([`action=list result=ok runs=${runs.length}`, ...lines].join("\n"), {
        action: "list",
        result: "ok",
        runs,
    });
}
function statusOf(manager, run) {
    const live = manager.getSnapshot(run.runId);
    const summary = summarizeRun(run, live);
    const agents = agentStatuses(run, live);
    const checkpoint = waitingCheckpoint(run);
    const lines = [`action=status result=ok ${formatRunSummary(summary)}`];
    if (agents.length)
        lines.push("agents:", formatAgentStatuses(agents));
    if (checkpoint) {
        lines.push(`waiting at checkpoint ${checkpoint.checkpointId} (${checkpoint.kind}): ${checkpoint.payload}`, "Answer with resume, this checkpointId, and a response.");
    }
    return result(lines.join("\n"), {
        action: "status",
        result: "ok",
        run: summary,
        agents,
        waitingCheckpoint: checkpoint,
    });
}
function readAgent(manager, runId, label) {
    const run = runId
        ? manager.findRun(runId)
        : manager
            .listRuns()
            .slice()
            .sort((a, b) => (a.startedAt < b.startedAt ? 1 : -1))[0];
    if (!run) {
        return controlError("read", runId ?? "", runId ? "run not found" : "no runs in this session", ["list"]);
    }
    const live = manager.getSnapshot(run.runId)?.agents.map(asTailAgent);
    const agents = live?.length ? live : (Array.isArray(run.agents) ? run.agents : []).map(asTailAgent);
    const agent = selectTailAgent(agents, label);
    if (!agent) {
        const labels = [...new Set(agents.map((item) => item.label))].join(", ") || "(none)";
        const message = label ? `no subagent labeled ${JSON.stringify(label)}; labels: ${labels}` : "no subagents yet";
        return controlError("read", run.runId, message, ["status"]);
    }
    return result(renderAgentTail(agent), {
        action: "read",
        result: "ok",
        runId: run.runId,
        label: agent.label,
        status: agent.status,
    });
}
/**
 * Resume a run, taking its result delivery into this session: the model that
 * resumes it is the one waiting for it, even when another session started it.
 * The run keeps that session as its parent lineage.
 */
async function resumeRun(manager, run, input) {
    let resumed;
    let reused;
    if ("checkpointId" in input) {
        // The answer is durable before the run continues, so a crash in between keeps it.
        await manager.attachCheckpointResponse(run.runId, input.checkpointId, input.response);
        resumed = await manager.resume(run.runId, { checkpointId: input.checkpointId, claimDelivery: true });
        reused = "It continues from the answered checkpoint.";
    }
    else if (input.tasks) {
        resumed = await manager.resume(run.runId, { tasks: input.tasks, claimDelivery: true });
        reused = "Unchanged tasks reuse their results; edited and new tasks run.";
    }
    else if (input.script) {
        resumed = await manager.resume(run.runId, { script: input.script, claimDelivery: true });
        reused = "Unchanged agent() calls before the first edit reuse their results; the rest run.";
    }
    else {
        resumed = await manager.resume(run.runId, { claimDelivery: true });
        reused = "Finished work is reused.";
    }
    if (!resumed)
        return refused(manager, "resume", run);
    // Judged from the record: the move can lose to a busy record, and an
    // already-answered checkpoint continues where another process resumed it.
    const current = manager.findRun(run.runId) ?? run;
    const summary = summarizeRun(current, manager.getSnapshot(current.runId));
    const here = manager.getSessionId();
    const delivery = (current.sessionId ?? here) === here
        ? "The result is delivered here when the run finishes."
        : `The result is delivered to session ${current.sessionId}, which owns the run, not here.`;
    return result(`action=resume result=resumed ${formatRunSummary(summary)}\n${reused} ${delivery}`, {
        action: "resume",
        result: "resumed",
        run: summary,
    });
}
async function messageAgent(manager, run, label, text) {
    const delivered = await manager.steerAgent(run.runId, label, text);
    if (delivered === 0) {
        return controlError("message", run.runId, `no running subagent labeled ${JSON.stringify(label)}; message reaches only running subagents, so change finished work with resume and an edited task list or script`, allowedActions(run.status));
    }
    return result(`action=message result=queued runId=${run.runId} label=${JSON.stringify(label)} subagents=${delivered}\nIt reaches the subagent before its next model call; if the subagent finishes first, the instruction is dropped.`, { action: "message", result: "queued", runId: run.runId, label, subagents: delivered });
}
function currentSummary(manager, fallback) {
    const current = manager.findRun(fallback.runId) ?? fallback;
    return summarizeRun(current, manager.getSnapshot(current.runId));
}
function result(text, details) {
    return { content: [{ type: "text", text }], details };
}
function actionResult(action, outcome, run) {
    return result(`action=${action} result=${outcome} ${formatRunSummary(run)}`, { action, result: outcome, run });
}
/**
 * Why a stop or resume was refused, phrased as what to do instead. Judged from
 * the record as it is now: another process may have deleted, finished, or
 * resumed the run since it was looked up.
 */
function refused(manager, action, attempted) {
    const run = manager.findRun(attempted.runId);
    if (!run)
        return controlError(action, attempted.runId, "run not found", ["list"]);
    const busy = "it is busy in another process or still settling; try again shortly";
    // A refused stop of a running or paused run means another process holds its lease.
    const reasons = action === "resume"
        ? {
            running: "it is still running; message it, or stop it first",
            completed: "it already finished; pass edited tasks or script to rerun it, reusing unchanged work",
        }
        : { running: "it is running in another session; stop it there", paused: busy };
    const reason = reasons[run.status] ?? (action === "resume" ? busy : `cannot stop a ${run.status} run`);
    return controlError(action, run.runId, reason, allowedActions(run.status));
}
function controlError(action, runId, message, allowed) {
    return result(`action=${action} result=error runId=${runId} error=${message} allowed=${allowed.join(",") || "none"}`, { action, result: "error", runId, error: message, allowedActions: allowed });
}
function allowedActions(status) {
    switch (status) {
        case "running":
            return ["status", "read", "message", "stop"];
        case "paused":
            return ["status", "read", "resume", "stop"];
        case "failed":
        case "pending":
        case "completed":
        case "aborted":
            return ["status", "read", "resume"];
    }
}
const CONTROL_FIELDS = new Set([
    "action",
    "runId",
    "label",
    "text",
    "tasks",
    "script",
    "checkpointId",
    "response",
]);
/**
 * Validate one request. Providers can fill every optional field, so declared
 * fields the action does not use are dropped, and null or blank values count
 * as omitted. Unknown fields and missing required fields are errors.
 */
function normalizeControlInput(value) {
    if (!value || typeof value !== "object" || Array.isArray(value)) {
        throw new Error("subagents_control requires an object argument");
    }
    const input = value;
    const action = input.action;
    if (typeof action !== "string" || !ACTIONS.includes(action)) {
        throw new Error(`subagents_control requires action: ${ACTIONS.join("|")}`);
    }
    const extra = Object.keys(input).find((key) => !CONTROL_FIELDS.has(key));
    if (extra)
        throw new Error(`subagents_control does not accept ${extra}`);
    const text = (field, required) => {
        const raw = input[field];
        if (!isGiven(raw)) {
            if (required)
                throw new Error(`subagents_control action "${action}" requires ${field}`);
            return undefined;
        }
        if (typeof raw !== "string")
            throw new Error(`subagents_control ${field} must be a string`);
        return field === "text" ? raw : raw.trim();
    };
    switch (action) {
        case "list":
            return { action: "list" };
        case "status":
        case "stop":
            return { action: action, runId: text("runId", true) };
        case "read": {
            const runId = text("runId", false);
            const label = text("label", false);
            return { action: "read", ...(runId ? { runId } : {}), ...(label ? { label } : {}) };
        }
        case "message":
            return {
                action: "message",
                runId: text("runId", true),
                label: text("label", true),
                text: text("text", true),
            };
        case "resume":
            return normalizeResume(input, text("runId", true), text);
    }
}
function normalizeResume(input, runId, text) {
    const checkpointId = text("checkpointId", false);
    const edits = ["tasks", "script"].filter((field) => isGiven(input[field]));
    if (edits.length + (checkpointId ? 1 : 0) > 1) {
        throw new Error("subagents_control resume takes one of tasks, script, or checkpointId with response");
    }
    if (checkpointId) {
        if (input.response === undefined) {
            throw new Error("subagents_control resume with checkpointId requires response");
        }
        return { action: "resume", runId, checkpointId, response: input.response };
    }
    if (edits[0] === "tasks")
        return { action: "resume", runId, tasks: parseSubagentTasks(input.tasks) };
    if (edits[0] === "script") {
        const script = normalizeScript(text("script", true));
        // Reject a malformed script here, before the run's lease is taken.
        parseWorkflowScript(script);
        return { action: "resume", runId, script };
    }
    return { action: "resume", runId };
}
