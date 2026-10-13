import { InMemoryCredentialStore, InMemoryModelsStore } from "@earendil-works/pi-ai";
import { ModelRuntime } from "@earendil-works/pi-coding-agent";
import { PROVIDER_API, PROVIDER_ID } from "../config.mjs";
import { log } from "../diagnostics.mjs";

/**
 * What the Go relay is verified to do: it is an OpenAI chat-completions endpoint
 * that streams tool calls and usage, reads `reasoning_effort`, and answers
 * `reasoning_content`. Everything else pi would auto-detect from the URL stays off,
 * because a loopback URL gives it nothing to detect.
 */
const RELAY_COMPAT = {
  supportsStore: false,
  supportsDeveloperRole: false,
  supportsReasoningEffort: true,
  supportsUsageInStreaming: true,
  maxTokensField: "max_tokens",
  thinkingFormat: "openai",
  supportsStrictMode: false,
  supportsLongCacheRetention: false,
  sendSessionAffinityHeaders: false,
};

const FREE = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 };

function toProviderModel(model) {
  return {
    id: model.id,
    name: model.name,
    reasoning: true,
    input: ["text"],
    cost: FREE,
    contextWindow: model.contextWindow,
    maxTokens: model.maxTokens,
    compat: RELAY_COMPAT,
  };
}

/**
 * (Re)declare the relay provider with exactly `models`. Registering a provider name
 * again replaces its model list in place and takes effect immediately, so this is both
 * how the provider first appears and how it is updated while a session is running.
 * The credential lives in the runtime, not in this declaration, and survives the update.
 */
export function registerRelayProvider(runtime, { baseUrl, models }) {
  runtime.registerProvider(PROVIDER_ID, {
    name: "YETRX Relay",
    baseUrl,
    api: PROVIDER_API,
    models: models.map(toProviderModel),
  });
}

/** Providers other than the relay that already hold credentials in this process. */
async function foreignProviders(runtime) {
  const available = await runtime.getAvailable();
  return [...new Set(available.map((model) => model.provider).filter((id) => id !== PROVIDER_ID))];
}

/**
 * A ModelRuntime that lives entirely in memory: no auth.json, no models.json, no
 * catalog cache, no network refresh. The relay token is held only as a runtime
 * credential, so it never reaches a file, a session entry or a log line.
 *
 * Workflow subagents reach this same runtime through `ctx.modelRegistry`, which is
 * how they inherit the main session's provider and token.
 */
export async function createRelayModelRuntime({ baseUrl, apiKey, models }) {
  const runtime = await ModelRuntime.create({
    credentials: new InMemoryCredentialStore(),
    modelsPath: null,
    modelsStore: new InMemoryModelsStore(),
    allowModelNetwork: false,
    refreshOnCreate: false,
  });
  registerRelayProvider(runtime, { baseUrl, models });
  await runtime.setRuntimeApiKey(PROVIDER_ID, apiKey);

  const foreign = await foreignProviders(runtime);
  if (foreign.length > 0) {
    log(`warning: providers with ambient credentials can bypass the relay: ${foreign.join(", ")}`);
  }
  return runtime;
}
