/**
 * `/workflows-models` command handler.
 *
 * Uses Pi's built-in `ctx.ui.select()`, `ctx.ui.confirm()`, and `ctx.ui.notify()`
 * to let users view and manage model tier configuration for workflows. Those
 * dialogs work in the terminal UI and in RPC hosts; only the model picker adds a
 * scrollable terminal list, with a `select()` fallback everywhere else.
 *
 * Model selection draws from the host session's shared model registry so users
 * see every provider Pi can reach, including extension-registered providers such
 * as `ollama-cloud`.
 *
 * Each tier holds exactly one model spec string. The string may include Pi
 * CLI-style thinking suffixes, e.g. `openai-codex/gpt-5.5:xhigh`.
 * When editing a tier, users pick a model, then an optional thinking level.
 */
import { existsSync } from "node:fs";
import { Container, SelectList, Spacer, Text, } from "@earendil-works/pi-tui";
import { listAvailableModelSpecs, listAvailableModels } from "./agent/available-models.js";
import { formatModelSpecWithThinking, splitModelSpecThinking, THINKING_LEVELS, } from "./model-spec.js";
import { buildDefaultTierConfig, getModelTierConfigPath, getProjectModelTierConfigPath, loadModelTierConfig, saveModelTierConfig, sortedTierNames, } from "./model-tier-config.js";
/**
 * Register the `/workflows-models` command with Pi.
 */
export function registerWorkflowModelsCommand(pi) {
    pi.registerCommand("workflows-models", {
        description: "View and edit model tiers used by workflows (small/medium/big)",
        handler: async (_args, ctx) => {
            await ctx.waitForIdle();
            // Load the saved config, or build an in-memory default spread across the
            // available models. If the model registry is empty, fall back to the
            // current Pi model so the tiers are still usable. Pass the host session's
            // registry explicitly: since pi 0.80.8 the no-registry fallback inside
            // listAvailableModels() initializes asynchronously and reports [] on the
            // first call, which would rank defaults from an empty model list.
            const cwd = ctx.cwd || process.cwd();
            const globalPath = getModelTierConfigPath();
            const projectPath = getProjectModelTierConfigPath(cwd);
            let scope = existsSync(projectPath) ? "project" : "global";
            const currentModel = ctx.model ? `${ctx.model.provider}/${ctx.model.id}` : undefined;
            const defaults = () => buildDefaultTierConfig(currentModel, listAvailableModels(ctx.modelRegistry));
            const loadForScope = (next) => {
                if (next === "project") {
                    return loadModelTierConfig({ cwd }) ?? defaults();
                }
                return loadModelTierConfig(globalPath) ?? defaults();
            };
            let config = loadForScope(scope);
            let dirty = false;
            const menu = () => [
                ...sortedTierNames(config).map((name) => ({
                    label: `${name} tier → ${config.tiers[name]}`,
                    choose: async () => {
                        const updated = await editSingleTier(ctx, config.tiers, name);
                        if (updated) {
                            config = { ...config, tiers: updated };
                            dirty = true;
                        }
                        return true;
                    },
                })),
                {
                    label: scope === "project" ? "Switch to global" : "Switch to project",
                    choose: async () => {
                        if (dirty && !(await ctx.ui.confirm("Switch scope", "Unsaved changes will be discarded. Continue?"))) {
                            return true;
                        }
                        scope = scope === "project" ? "global" : "project";
                        config = loadForScope(scope);
                        dirty = false;
                        return true;
                    },
                },
                {
                    label: "Reset to defaults",
                    choose: async () => {
                        if (await ctx.ui.confirm("Reset model tiers", "This will reset tiers from your available model list. Continue?")) {
                            config = defaults();
                            dirty = true;
                            ctx.ui.notify("Tiers reset to defaults. Use 'Save and exit' to persist.", "info");
                        }
                        return true;
                    },
                },
                {
                    label: dirty ? "Save and exit" : "Exit",
                    choose: async () => {
                        if (dirty) {
                            saveModelTierConfig(config, scope === "project" ? projectPath : globalPath);
                            ctx.ui.notify(scope === "project" ? "Project model tiers saved." : "Model tiers saved.", "info");
                        }
                        return false;
                    },
                },
            ];
            for (;;) {
                const entries = menu();
                const choice = await ctx.ui.select(`Model tiers (editing ${scope})`, entries.map((entry) => entry.label));
                const entry = entries.find((candidate) => candidate.label === choice);
                if (!entry || !(await entry.choose()))
                    break;
            }
        },
    });
}
const DEFAULT_THINKING_CHOICE = "Default thinking (session setting)";
const THINKING_CHOICES = [DEFAULT_THINKING_CHOICE, ...THINKING_LEVELS];
function fromThinkingChoice(choice) {
    return THINKING_LEVELS.find((level) => level === choice);
}
/**
 * Interactive editor for a single tier: pick a base model, then a Pi CLI-style
 * thinking suffix or the session default.
 *
 * Returns the updated tiers object, or null if nothing changed.
 */
export async function editSingleTier(ctx, tiers, tierName) {
    const available = listAvailableModelSpecs(ctx.modelRegistry);
    if (available.length === 0) {
        ctx.ui.notify("No models are available to choose from. Check that a provider is signed in.", "warning");
        return null;
    }
    const current = tiers[tierName];
    const currentParts = splitModelSpecThinking(current, available);
    const title = current ? `Pick a model for "${tierName}" (current: ${current})` : `Pick a model for "${tierName}"`;
    const selectedModel = await pickModel(ctx, title, available, currentParts.modelSpec);
    if (!selectedModel)
        return null;
    const currentThinkingLabel = currentParts.thinkingLevel ?? DEFAULT_THINKING_CHOICE;
    const thinkingChoice = await ctx.ui.select(`Thinking for "${tierName}" tier (current: ${currentThinkingLabel})`, THINKING_CHOICES.map((choice) => String(choice)));
    if (!thinkingChoice)
        return null;
    const thinkingLevel = fromThinkingChoice(thinkingChoice);
    const result = formatModelSpecWithThinking(selectedModel, thinkingLevel);
    if (result === current)
        return null;
    ctx.ui.notify(`"${tierName}" tier → ${result}`, "info");
    return { ...tiers, [tierName]: result };
}
/**
 * Ask the user for one model spec; resolves undefined when they cancel.
 *
 * The terminal UI gets a scrollable `SelectList` with the current model
 * preselected. That list is a `ui.custom()` component, which needs a real
 * terminal: RPC hosts resolve `custom()` to undefined even though `ctx.hasUI`
 * is true there, so every other mode asks through `ui.select()`, which RPC
 * hosts implement over their extension-UI protocol.
 */
async function pickModel(ctx, title, models, preselected) {
    if (ctx.mode !== "tui")
        return ctx.ui.select(title, models);
    return ctx.ui.custom((tui, theme, _keybindings, done) => {
        const items = models.map((model) => ({ value: model, label: model }));
        const selectTheme = {
            selectedPrefix: (t) => theme.bg("selectedBg", theme.fg("accent", t)),
            selectedText: (t) => theme.bg("selectedBg", theme.bold(t)),
            description: (t) => theme.fg("muted", t),
            scrollInfo: (t) => theme.fg("dim", t),
            noMatch: (t) => theme.fg("warning", t),
        };
        const selectList = new SelectList(items, 12, selectTheme);
        const preselectedIndex = items.findIndex((item) => item.value === preselected);
        if (preselectedIndex >= 0)
            selectList.setSelectedIndex(preselectedIndex);
        selectList.onSelect = (item) => done(item.value);
        selectList.onCancel = () => done(undefined);
        const container = new Container();
        container.addChild(new Text(theme.fg("accent", title), 1, 0));
        container.addChild(new Spacer(1));
        container.addChild(selectList);
        container.addChild(new Spacer(1));
        container.addChild(new Text(theme.fg("dim", "↑↓ navigate  enter select  esc cancel  · thinking is chosen next"), 1, 0));
        return {
            render: (w) => container.render(w),
            invalidate: () => container.invalidate(),
            handleInput: (data) => {
                selectList.handleInput(data);
                tui.requestRender();
            },
        };
    });
}
