import { PROVIDER_ID } from "../config.mjs";
import { log } from "../diagnostics.mjs";

/**
 * Record the main session's model as the agent directory's default. Workflow subagents
 * that name no model open their own session from these settings, so this is what makes
 * them inherit the main model (and the thinking level) by default.
 */
export async function persistModelDefaults(settingsManager, modelId, thinkingLevel) {
  settingsManager.setDefaultModelAndProvider(PROVIDER_ID, modelId);
  if (thinkingLevel !== undefined) settingsManager.setDefaultThinkingLevel(thinkingLevel);
  await settingsManager.flush();
  for (const { scope, error } of settingsManager.drainErrors()) log(`settings (${scope}): ${error.message}`);
}
