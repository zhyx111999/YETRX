/** Implements checkpoint(): a journaled pause that suspends the run until the host persists a response for its ID. */

import { isDeepStrictEqual } from "node:util";
import { WorkflowCheckpointSuspensionError, WorkflowError, WorkflowErrorCode } from "../errors.js";
import type { WorkflowCheckpoint, WorkflowCheckpointInput } from "../workflow.js";
import { hashCheckpointCall } from "./call-identity.js";
import { cloneDurableJsonValue } from "./durable-json.js";
import { currentCallSequence } from "./fanout.js";
import type { RunContext } from "./run-context.js";

const CHECKPOINT_IDENTIFIER = /^[A-Za-z0-9._:-]{1,200}$/u;

const invalidCheckpoint = (message: string) =>
  new WorkflowError(message, WorkflowErrorCode.SCRIPT_VALIDATION_ERROR, { recoverable: false });

/** Build the script's checkpoint() global for this run. */
export function createCheckpoint(ctx: RunContext) {
  const { state, options } = ctx;
  return async (input: WorkflowCheckpointInput) => {
    ctx.throwIfAborted();
    if (!isWorkflowCheckpointInput(input)) {
      throw new TypeError("checkpoint() needs exactly { kind, checkpointId, payload } with valid string identifiers");
    }
    ctx.ensureAgentCapacity();

    const callIndex = currentCallSequence(ctx).takeIndex();
    const durableInput: WorkflowCheckpointInput = {
      ...input,
      payload: cloneDurableJsonValue(input.payload, "checkpoint payload"),
    };
    const { checkpointId } = durableInput;
    if (state.seenCheckpointIds.has(checkpointId)) {
      throw invalidCheckpoint(`durable checkpoint ID ${JSON.stringify(checkpointId)} must be unique within the run`);
    }
    state.seenCheckpointIds.add(checkpointId);

    // Replay a journaled answer like agent() does, except for the checkpoint
    // whose persisted response this execution is consuming right now.
    const activeResponse = state.activeCheckpointResponse;
    const callHash = hashCheckpointCall(durableInput);
    // The checkpoint whose persisted response this execution consumes takes it from the response.
    const cached = ctx.replayable(callIndex, callHash, () => activeResponse?.checkpointId === checkpointId);
    state.agentCount++;
    if (cached) return cached.result;

    if (activeResponse) {
      if (state.activeCheckpointId !== checkpointId) {
        throw invalidCheckpoint(
          `durable checkpoint ${JSON.stringify(checkpointId)} does not own the active reservation`,
        );
      }
      return consumeResponse(ctx, callIndex, callHash, durableInput, activeResponse);
    }
    if (state.activeCheckpointId !== null) {
      throw invalidCheckpoint(`durable checkpoint ${JSON.stringify(state.activeCheckpointId)} is already active`);
    }
    state.activeCheckpointId = checkpointId;

    const waiting: WorkflowCheckpoint = {
      version: 1,
      ...durableInput,
      status: "waiting",
      createdAt: new Date().toISOString(),
    };
    options.onWorkflowCheckpoint?.(waiting);
    state.checkpointSuspension = new WorkflowCheckpointSuspensionError(waiting.checkpointId);
    throw state.checkpointSuspension;
  };
}

/** Return the persisted response once, after checking it answers exactly this checkpoint. */
function consumeResponse(
  ctx: RunContext,
  callIndex: number,
  callHash: string,
  input: WorkflowCheckpointInput,
  active: WorkflowCheckpoint,
): unknown {
  const { state, options } = ctx;
  if (
    active.version !== 1 ||
    active.status !== "resuming" ||
    active.checkpointId !== input.checkpointId ||
    active.kind !== input.kind ||
    !isDeepStrictEqual(active.payload, input.payload) ||
    !Object.hasOwn(active, "response")
  ) {
    throw invalidCheckpoint(
      `durable checkpoint ${JSON.stringify(input.checkpointId)} does not match the persisted response`,
    );
  }
  options.onAgentJournal?.({ index: callIndex, hash: callHash, result: active.response });
  options.onWorkflowCheckpoint?.({ ...active, status: "consumed", consumedAt: new Date().toISOString() });
  state.activeCheckpointResponse = null;
  state.activeCheckpointId = null;
  return active.response;
}

function isWorkflowCheckpointInput(value: unknown): value is WorkflowCheckpointInput {
  if (typeof value !== "object" || value === null || Array.isArray(value)) return false;
  const input = value as Record<string, unknown>;
  return (
    Object.keys(input).length === 3 &&
    Object.hasOwn(input, "payload") &&
    typeof input.checkpointId === "string" &&
    CHECKPOINT_IDENTIFIER.test(input.checkpointId) &&
    typeof input.kind === "string" &&
    CHECKPOINT_IDENTIFIER.test(input.kind)
  );
}
