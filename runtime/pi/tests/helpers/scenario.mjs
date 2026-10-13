import { rmSync } from "node:fs";
import { startFakeOpenAI } from "./fake-openai.mjs";
import { initCommand, makeWorkspace, startBridge } from "./bridge-client.mjs";

/**
 * Run `fn` against a live bridge wired to a scripted fake relay, and clean up after it.
 *
 * @param {object} options
 * @param {Function} options.script reply script for the fake relay (see fake-openai.mjs)
 * @param {(workspace: object) => void} [options.prepare] lay out files before the bridge starts
 * @param {object | false} [options.init] overrides for the init command; `false` skips init
 * @param {object} [options.workspace] reuse a workspace (to restart a bridge on the same session)
 */
export async function withBridge({ script, prepare, init = {}, workspace }, fn) {
  const fake = await startFakeOpenAI(script);
  const ws = workspace ?? makeWorkspace();
  prepare?.(ws);
  const bridge = startBridge(ws);
  try {
    const ready = init === false ? undefined : await bridge.request(initCommand(ws, fake.baseUrl, init));
    await fn({ bridge, fake, ws, ready });
  } finally {
    bridge.kill();
    await bridge.exited;
    await fake.close();
    if (!workspace) rmSync(ws.root, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
  }
}

/** Send a prompt and return the response plus the records that arrived up to it. */
export async function prompt(bridge, id, message) {
  const from = bridge.records.length;
  const response = await bridge.request({ id, type: "prompt", message });
  return { response, records: bridge.records.slice(from) };
}

/** Plain text of an OpenAI message `content` (string or content parts). */
export function textOf(content) {
  if (typeof content === "string") return content;
  if (Array.isArray(content)) return content.map((part) => part.text ?? "").join("");
  return "";
}

/** `role: text` for every message of a recorded chat-completions request. */
export function transcript(request) {
  return request.body.messages.map((message) => `${message.role}: ${textOf(message.content)}`);
}

/** The text a request carries for one role, joined. */
export function textFor(request, role) {
  return request.body.messages
    .filter((message) => message.role === role)
    .map((message) => textOf(message.content))
    .join("\n");
}

/** Whether a recorded request is the main session's (only it has the subagents tool). */
export function isParentRequest(request) {
  return request.body.tools?.some((tool) => tool.function.name === "subagents") === true;
}
