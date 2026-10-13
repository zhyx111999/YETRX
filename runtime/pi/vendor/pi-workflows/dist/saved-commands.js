/**
 * Saved workflows as `/<name>` slash commands. Each saved workflow becomes a
 * command that runs its script, passing parsed arguments through as `args`.
 */
import { claimCommand, commandOwner, isCommandTaken } from "./command-registry.js";
import { startCommandRun } from "./command-run.js";
import { errorMessage } from "./errors.js";
import { PI_RESERVED_COMMANDS } from "./pi-reserved-commands.js";
function savedCommandOwnedByExtension(pi, name) {
    const owner = commandOwner(pi, name);
    return owner === "builtin" || owner === "saved";
}
/**
 * Pi cannot unregister a slash command and never reports a name clash. Refuse a
 * name Pi or another extension answers before a save or rename reaches disk;
 * Pi's own commands go first because its editor runs them whoever registered.
 */
export function savedWorkflowCommandAvailability(pi, name) {
    if (PI_RESERVED_COMMANDS.has(name))
        return { ok: false, message: `/${name} is a built-in Pi command.` };
    if (!isCommandTaken(pi, name) || savedCommandOwnedByExtension(pi, name))
        return { ok: true };
    return { ok: false, message: `/${name} is already provided by Pi or another extension.` };
}
export function parseCommandArgs(raw, parameters) {
    const out = {};
    const positional = [];
    for (const token of raw.trim().split(/\s+/).filter(Boolean)) {
        const eq = token.indexOf("=");
        if (eq > 0)
            out[token.slice(0, eq)] = token.slice(eq + 1);
        else
            positional.push(token);
    }
    out._ = positional.join(" ");
    out._raw = raw.trim();
    for (const [key, spec] of Object.entries(parameters ?? {})) {
        if (out[key] === undefined && spec.default !== undefined)
            out[key] = spec.default;
    }
    return out;
}
/**
 * Register `/<name>` for a saved workflow. `load` re-reads the workflow at each
 * invocation: Pi cannot unregister a command, so after a rename or delete the
 * lingering command must find nothing rather than run a frozen script.
 */
export function registerSavedWorkflow(pi, wf, getManager, load) {
    const availability = savedWorkflowCommandAvailability(pi, wf.name);
    if (!availability.ok)
        return availability;
    if (isCommandTaken(pi, wf.name))
        return { ok: true };
    try {
        pi.registerCommand(wf.name, {
            description: wf.description || `Saved workflow: ${wf.name}`,
            async handler(args, ctx) {
                const live = load();
                if (!live) {
                    ctx.ui.notify(`/${wf.name} is not available in this project — reload the session to drop the stale command.`, "warning");
                    return;
                }
                startCommandRun(getManager(), ctx, {
                    name: live.name,
                    script: live.script,
                    args: parseCommandArgs(args, live.parameters),
                    exec: { toolset: live.toolset },
                    deliverable: "result",
                });
            },
        });
        claimCommand(pi, wf.name, "saved");
        return { ok: true };
    }
    catch (error) {
        return { ok: false, message: errorMessage(error) };
    }
}
export function registerAllSavedWorkflows(pi, getStorage, getManager) {
    for (const workflow of getStorage().list()) {
        registerSavedWorkflow(pi, workflow, getManager, () => getStorage().load(workflow.name));
    }
}
