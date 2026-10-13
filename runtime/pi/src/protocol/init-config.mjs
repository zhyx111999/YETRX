import { statSync } from "node:fs";
import { isAbsolute, resolve } from "node:path";
import { OPTIONAL_BUILTIN_TOOLS, THINKING_LEVELS } from "../config.mjs";
import { parseHistory } from "../runtime/history-messages.mjs";

/** The relay is private to this machine; never send its token anywhere else. */
function parseLoopbackUrl(value) {
  let url;
  try {
    url = new URL(value);
  } catch {
    throw new Error("baseUrl must be an absolute http(s) URL");
  }
  const host = url.hostname.replace(/^\[|\]$/g, "");
  const loopback = host === "localhost" || host === "::1" || /^127(\.\d{1,3}){3}$/.test(host);
  if (!/^https?:$/.test(url.protocol) || !loopback) throw new Error("baseUrl must be an http(s) loopback URL");
  return value;
}

/**
 * A leading `!` makes pi run the key as a shell command and `$` triggers env
 * interpolation, so only plain token characters are accepted.
 */
function parseToken(value) {
  if (typeof value !== "string" || !/^[A-Za-z0-9._~+/=-]{8,}$/.test(value)) {
    throw new Error("apiKey must be a token of at least 8 characters from [A-Za-z0-9._~+/=-]");
  }
  return value;
}

function parseDirectory(name, value, { mustExist }) {
  if (typeof value !== "string" || value === "") throw new Error(`${name} must be a non-empty path`);
  const path = resolve(value);
  if (mustExist && !statSync(path, { throwIfNoEntry: false })?.isDirectory()) {
    throw new Error(`${name} is not an existing directory: ${path}`);
  }
  return path;
}

function parsePositiveInteger(index, field, value) {
  if (!Number.isInteger(value) || value <= 0) throw new Error(`models[${index}].${field} must be a positive integer`);
  return value;
}

function parseModels(models) {
  if (!Array.isArray(models) || models.length === 0) throw new Error("models must be a non-empty array");
  const seen = new Set();
  return models.map((model, index) => {
    if (model === null || typeof model !== "object") throw new Error(`models[${index}] must be an object`);
    if (typeof model.id !== "string" || model.id === "") throw new Error(`models[${index}].id must be a non-empty string`);
    if (seen.has(model.id)) throw new Error(`models[${index}].id duplicates "${model.id}"`);
    seen.add(model.id);
    return {
      id: model.id,
      name: typeof model.name === "string" && model.name !== "" ? model.name : model.id,
      contextWindow: parsePositiveInteger(index, "contextWindow", model.contextWindow),
      maxTokens: parsePositiveInteger(index, "maxTokens", model.maxTokens),
    };
  });
}

/** The relay's model list, which must contain the model the session runs on. */
export function parseModelSelection(models, modelId) {
  const parsed = parseModels(models);
  if (!parsed.some((model) => model.id === modelId)) throw new Error(`modelId "${String(modelId)}" is not in models`);
  return parsed;
}

/** `value` when it is a known level, `fallback` when it is absent. */
export function parseThinkingLevel(value, fallback) {
  if (value === undefined) return fallback;
  if (!THINKING_LEVELS.includes(value)) throw new Error(`thinkingLevel must be one of ${THINKING_LEVELS.join(", ")}`);
  return value;
}

function parseTools(tools) {
  if (tools === undefined) return [];
  if (!Array.isArray(tools) || tools.some((name) => !OPTIONAL_BUILTIN_TOOLS.includes(name))) {
    throw new Error(`tools may only list ${OPTIONAL_BUILTIN_TOOLS.join(", ")}`);
  }
  return [...new Set(tools)];
}

/** An omitted prompt keeps the current value; a present prompt must be a string. */
export function parseSystemPrompt(command) {
  if (!Object.hasOwn(command, "systemPrompt")) return undefined;
  if (typeof command.systemPrompt !== "string") throw new Error("systemPrompt must be a string");
  return command.systemPrompt;
}

/**
 * Validate an `init` command and normalise it. Throws a plain Error naming the
 * first problem; the message never contains the token.
 */
export function parseInit(command) {
  const sessionFile = command.sessionFile;
  if (typeof sessionFile !== "string" || !isAbsolute(sessionFile)) throw new Error("sessionFile must be an absolute path");
  return {
    cwd: parseDirectory("cwd", command.cwd, { mustExist: true }),
    agentDir: parseDirectory("agentDir", command.agentDir, { mustExist: false }),
    sessionFile: resolve(sessionFile),
    baseUrl: parseLoopbackUrl(command.baseUrl),
    apiKey: parseToken(command.apiKey),
    models: parseModelSelection(command.models, command.modelId),
    modelId: command.modelId,
    thinkingLevel: parseThinkingLevel(command.thinkingLevel, "off"),
    history: command.history === undefined ? undefined : parseHistory(command.history),
    tools: parseTools(command.tools),
    systemPrompt: parseSystemPrompt(command),
    trustProject: command.trustProject === true,
  };
}
