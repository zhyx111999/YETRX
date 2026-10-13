export type AgentHistoryRole = "user" | "assistant" | "tool";

export type AgentHistoryKind = "text" | "toolCall" | "toolResult" | "error";

export interface AgentHistoryEntry {
  role: AgentHistoryRole;
  kind: AgentHistoryKind;
  text: string;
  toolName?: string;
  /** Pi's tool call id, shared by a call and its result so views pair them exactly, even for parallel calls. */
  toolCallId?: string;
  /** Target file of a read, write, or edit call; the pager labels and highlights from it. */
  path?: string;
  /** Pi's display-oriented edit diff, preserved from EditToolDetails. */
  diff?: string;
  isError?: boolean;
  timestamp?: number;
}

export interface AgentHistoryOptions {
  maxEntries?: number;
  maxTextChars?: number;
  maxTotalChars?: number;
}

const DEFAULT_MAX_ENTRIES = 40;
const DEFAULT_MAX_TEXT_CHARS = 2000;
const DEFAULT_MAX_TOTAL_CHARS = 20000;

const FILE_TOOLS = new Set(["read", "write", "edit"]);

export function compactAgentHistory(messages: unknown[], options: AgentHistoryOptions = {}): AgentHistoryEntry[] {
  const maxEntries = positiveInt(options.maxEntries, DEFAULT_MAX_ENTRIES);
  const maxTextChars = positiveInt(options.maxTextChars, DEFAULT_MAX_TEXT_CHARS);
  const maxTotalChars = positiveInt(options.maxTotalChars, DEFAULT_MAX_TOTAL_CHARS);
  const entries: AgentHistoryEntry[] = [];

  for (const raw of messages) {
    const message = asRecord(raw);
    if (!message) continue;
    const role = message.role;
    const timestamp = typeof message.timestamp === "number" ? message.timestamp : undefined;

    if (role === "user") {
      const text = textFromContent(message.content);
      if (text.trim()) entries.push({ role: "user", kind: "text", text, timestamp });
      continue;
    }

    if (role === "assistant") {
      for (const part of Array.isArray(message.content) ? message.content : []) {
        const block = asRecord(part);
        if (!block || typeof block.type !== "string") continue;
        if (block.type === "text" && typeof block.text === "string" && block.text.trim()) {
          entries.push({ role: "assistant", kind: "text", text: block.text, timestamp });
        } else if (block.type === "toolCall" && typeof block.name === "string") {
          entries.push(toolCallEntry(block, block.name, timestamp));
        }
      }
      if (typeof message.errorMessage === "string" && message.errorMessage.trim()) {
        entries.push({ role: "assistant", kind: "error", text: message.errorMessage, isError: true, timestamp });
      }
      continue;
    }

    if (role === "toolResult") {
      const toolName = typeof message.toolName === "string" ? message.toolName : undefined;
      const text = textFromContent(message.content) || "(no text output)";
      const details = asRecord(message.details);
      const diff = toolName === "edit" && typeof details?.diff === "string" ? details.diff : undefined;
      entries.push({
        role: "tool",
        kind: message.isError ? "error" : "toolResult",
        toolName,
        toolCallId: typeof message.toolCallId === "string" ? message.toolCallId : undefined,
        text,
        diff,
        isError: Boolean(message.isError),
        timestamp,
      });
    }
  }

  return fitEntries(entries, maxEntries, maxTextChars, maxTotalChars);
}

function toolCallEntry(block: Record<string, unknown>, name: string, timestamp: number | undefined): AgentHistoryEntry {
  const args = asRecord(block.arguments);
  const path = FILE_TOOLS.has(name) && typeof args?.path === "string" ? args.path : undefined;
  const writeContent = name === "write" && path && typeof args?.content === "string" ? args.content : undefined;
  return {
    role: "assistant",
    kind: "toolCall",
    toolName: name,
    toolCallId: typeof block.id === "string" ? block.id : undefined,
    // A write's JSON envelope is both noisy and likely to be truncated into
    // invalid JSON. Preserve its source directly so the pager can render it as
    // code; the path names its language.
    text: writeContent ?? stringifyCompact(block.arguments ?? {}),
    path,
    timestamp,
  };
}

function fitEntries(
  entries: AgentHistoryEntry[],
  maxEntries: number,
  maxTextChars: number,
  maxTotalChars: number,
): AgentHistoryEntry[] {
  const fitted: AgentHistoryEntry[] = [];
  let total = 0;

  for (const entry of entries.slice(-maxEntries).reverse()) {
    const remaining = maxTotalChars - total;
    if (remaining <= 0) break;

    // Treat an edit diff as the entry's primary display payload. Keeping it
    // within the same per-entry and total bounds prevents EditToolDetails from
    // bypassing history compaction with a large changed file.
    let entryBudget = Math.min(maxTextChars, remaining);
    const diff = entry.diff ? truncateText(entry.diff, entryBudget) : undefined;
    entryBudget -= diff?.length ?? 0;
    const text = truncateText(entry.text, entryBudget);
    fitted.unshift({ ...entry, text, diff });
    total += text.length + (diff?.length ?? 0);
  }

  return fitted;
}

function textFromContent(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content
    .map((part) => {
      const block = asRecord(part);
      return block?.type === "text" && typeof block.text === "string" ? block.text : "";
    })
    .filter(Boolean)
    .join("");
}

function stringifyCompact(value: unknown): string {
  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}

function truncateText(text: string, maxChars: number): string {
  if (text.length <= maxChars) return text;
  if (maxChars <= 20) return text.slice(0, maxChars);
  return `${text.slice(0, maxChars - 20)}... [truncated]`;
}

function positiveInt(value: number | undefined, fallback: number): number {
  return typeof value === "number" && Number.isInteger(value) && value > 0 ? value : fallback;
}

function asRecord(value: unknown): Record<string, unknown> | undefined {
  return value && typeof value === "object" ? (value as Record<string, unknown>) : undefined;
}
