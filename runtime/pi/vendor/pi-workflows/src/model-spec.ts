import type { Api, Model } from "@earendil-works/pi-ai";
import { type ModelRuntime, type ResolveCliModelResult, resolveCliModel } from "@earendil-works/pi-coding-agent";
import { WorkflowError, WorkflowErrorCode } from "./errors.js";

export const THINKING_LEVELS = ["off", "minimal", "low", "medium", "high", "xhigh", "max"] as const;

export type ModelThinkingLevel = (typeof THINKING_LEVELS)[number];

/** Validate the separate script/SDK option without changing model-id parsing. */
export function validateThinkingLevel(value: unknown): asserts value is ModelThinkingLevel | undefined {
  if (value === undefined) return;
  if (typeof value !== "string" || !(THINKING_LEVELS as readonly string[]).includes(value)) {
    throw new WorkflowError(
      `agent thinking must be one of: ${THINKING_LEVELS.join(", ")}`,
      WorkflowErrorCode.SCRIPT_VALIDATION_ERROR,
      { recoverable: false },
    );
  }
}

export interface ResolvedModelSpec {
  requestedSpec: string;
  model?: Model<Api>;
  thinkingLevel?: ModelThinkingLevel;
  resolvedSpec?: string;
  warning?: string;
  error?: string;
}

export function isThinkingLevel(value: string): value is ModelThinkingLevel {
  return (THINKING_LEVELS as readonly string[]).includes(value);
}

export function formatModelSpecWithThinking(modelSpec: string, thinkingLevel: ModelThinkingLevel | undefined): string {
  return thinkingLevel ? `${modelSpec}:${thinkingLevel}` : modelSpec;
}

export function canonicalModelSpec(model: Model<Api>): string {
  return `${model.provider}/${model.id}`;
}

/**
 * Split a stored tier spec for display/editing. Exact known model specs win, so
 * model ids that legitimately contain colons are not mistaken for thinking.
 */
export function splitModelSpecThinking(
  spec: string | undefined,
  knownModelSpecs?: readonly string[],
): { modelSpec: string; thinkingLevel?: ModelThinkingLevel } {
  const trimmed = spec?.trim() ?? "";
  if (!trimmed) return { modelSpec: "", thinkingLevel: undefined };

  const known = knownModelSpecs ? new Set(knownModelSpecs) : undefined;
  if (known?.has(trimmed)) return { modelSpec: trimmed, thinkingLevel: undefined };

  const lastColon = trimmed.lastIndexOf(":");
  if (lastColon === -1) return { modelSpec: trimmed, thinkingLevel: undefined };

  const prefix = trimmed.slice(0, lastColon);
  const suffix = trimmed.slice(lastColon + 1);
  if (!prefix || !isThinkingLevel(suffix)) return { modelSpec: trimmed, thinkingLevel: undefined };
  if (known && !known.has(prefix)) return { modelSpec: trimmed, thinkingLevel: undefined };
  return { modelSpec: prefix, thinkingLevel: suffix };
}

/**
 * Resolve a workflow model spec (an agent `model`, a tier entry, a phase route)
 * with Pi's own `--model` resolver, so a spec selects the same model in a
 * workflow as on the command line: `provider/modelId[:thinking]`, bare ids,
 * fuzzy patterns, and model ids that contain colons.
 *
 * Two workflow rules sit on top, and only for specs without a provider prefix:
 * - The id is resolved without its thinking suffix. The SDK settles an id that
 *   several providers share (the only authenticated one, else an ambiguity
 *   error) only when the id is the whole input, and tier specs usually carry a
 *   suffix.
 * - A choice the SDK cannot settle by id goes to the main session's provider
 *   when that provider catalogs a match (see preferProvider).
 */
export function resolveModelSpecWithThinking(
  spec: string,
  modelRuntime: ModelRuntime,
  options: { preferredProvider?: string } = {},
): ResolvedModelSpec {
  const requestedSpec = spec.trim();
  if (!requestedSpec) return { requestedSpec, error: "No model spec provided." };

  const resolved = resolveCliModel({ cliModel: requestedSpec, modelRuntime });
  const chosen = namesProvider(requestedSpec, modelRuntime)
    ? resolved
    : resolveBareSpec(requestedSpec, resolved, modelRuntime, options.preferredProvider);
  if (!chosen.model) return { requestedSpec, warning: chosen.warning, error: chosen.error };
  return {
    requestedSpec,
    model: chosen.model,
    thinkingLevel: chosen.thinkingLevel,
    warning: chosen.warning,
    resolvedSpec: formatModelSpecWithThinking(canonicalModelSpec(chosen.model), chosen.thinkingLevel),
  };
}

/** Whether the SDK reads the spec as `provider/model`: the text before the first slash names a catalog provider. */
function namesProvider(spec: string, modelRuntime: ModelRuntime): boolean {
  const slash = spec.indexOf("/");
  if (slash === -1) return false;
  const prefix = spec.slice(0, slash).toLowerCase();
  return modelRuntime.getModels().some((model) => model.provider.toLowerCase() === prefix);
}

/** Resolve a spec without a provider prefix; `resolved` is the SDK's answer for the whole spec. */
function resolveBareSpec(
  spec: string,
  resolved: ResolveCliModelResult,
  modelRuntime: ModelRuntime,
  preferredProvider: string | undefined,
): ResolveCliModelResult {
  // The SDK reports a thinking level only after splitting it off the last colon.
  const thinkingLevel = resolved.thinkingLevel;
  const reference = thinkingLevel ? spec.slice(0, spec.lastIndexOf(":")) : spec;
  const byReference = thinkingLevel ? resolveCliModel({ cliModel: reference, modelRuntime }) : resolved;
  const chosen = preferredProvider
    ? preferProvider(reference, byReference, modelRuntime, preferredProvider)
    : byReference;
  return chosen.model ? { ...chosen, thinkingLevel: thinkingLevel ?? chosen.thinkingLevel } : chosen;
}

/**
 * The SDK settles a reference that is a model id on its own: it returns that
 * model (the only one, or the only authenticated one) or reports the id as
 * ambiguous. Everything else is a fuzzy ranking across every provider. The
 * main session's provider is the better tiebreak for both the ranking and the
 * ambiguity, since the user already runs on it; an ambiguous id still only
 * resolves to that same id.
 */
function preferProvider(
  reference: string,
  resolved: ResolveCliModelResult,
  modelRuntime: ModelRuntime,
  provider: string,
): ResolveCliModelResult {
  const isReferenceId = (model: Model<Api> | undefined) => model?.id.toLowerCase() === reference.toLowerCase();
  if (isReferenceId(resolved.model)) return resolved;

  const preferred = resolveCliModel({ cliProvider: provider, cliModel: reference, modelRuntime });
  // Given a provider, the SDK invents a custom model id when nothing matches; that is no catalog match.
  const cataloged = preferred.model && modelRuntime.getModel(preferred.model.provider, preferred.model.id);
  return cataloged && (resolved.model || isReferenceId(preferred.model)) ? preferred : resolved;
}
