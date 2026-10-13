/** Saved-workflow actions started from the navigator: save a run, rename, and delete, keeping `/name` commands in sync. */
import { commitRunSave, planRunSave, runSaveDraft } from "../run-save.js";
import { registerSavedWorkflow, savedWorkflowCommandAvailability } from "../saved-commands.js";
import { isSafeSavedWorkflowName, SAVED_WORKFLOW_NAME_RULE } from "../workflow-saved.js";
import { sameIdentity, savedIdentity } from "./items.js";
import { currentSnapshot, currentStorage } from "./session.js";
/** Register `/<name>` for a saved workflow, resolving manager and storage live at run time. */
function registerSavedCommand(session, saved) {
    const { pi, manager, options } = session;
    return registerSavedWorkflow(pi, saved, options.getManager ?? (() => manager), () => currentStorage(session)?.load(saved.name));
}
function findSaved(session, target) {
    return session.model.saved().find((candidate) => sameIdentity(target, savedIdentity(candidate)));
}
/** Register the saved workflow's command and report `summary`, or why the command is missing. */
function announceSaved(session, saved, summary) {
    const registered = registerSavedCommand(session, saved);
    session.ui.notify(registered.ok ? summary : `${summary}, but ${registered.message}`, registered.ok ? "info" : "error");
}
/**
 * Save the selected run's script as a project workflow. A different workflow
 * already under that name is replaced only after `s` is pressed again; the
 * same content is not written again, so its backup keeps the earlier version.
 */
export function saveRunAsWorkflow(session, runId) {
    const { pi, manager, ui, state } = session;
    const run = runId ? manager.findRun(runId) : undefined;
    const storage = currentStorage(session);
    if (!run?.script) {
        ui.notify("No saved run script to save", "warning");
        return;
    }
    if (!storage) {
        ui.notify("Saving is not available (no storage)", "error");
        return;
    }
    const name = run.workflowName || "workflow";
    const availability = savedWorkflowCommandAvailability(pi, name);
    if (!availability.ok) {
        ui.notify(availability.message, "error");
        return;
    }
    const draft = runSaveDraft(run, name);
    if (!draft)
        return;
    const plan = planRunSave(storage, draft);
    if (plan.kind === "unchanged")
        announceSaved(session, plan.existing, `/${name} is already saved with this script`);
    else if (plan.kind === "create")
        writeRunSave(session, plan);
    else
        state.beginConfirmation({ action: "replaceSaved", target: { kind: "run", runId: run.runId }, save: plan }, currentSnapshot(session));
}
/** Write a new workflow, or a replace the user confirmed, then register its command. */
export function writeRunSave(session, plan) {
    const { ui } = session;
    const storage = currentStorage(session);
    if (!storage) {
        ui.notify("Saving is not available (no storage)", "error");
        return;
    }
    const result = commitRunSave(storage, plan);
    if (result.ok)
        announceSaved(session, result.workflow, `Saved /${result.workflow.name}`);
    else if (result.code === "stale" || result.code === "busy")
        ui.notify(`${result.message} Press s to check again.`, "warning");
    else
        ui.notify(result.message, "error");
}
/** Apply the typed rename to the workflow it was started on, then move its command. */
export function commitRename(session) {
    const { pi, ui, model, state } = session;
    if (!isSafeSavedWorkflowName(state.draft)) {
        ui.notify(SAVED_WORKFLOW_NAME_RULE, "warning");
        return;
    }
    const rename = state.takeRename();
    if (!rename)
        return;
    const workflow = findSaved(session, rename.target);
    if (!workflow) {
        ui.notify("Saved workflow no longer exists.", "warning");
        return;
    }
    if (rename.name === workflow.name) {
        ui.notify(`Kept /${workflow.name}`, "info");
        return;
    }
    const availability = savedWorkflowCommandAvailability(pi, rename.name);
    if (!availability.ok) {
        ui.notify(availability.message, "error");
        return;
    }
    const result = model.renameSaved(workflow, rename.name);
    if (!result.ok) {
        ui.notify(result.message, "error");
        return;
    }
    if (!result.workflow) {
        ui.notify("Rename did not return a saved workflow.", "error");
        return;
    }
    const saved = result.workflow;
    state.replaceSavedIdentity(rename.target, saved);
    const registered = registerSavedCommand(session, saved);
    if (!registered.ok)
        ui.notify(`Renamed /${workflow.name} to /${saved.name}, but ${registered.message}`, "error");
    else
        ui.notify(`Renamed /${workflow.name} to /${saved.name}`, "info");
}
/** Delete the confirmed saved workflow, leaving its detail page if that is where the user was. */
export function deleteSavedWorkflow(session, target) {
    const { ui, model, state } = session;
    const workflow = findSaved(session, target);
    if (!workflow) {
        ui.notify("Saved workflow no longer exists.", "warning");
        return;
    }
    const result = model.deleteSaved(workflow);
    if (result.ok)
        ui.notify(`Deleted /${workflow.name}`, "info");
    else
        ui.notify(result.message, "error");
    if (result.ok && state.kind === "savedDetail")
        state.back();
}
