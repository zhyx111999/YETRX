/** Lists the models that have auth configured, in the SDK-agnostic shape tier ranking and the model picker use. */

import type { ModelRegistry } from "@earendil-works/pi-coding-agent";
import { canonicalModelSpec } from "../model-spec.js";
import type { RankableModel } from "../model-tier-config.js";
import { builtFallbackRegistry, ensureFallbackRegistry } from "./model-registry.js";

/**
 * List the user's currently available models (those with auth configured) with
 * the minimal fields tier ranking needs: canonical spec, output price, and
 * context window. This is the single place the SDK `Model` is projected into
 * the SDK-agnostic `RankableModel`. Best-effort: returns [] if the registry
 * can't be built (or while the disk-backed fallback is still initializing).
 */
export function listAvailableModels(registry?: ModelRegistry): RankableModel[] {
  try {
    const modelRegistry = registry ?? builtFallbackRegistry();
    if (!modelRegistry) {
      // Start the async fallback build; this call reports [] and later calls
      // (e.g. the tool's lazy promptGuidelines re-reads) see real specs.
      void ensureFallbackRegistry().catch(() => {});
      return [];
    }
    return modelRegistry.getAvailable().map((model) => ({
      spec: canonicalModelSpec(model),
      costOutput: model.cost?.output,
      contextWindow: model.contextWindow,
    }));
  } catch {
    return [];
  }
}

/** The available models as `provider/modelId` specs, for telling a workflow author where agents may route. */
export function listAvailableModelSpecs(registry?: ModelRegistry): string[] {
  return listAvailableModels(registry).map((model) => model.spec);
}
