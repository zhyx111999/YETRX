// Detects provider usage/quota limits, which the pi SDK records on the last assistant message instead of throwing.
import type { AssistantMessage } from "@earendil-works/pi-ai";
import { classifyProviderLimit, WorkflowError, WorkflowErrorCode } from "../errors.js";

/**
 * The last assistant message's terminal metadata. The pi SDK does not throw
 * provider usage/quota limits: it records them as an assistant message with
 * stopReason "error" and an errorMessage, so this is the only place they show.
 */
export function lastAssistantError(messages: unknown[]): { stopReason?: string; errorMessage?: string } | undefined {
  for (let i = messages.length - 1; i >= 0; i--) {
    const message = messages[i] as Partial<AssistantMessage> | undefined;
    if (message?.role !== "assistant") continue;
    return { stopReason: message.stopReason, errorMessage: message.errorMessage };
  }
  return undefined;
}

/**
 * Throw PROVIDER_USAGE_LIMIT with the provider's message and reset hint when the
 * turn ended in a usage/quota/rate-limit error. Gated on stopReason "error" so a
 * successful answer that merely mentions "rate limit" is never misclassified.
 * Not recoverable: the run pauses for a later resume instead of retrying into the
 * same limit or collapsing to a silent null.
 */
export function throwIfProviderLimit(messages: unknown[], label?: string): void {
  const err = lastAssistantError(messages);
  if (err?.stopReason !== "error") return;
  const { matched, resetHint } = classifyProviderLimit(err.errorMessage);
  if (!matched) return;
  throw new WorkflowError(
    err.errorMessage ?? "Provider usage/quota limit reached",
    WorkflowErrorCode.PROVIDER_USAGE_LIMIT,
    { recoverable: false, agentLabel: label, resetHint },
  );
}
