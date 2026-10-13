/** Two-step confirmation for destructive actions, bound to the exact view and target that asked. */
import { replacedWorkflowLabel } from "../run-save.js";
import { sameIdentity } from "./items.js";
/** The key that asked for a confirmation is the only one that accepts it. */
export const CONFIRM_KEYS = {
    pause: "p",
    stop: "x",
    deleteSaved: "x",
    replaceSaved: "s",
};
/** The footer prompt. A replace names the workflow and ends with its file, so a narrow overlay cuts only the path. */
export function confirmationPrompt(request) {
    const key = CONFIRM_KEYS[request.action];
    if (request.action === "replaceSaved") {
        const { existing } = request.save;
        return `Press ${key} again to replace the ${replacedWorkflowLabel(existing)}, or Esc to cancel. File: ${existing.path}`;
    }
    return `Press ${key} again to confirm ${request.action === "deleteSaved" ? "delete" : request.action}, or Esc to cancel.`;
}
export function sameConfirmationContext(current, requested) {
    return (current.kind === requested.kind &&
        current.runId === requested.runId &&
        current.phase === requested.phase &&
        current.agentId === requested.agentId &&
        ((!current.savedIdentity && !requested.savedIdentity) ||
            sameIdentity(current.savedIdentity, requested.savedIdentity)));
}
