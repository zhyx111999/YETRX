/**
 * The bundled `/deep-research` command. It starts its generated workflow in
 * the background (see command-run.ts); the report is delivered back into the
 * conversation when the run finishes.
 */
import { findBuiltinWorkflow } from "./builtin-workflows.js";
import { claimCommand, isCommandTaken } from "./command-registry.js";
import { startCommandRun } from "./command-run.js";
import { errorMessage } from "./errors.js";
import { parseCommandArgs } from "./saved-commands.js";
const DEEP_RESEARCH = "deep-research";
export function registerBuiltinWorkflows(pi, { getManager, getCwd, getStorage }) {
    if (isCommandTaken(pi, DEEP_RESEARCH))
        return;
    /**
     * A saved workflow named like the built-in runs instead of it. Saved commands
     * register after built-ins and skip taken names, so the check happens here,
     * at invocation. The whole command text becomes `question`, exactly what the
     * built-in would research, unless the user named `question=` explicitly or
     * typed nothing (then a declared default applies).
     */
    const runSavedShadow = (rawArgs, ctx) => {
        const saved = getStorage().load(DEEP_RESEARCH);
        if (!saved)
            return false;
        const args = parseCommandArgs(rawArgs, saved.parameters);
        const whole = rawArgs.trim();
        if (whole && !/(?:^|\s)question=/.test(rawArgs))
            args.question = whole;
        startCommandRun(getManager(), ctx, {
            name: DEEP_RESEARCH,
            script: saved.script,
            args,
            exec: { toolset: saved.toolset },
            deliverable: "result",
        });
        return true;
    };
    pi.registerCommand(DEEP_RESEARCH, {
        description: "Research a question across the web and return sourced claims",
        async handler(rawArgs, ctx) {
            if (runSavedShadow(rawArgs, ctx))
                return;
            const question = rawArgs.trim();
            if (!question)
                return ctx.ui.notify(`Usage: /${DEEP_RESEARCH} <question>`, "warning");
            // The same registry entry the subagents tool's `name` input runs, so both
            // start the identical script with the identical tools.
            const builtin = findBuiltinWorkflow(DEEP_RESEARCH);
            if (!builtin)
                throw new Error(`internal error: no built-in workflow registered for "${DEEP_RESEARCH}"`);
            let invocation;
            try {
                invocation = builtin.resolve(getCwd(), { question });
            }
            catch (error) {
                return ctx.ui.notify(`/${DEEP_RESEARCH}: ${errorMessage(error)}`, "warning");
            }
            startCommandRun(getManager(), ctx, {
                name: DEEP_RESEARCH,
                script: invocation.script,
                args: { question },
                exec: { tools: invocation.tools, toolset: invocation.toolset },
                deliverable: "report",
            });
        },
    });
    claimCommand(pi, DEEP_RESEARCH, "builtin");
}
