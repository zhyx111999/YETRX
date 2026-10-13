import { WorkflowError, WorkflowErrorCode } from "../errors.js";
import { canonicalModelSpec, formatModelSpecWithThinking, resolveModelSpecWithThinking, } from "../model-spec.js";
import { formatTierFallbackNotice, resolveTierModel, sortedTierNames, } from "../model-tier-config.js";
import { runtimeOf } from "./model-registry.js";
/**
 * Route a subagent call to a model spec. Precedence, most specific first: an
 * explicit model, then a tier, then (for an untagged agent) the inherited main
 * model with inheritMainModel on, then the configured "medium" tier, so a
 * configured tier set affects the whole workflow and not only tagged agents.
 *
 * A tier that model-tiers.json does not define throws MODEL_NOT_FOUND: a
 * misspelled tier would otherwise run silently on some other model.
 */
export function routeAgentModel(request, mainModel, config, inheritMainModel = false) {
    if (request.model)
        return { kind: "model", spec: request.model };
    if (request.tier) {
        if (!config)
            return { kind: "tier-without-config", tier: request.tier, spec: mainModel };
        const spec = resolveTierModel(request.tier, config);
        if (!spec) {
            throw new WorkflowError(`tier "${request.tier}" is not defined in model-tiers.json (defined: ${sortedTierNames(config).join(", ")}). ` +
                "Use /workflows-models to configure it.", WorkflowErrorCode.MODEL_NOT_FOUND, { recoverable: false, agentLabel: request.label });
        }
        return { kind: "tier", tier: request.tier, spec };
    }
    if (inheritMainModel && mainModel)
        return { kind: "inherit-main", spec: mainModel };
    const medium = config ? resolveTierModel("medium", config) : undefined;
    return medium ? { kind: "medium-tier", spec: medium } : { kind: "session-default" };
}
/**
 * Model selection for one WorkflowAgent. A spec that does not resolve is handled
 * by who asked for it:
 * - A route the script asked for (a model, an agentType or phase model, or a
 *   tier) throws MODEL_NOT_FOUND. Resolution is deterministic, so retrying is
 *   pointless, and silently substituting another model would run real calls
 *   against weights the caller never chose.
 * - An untagged agent only reached the spec through an implicit route (the
 *   default medium tier or the inherited main model). It degrades to the
 *   session default rather than failing every untagged agent.
 * Notices about routing go to onNotice, each degrade once per instance, so
 * they land in the run's own log.
 */
export class AgentModelSelector {
    routing;
    noticesSent = new Set();
    constructor(routing) {
        this.routing = routing;
    }
    select(request, registry) {
        const { mainModel, inheritMainModel } = this.routing;
        const route = routeAgentModel(request, mainModel, this.routing.loadTierConfig(), inheritMainModel);
        if (route.kind === "tier-without-config") {
            this.noticeOnce(request, "tier-without-config", () => formatTierFallbackNotice(mainModel, this.routing.listModels(registry)));
        }
        const spec = route.kind === "session-default" ? undefined : route.spec;
        if (!spec)
            return { thinkingLevel: request.thinking };
        // Pi CLI-style parsing, including an optional :thinking suffix such as gpt-5.5:xhigh.
        const resolved = resolveModelSpecWithThinking(spec, runtimeOf(registry), {
            preferredProvider: mainModel?.split("/", 1)[0],
        });
        if (resolved.warning)
            request.onNotice?.(resolved.warning);
        if (!resolved.model) {
            if (route.kind === "inherit-main" || route.kind === "medium-tier") {
                this.noticeOnce(request, "implicit-degrade", () => route.kind === "inherit-main"
                    ? `inherited main model "${spec}" unavailable — using the session default`
                    : `default "medium" tier model "${spec}" unavailable — using the session default`);
                return { thinkingLevel: request.thinking };
            }
            throw new WorkflowError(unavailableModelMessage(route, spec, resolved.error), WorkflowErrorCode.MODEL_NOT_FOUND, {
                recoverable: false,
                agentLabel: request.label,
            });
        }
        const resolvedSpec = resolved.resolvedSpec ?? canonicalModelSpec(resolved.model);
        request.onModelResolved?.(resolved.thinkingLevel !== undefined || request.thinking === undefined
            ? resolvedSpec
            : formatModelSpecWithThinking(resolvedSpec, request.thinking));
        return { model: resolved.model, thinkingLevel: resolved.thinkingLevel ?? request.thinking };
    }
    noticeOnce(request, kind, message) {
        if (this.noticesSent.has(kind) || !request.onNotice)
            return;
        this.noticesSent.add(kind);
        request.onNotice(message());
    }
}
/** Why a model the script asked for cannot run, naming where the spec came from. */
function unavailableModelMessage(route, spec, resolverError) {
    const origin = route.kind === "tier"
        ? `tier "${route.tier}" from model-tiers.json ("${spec}")`
        : route.kind === "tier-without-config"
            ? `tier "${route.tier}" (no model-tiers.json, so the session's main model "${spec}")`
            : `Model "${spec}"`;
    const reason = (resolverError ?? "it is not available").replace(/.?$/, ".");
    return `${origin} cannot be used: ${reason} Use /workflows-models to choose an available model.`;
}
