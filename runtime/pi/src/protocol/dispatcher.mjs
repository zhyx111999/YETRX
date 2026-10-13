import { errorText, log, registerSecret } from "../diagnostics.mjs";
import { parseInit, parseModelSelection, parseThinkingLevel } from "./init-config.mjs";

const STREAMING_BEHAVIORS = ["steer", "followUp"];

function parsePrompt(command) {
  if (typeof command.message !== "string" || command.message.trim() === "") {
    throw new Error("message must be a non-empty string");
  }
  const streamingBehavior = command.streamingBehavior ?? "followUp";
  if (!STREAMING_BEHAVIORS.includes(streamingBehavior)) {
    throw new Error(`streamingBehavior must be one of ${STREAMING_BEHAVIORS.join(", ")}`);
  }
  return { message: command.message, streamingBehavior };
}

function parseSetModel(command) {
  if (typeof command.modelId !== "string" || command.modelId === "") throw new Error("modelId must be a non-empty string");
  return { modelId: command.modelId, thinkingLevel: parseThinkingLevel(command.thinkingLevel, undefined) };
}

function parseSetModels(command) {
  return {
    models: parseModelSelection(command.models, command.modelId),
    modelId: command.modelId,
    thinkingLevel: parseThinkingLevel(command.thinkingLevel, undefined),
  };
}

/**
 * Turns protocol lines into host calls and host results into protocol records.
 * Every command is answered exactly once, with its own `id`: `ready` for a successful
 * `init`, `response` (`ok` true or false) for everything else, including every failure.
 *
 * @param {object} deps
 * @param {import("../runtime/session-host.mjs").SessionHost} deps.host
 * @param {(record: object) => void} deps.send
 * @param {() => Promise<void>} deps.finish flushes output and exits the process
 */
export function createDispatcher({ host, send, finish }) {
  const respond = (id, error) =>
    send(error === undefined ? { type: "response", id, ok: true } : { type: "response", id, ok: false, error });

  const handlers = {
    async init(command) {
      const config = parseInit(command);
      registerSecret(config.apiKey);
      const ready = await host.init(config);
      send({ type: "ready", id: command.id, ...ready });
    },
    async prompt(command) {
      await host.prompt(parsePrompt(command));
      respond(command.id);
    },
    async abort(command) {
      await host.abort();
      respond(command.id);
    },
    async set_model(command) {
      await host.setModel(parseSetModel(command));
      respond(command.id);
    },
    async set_models(command) {
      await host.setModels(parseSetModels(command));
      respond(command.id);
    },
    async shutdown(command) {
      // Whatever goes wrong while closing, the host asked to stop: answer, then exit.
      await host.shutdown().catch((error) => log(`shutdown failed: ${errorText(error)}`));
      respond(command.id);
      await finish();
    },
  };

  /** @param {string} line one protocol record, without its LF */
  return async function dispatch(line) {
    let command;
    try {
      command = JSON.parse(line);
    } catch (error) {
      return send({ type: "response", ok: false, error: `invalid JSON: ${errorText(error)}` });
    }
    if (command === null || typeof command !== "object" || Array.isArray(command)) {
      return send({ type: "response", ok: false, error: "command must be a JSON object" });
    }
    const handler = Object.hasOwn(handlers, command.type) ? handlers[command.type] : undefined;
    if (!handler) return respond(command.id, `unknown command type: ${String(command.type)}`);
    try {
      await handler(command);
    } catch (error) {
      respond(command.id, errorText(error));
    }
  };
}
