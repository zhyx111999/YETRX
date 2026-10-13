/**
 * Validation of `init.history`: pi-compatible messages handed over by the Go host
 * when a legacy conversation is converted. Everything is checked before anything is
 * written, so a malformed history never leaves a half-imported session behind.
 */

const ROLES = new Set(["user", "assistant", "toolResult"]);

function fail(index, reason) {
  throw new Error(`history[${index}]: ${reason}`);
}

function requireString(index, message, field) {
  if (typeof message[field] !== "string" || message[field] === "") fail(index, `${field} must be a non-empty string`);
}

function requireContent(index, message) {
  const { content } = message;
  if (message.role === "user" && typeof content === "string") return;
  if (!Array.isArray(content)) fail(index, `${message.role} content must be ${message.role === "user" ? "a string or " : ""}an array of content blocks`);
  for (const block of content) {
    if (block === null || typeof block !== "object" || typeof block.type !== "string") {
      fail(index, "content blocks must be objects with a type");
    }
  }
}

function validateMessage(message, index) {
  if (message === null || typeof message !== "object" || Array.isArray(message)) fail(index, "must be an object");
  if (!ROLES.has(message.role)) fail(index, `role must be one of ${[...ROLES].join(", ")}`);
  requireContent(index, message);
  if (message.role === "assistant") {
    for (const field of ["api", "provider", "model", "stopReason"]) requireString(index, message, field);
    if (message.usage === null || typeof message.usage !== "object") fail(index, "assistant usage must be an object");
  }
  if (message.role === "toolResult") {
    requireString(index, message, "toolCallId");
    requireString(index, message, "toolName");
    if (typeof message.isError !== "boolean") fail(index, "toolResult isError must be a boolean");
  }
}

/**
 * Validate `history` and return copies that all carry a numeric timestamp.
 * Missing timestamps are filled with strictly increasing values so order survives.
 */
export function parseHistory(history) {
  if (!Array.isArray(history)) throw new Error("history must be an array of messages");
  history.forEach(validateMessage);
  const base = Date.now() - history.length;
  return history.map((message, index) => ({
    ...message,
    timestamp: Number.isFinite(message.timestamp) ? message.timestamp : base + index,
  }));
}
