/** Two-step confirmation for destructive actions, bound to the exact view and target that asked. */

import type { PhaseKey } from "../phase-groups.js";
import { type RunSaveReplace, replacedWorkflowLabel } from "../run-save.js";
import type { ViewKind } from "../workflow-ui.js";
import { type RunItemIdentity, type SavedItemIdentity, sameIdentity } from "./items.js";

/**
 * What a confirmation would do once accepted. `target` is the item the key
 * acted on; a replace also carries the save it showed, so accepting writes
 * exactly what the prompt named.
 */
export type ConfirmRequest =
  | { action: "pause" | "stop"; target: RunItemIdentity }
  | { action: "deleteSaved"; target: SavedItemIdentity }
  | { action: "replaceSaved"; target: RunItemIdentity; save: RunSaveReplace };

export type ConfirmKind = ConfirmRequest["action"];

/** The key that asked for a confirmation is the only one that accepts it. */
export const CONFIRM_KEYS: Readonly<Record<ConfirmKind, string>> = {
  pause: "p",
  stop: "x",
  deleteSaved: "x",
  replaceSaved: "s",
};

/** The footer prompt. A replace names the workflow and ends with its file, so a narrow overlay cuts only the path. */
export function confirmationPrompt(request: ConfirmRequest): string {
  const key = CONFIRM_KEYS[request.action];
  if (request.action === "replaceSaved") {
    const { existing } = request.save;
    return `Press ${key} again to replace the ${replacedWorkflowLabel(existing)}, or Esc to cancel. File: ${existing.path}`;
  }
  return `Press ${key} again to confirm ${request.action === "deleteSaved" ? "delete" : request.action}, or Esc to cancel.`;
}

/** The view a confirmation was requested from; confirming anywhere else cancels it. */
export type ConfirmationContext = {
  kind: ViewKind;
  runId?: string;
  phase?: PhaseKey;
  agentId?: number;
  savedIdentity?: SavedItemIdentity;
};

export type PendingConfirmation = {
  request: ConfirmRequest;
  cursor: number;
  filter: string;
  context: ConfirmationContext;
};

export function sameConfirmationContext(current: ConfirmationContext, requested: ConfirmationContext): boolean {
  return (
    current.kind === requested.kind &&
    current.runId === requested.runId &&
    current.phase === requested.phase &&
    current.agentId === requested.agentId &&
    ((!current.savedIdentity && !requested.savedIdentity) ||
      sameIdentity(current.savedIdentity, requested.savedIdentity))
  );
}
