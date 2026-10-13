// Converts SDK session stats and the streaming assistant message into AgentUsage figures.
import type { AgentSessionEvent } from "@earendil-works/pi-coding-agent";
import { type AgentUsage, createEmptyAgentUsage, sumAgentUsage } from "../agent-usage.js";

/** The token/cost part of the SDK's session stats. */
export interface SessionUsageStats {
  tokens: { input: number; output: number; cacheRead: number; cacheWrite: number; total: number };
  cost: number;
}

/**
 * Map session stats to an AgentUsage, or undefined when the provider reported
 * no usage at all (all-zero stats). Returning undefined instead of a zero
 * breakdown lets displays fall back to their scalar token count, so
 * non-reporting providers render as plain token totals.
 */
export function usageFromStats(stats: SessionUsageStats): AgentUsage | undefined {
  const { tokens, cost } = stats;
  if (tokens.total <= 0 && cost <= 0) return undefined;
  return {
    input: tokens.input,
    output: tokens.output,
    cacheRead: tokens.cacheRead,
    cacheWrite: tokens.cacheWrite,
    total: tokens.total,
    cost,
  };
}

/** `stats` minus `baseline` (never below zero); `stats` itself when there is no baseline. */
export function subtractSessionUsageStats(stats: SessionUsageStats, baseline?: SessionUsageStats): SessionUsageStats {
  if (!baseline) {
    return stats;
  }
  return {
    tokens: {
      input: Math.max(0, stats.tokens.input - baseline.tokens.input),
      output: Math.max(0, stats.tokens.output - baseline.tokens.output),
      cacheRead: Math.max(0, stats.tokens.cacheRead - baseline.tokens.cacheRead),
      cacheWrite: Math.max(0, stats.tokens.cacheWrite - baseline.tokens.cacheWrite),
      total: Math.max(0, stats.tokens.total - baseline.tokens.total),
    },
    cost: Math.max(0, stats.cost - baseline.cost),
  };
}

/**
 * Combine usage from completed messages with the current streaming message.
 * AgentSession notifies subscribers before it appends a message_end event to
 * SessionManager, so the event's assistant usage is absent from stats and must
 * be added exactly once. The real-session regression test pins this ordering.
 */
export function usageFromSessionProgress(stats: SessionUsageStats, event: AgentSessionEvent): AgentUsage | undefined {
  const persisted = usageFromStats(stats);
  const streaming = estimateStreamingAssistantUsage(event);
  if (!streaming) {
    return persisted;
  }
  return sumAgentUsage(persisted ?? createEmptyAgentUsage(), streaming);
}

/**
 * Usage of the assistant message an event carries: the provider's figures when
 * it reported any, else an output estimate of one token per four streamed
 * characters, tagged `estimated` so it is never shown as a measurement.
 */
function estimateStreamingAssistantUsage(event: AgentSessionEvent): AgentUsage | undefined {
  if (event.type !== "message_update" && event.type !== "message_end") {
    return undefined;
  }
  if (event.message.role !== "assistant") {
    return undefined;
  }

  const reported = event.message.usage;
  const reportedTotal = reported.input + reported.output + reported.cacheRead + reported.cacheWrite;
  if (reportedTotal > 0 || reported.cost.total > 0) {
    return {
      input: reported.input,
      output: reported.output,
      cacheRead: reported.cacheRead,
      cacheWrite: reported.cacheWrite,
      total: reportedTotal,
      cost: reported.cost.total,
    };
  }

  let streamedCharacters = 0;
  for (const content of event.message.content) {
    if (content.type === "text") {
      streamedCharacters += content.text.length;
    } else if (content.type === "thinking") {
      streamedCharacters += content.thinking.length;
    } else if (content.type === "toolCall") {
      streamedCharacters += JSON.stringify(content.arguments).length;
    }
  }
  if (streamedCharacters === 0) {
    return undefined;
  }

  const estimatedOutput = Math.max(1, Math.ceil(streamedCharacters / 4));
  return {
    input: 0,
    output: estimatedOutput,
    cacheRead: 0,
    cacheWrite: 0,
    total: estimatedOutput,
    cost: 0,
    estimated: true,
  };
}
