/**
 * Interactive-session wiring for the parent model: the hidden parent prompt
 * sent with typed messages, the `/workflows run` directive, and the
 * `/workflows-progress` panel preference command.
 */
import { errorMessage } from "./errors.js";
import { PARENT_PROMPT_MESSAGE_TYPE, PARENT_WORKFLOW_PROMPT } from "./parent-prompt.js";
/**
 * The directive for the explicit `/workflows run <prompt>` command. The user
 * typed a command whose only purpose is to run a workflow, so the model is told
 * to call the tool rather than judge whether delegation is worthwhile. It
 * avoids MUST/ONLY wording, which produced a bare tool call that left the user
 * at an idle prompt, and says the turn ending is expected.
 */
export function buildForcedWorkflowPrompt(text) {
    return [
        text,
        "",
        "---",
        "[/workflows run — you ran an explicit command to execute a workflow for this request.",
        "Call the `subagents` tool now: list the tasks to fan this request out across subagents,",
        "or write a script when the steps depend on each other. (This is a direct command, not a",
        "heuristic guess, so do not answer in prose instead of starting the subagents.)",
        "The `subagents` tool always runs in the background: this turn will end and the result is delivered back into the conversation automatically when it finishes — that's expected, not a stall, so you do not need to stay and block.]",
    ].join("\n");
}
/**
 * Send the parent prompt as a hidden message after each message the user types,
 * leaving the user's own text unchanged in the transcript.
 *
 * Only `input` knows where text came from, and only `before_agent_start` can add
 * a message, so `input` records eligible text and `before_agent_start` sends the
 * prompt when its prompt is exactly that text. Slash commands, steer and
 * follow-up messages typed during a run, and RPC or extension input are not
 * recorded.
 */
export function installParentPromptHook(pi) {
    let typedPrompt;
    pi.on("input", (event) => {
        // Every input replaces the record, so text that never started a run (handled
        // by another extension, or a failed submit) cannot match a later run.
        typedPrompt = isTypedPrompt(event) ? event.text : undefined;
    });
    pi.on("before_agent_start", (event) => {
        const matches = typedPrompt !== undefined && event.prompt === typedPrompt;
        typedPrompt = undefined;
        if (!matches)
            return;
        return { message: { customType: PARENT_PROMPT_MESSAGE_TYPE, content: PARENT_WORKFLOW_PROMPT, display: false } };
    });
}
function isTypedPrompt(event) {
    return event.source === "interactive" && !event.streamingBehavior && !event.text.trimStart().startsWith("/");
}
/**
 * Register the bottom progress-panel preference command:
 *  - `/workflows-progress compact|detailed|status` — switch (or report) the panel mode.
 *  - `/workflows-progress max <1-1000>` — cap agents shown per phase in detailed mode.
 * Both persist via `settingsStore` and take effect on the next live run (the panel
 * live-reads its settings), so no session restart is needed. The panel has no
 * other source for these values, so a failed save changes nothing and is
 * reported that way.
 */
export function registerWorkflowProgressCommands(pi, settingsStore) {
    pi.registerCommand?.("workflows-progress", {
        description: "Bottom progress panel: compact | detailed | status | max <N>",
        async handler(args, _ctx) {
            const trimmed = args.trim();
            const say = (content) => pi.sendMessage({ customType: "workflows-progress", content, display: true });
            const spaceIdx = trimmed.indexOf(" ");
            const verb = (spaceIdx === -1 ? trimmed : trimmed.slice(0, spaceIdx)).toLowerCase();
            const rest = spaceIdx === -1 ? "" : trimmed.slice(spaceIdx + 1).trim();
            if (verb === "compact" || verb === "detailed") {
                const failure = saveProgressSettings(settingsStore, { progressPanelMode: verb });
                await say(failure === undefined
                    ? `Workflow progress panel set to ${verb} — takes effect on the next render of a live run (no restart needed).`
                    : `Workflow progress panel mode ${verb} was not saved (${failure}); the panel is still ${loadProgressMode(settingsStore)}.`);
                return;
            }
            if (verb === "max") {
                if (!rest) {
                    await say(`Detailed progress shows up to ${loadProgressMaxAgents(settingsStore)} agents per phase. Usage: /workflows-progress max <1-1000>`);
                    return;
                }
                const n = Number.parseInt(rest, 10);
                if (!Number.isFinite(n) || n < 1) {
                    await say(`Invalid value "${rest}". Usage: /workflows-progress max <1-1000> (a whole number ≥ 1).`);
                    return;
                }
                const clamped = Math.min(1000, n);
                const failure = saveProgressSettings(settingsStore, { progressPanelMaxAgents: clamped });
                await say(failure === undefined
                    ? `Detailed progress now shows up to ${clamped} agents per phase.`
                    : `Max ${clamped} was not saved (${failure}); detailed progress still shows up to ${loadProgressMaxAgents(settingsStore)} agents per phase.`);
                return;
            }
            await say(`Workflow progress panel is ${loadProgressMode(settingsStore)}, showing up to ${loadProgressMaxAgents(settingsStore)} agents per phase. Usage: /workflows-progress compact | detailed | status | max <N>`);
        },
    });
}
/** Save panel preferences; returns why the save failed, or undefined when it succeeded. */
function saveProgressSettings(settingsStore, settings) {
    try {
        settingsStore.save(settings);
        return undefined;
    }
    catch (error) {
        return errorMessage(error);
    }
}
function loadProgressMode(settingsStore) {
    try {
        return settingsStore.load().progressPanelMode ?? "compact";
    }
    catch {
        return "compact";
    }
}
function loadProgressMaxAgents(settingsStore) {
    try {
        return settingsStore.load().progressPanelMaxAgents ?? 8;
    }
    catch {
        return 8;
    }
}
