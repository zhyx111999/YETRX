/** The runs pane's selectable items: their identities, and the filtered list every runs-pane operation shares. */
import { savedWorkflowRevision } from "../workflow-saved.js";
import { asText } from "./as-text.js";
export function savedIdentity(workflow) {
    return {
        kind: "saved",
        path: workflow.path,
        name: workflow.name,
        revision: savedWorkflowRevision(workflow),
    };
}
export function sameIdentity(a, b) {
    if (!a || !b || a.kind !== b.kind)
        return false;
    if (a.kind === "run" && b.kind === "run")
        return a.runId === b.runId;
    const left = a;
    const right = b;
    return left.path === right.path && left.name === right.name && left.revision === right.revision;
}
/** Runs first, then saved workflows, keeping those that match `filter` case-insensitively. */
export function visibleItems(filter, runs, saved) {
    const needle = filter.toLocaleLowerCase();
    const contains = (...values) => !needle || values.some((value) => asText(value).toLocaleLowerCase().includes(needle));
    const items = [];
    for (const row of runs) {
        if (contains(row.name, row.runId, row.status)) {
            items.push({ kind: "run", identity: { kind: "run", runId: row.runId }, row });
        }
    }
    for (const workflow of saved) {
        if (contains(workflow.name, workflow.description)) {
            items.push({ kind: "saved", identity: savedIdentity(workflow), workflow });
        }
    }
    return { filter, items };
}
