import { PROVIDER_ID } from "../config.mjs";
import { persistModelDefaults } from "./model-defaults.mjs";
import { registerRelayProvider } from "./relay-model-runtime.mjs";

/** Whether two registered models describe the same thing to pi. */
function sameDefinition(a, b) {
  return a.id === b.id && a.name === b.name && a.contextWindow === b.contextWindow && a.maxTokens === b.maxTokens;
}

/**
 * Replace the relay's model list while the session keeps running. Only the provider's
 * declaration changes: the session, its history and every background workflow are
 * untouched, and subagents opened afterwards see the new list through the shared
 * runtime. The session is moved to `modelId` only when that is a different model, or
 * the same id with a changed definition; otherwise it keeps running as it was.
 * Subagents already running keep the model objects they started with.
 *
 * @param {{ session: any, modelRuntime: any, settingsManager: any, baseUrl: string }} active
 * @param {{ models: object[], modelId: string, thinkingLevel?: string }} update models already validated
 */
export async function updateModels(active, { models, modelId, thinkingLevel }) {
  const { session, modelRuntime, settingsManager, baseUrl } = active;
  registerRelayProvider(modelRuntime, { baseUrl, models });

  const next = modelRuntime.getModel(PROVIDER_ID, modelId);
  if (!next) throw new Error(`model "${modelId}" is not available after the update`);
  const current = session.model;
  if (!current || current.provider !== PROVIDER_ID || !sameDefinition(current, next)) await session.setModel(next);
  if (thinkingLevel !== undefined) session.setThinkingLevel(thinkingLevel);
  await persistModelDefaults(settingsManager, modelId, thinkingLevel);
}
