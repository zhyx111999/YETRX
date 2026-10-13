// Creates the in-memory state of a run that is about to start.

import { createWorkflowSnapshot } from "../display.js";
import { generateRunId } from "../run-persistence.js";
import { parseWorkflowScript } from "../workflow.js";
import type { ManagedRun } from "../workflow-manager.js";

/** A run id prefixed with a slug of the workflow name, so ids stay recognizable. */
function newRunId(name: string | undefined): string {
  const slug = name
    ? name
        .toLowerCase()
        .replace(/[^a-z0-9]+/g, "-")
        .replace(/^-+|-+$/g, "")
        .slice(0, 40) || "workflow"
    : "";
  return slug ? `${slug}-${generateRunId()}` : generateRunId();
}

export interface NewRunOwner {
  /** The session that started the run; it owns delivery and becomes the parent lineage. */
  sessionId?: string;
  sessionFile?: string;
  toolset?: string;
}

/** A fresh running run for `script`, without a lease yet. Throws when the script does not parse. */
export function newManagedRun(script: string, args: unknown, owner: NewRunOwner): ManagedRun {
  const { meta } = parseWorkflowScript(script);
  return {
    runId: newRunId(meta.name),
    status: "running",
    snapshot: createWorkflowSnapshot(meta),
    controller: new AbortController(),
    startedAt: new Date(),
    script,
    args,
    journal: [],
    sessionId: owner.sessionId,
    parentSessionId: owner.sessionId,
    parentSessionFile: owner.sessionFile,
    toolset: owner.toolset,
    agentTimestamps: new Map(),
    agentsById: new Map(),
    agentSessionsByCallId: new Map(),
    replayedAgentStatesByCallId: new Map(),
    agentTimestampsByCallId: new Map(),
    replayingAgentCalls: new Set(),
    executionAgentRows: new Set(),
    steerable: new Map(),
  };
}
