import { PROVIDER_ID } from "../config.mjs";

/**
 * Seed a brand-new session with a converted conversation. The caller only calls this
 * for a session file that does not exist yet, so re-opening a session never imports twice.
 * The selection entries come first, as in a session that was started natively, and pi
 * writes the file as soon as the first user or assistant message is appended.
 *
 * @param {import("@earendil-works/pi-coding-agent").SessionManager} sessionManager
 * @param {object[]} messages validated by parseHistory
 */
export function importHistory(sessionManager, messages, { modelId, thinkingLevel }) {
  sessionManager.appendModelChange(PROVIDER_ID, modelId);
  sessionManager.appendThinkingLevelChange(thinkingLevel);
  for (const message of messages) sessionManager.appendMessage(message);
}
