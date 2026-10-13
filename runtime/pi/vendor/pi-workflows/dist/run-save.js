/**
 * Saving a run's script as a project workflow, shared by `/workflows save` and
 * the navigator. A name that already holds a different workflow is replaced
 * only after the caller has asked the user; saving identical content again
 * writes nothing, so the backup still holds the version before it.
 */
/**
 * The saved workflow a run becomes under `name`. Its toolset goes with it, so
 * running the saved workflow gets the same tools (deep-research's web tools).
 */
export function runSaveDraft(run, name) {
    if (!run.script)
        return undefined;
    return {
        name,
        description: run.workflowName,
        script: run.script,
        ...(run.toolset && { toolset: run.toolset }),
    };
}
export function planRunSave(storage, draft) {
    const existing = storage.load(draft.name);
    if (!existing)
        return { kind: "create", draft };
    return sameContent(existing, draft) ? { kind: "unchanged", existing } : { kind: "replace", draft, existing };
}
/** A save writes no parameters, so an existing workflow that declares some is different content. */
function sameContent(existing, draft) {
    return (existing.script === draft.script &&
        existing.description === draft.description &&
        (existing.toolset ?? "") === (draft.toolset ?? "") &&
        Object.keys(existing.parameters ?? {}).length === 0);
}
/** Which workflow a replace affects, for the question the user answers; callers also show its path. */
export function replacedWorkflowLabel(existing) {
    return `${existing.location} workflow /${existing.name}`;
}
/**
 * Write a planned save to the project. A replaced project workflow must still
 * be exactly the version the user agreed to replace; a user workflow of the
 * same name is shadowed, not changed, so then the project target must be free.
 */
export function commitRunSave(storage, plan) {
    const replacing = plan.kind === "replace" && plan.existing.location === "project" ? plan.existing : undefined;
    const result = storage.save(plan.draft, { location: "project", replacing });
    // A workflow that appeared since the plan was made makes the plan stale, as a changed one does.
    if (!result.ok && result.code === "conflict") {
        return {
            ok: false,
            code: "stale",
            message: `/${plan.draft.name} changed after it was checked; nothing was saved.`,
        };
    }
    return result;
}
