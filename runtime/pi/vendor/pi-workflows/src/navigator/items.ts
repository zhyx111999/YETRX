/** The runs pane's selectable items: their identities, and the filtered list every runs-pane operation shares. */

import { type SavedWorkflow, savedWorkflowRevision } from "../workflow-saved.js";
import { asText } from "./as-text.js";
import type { RunRow } from "./rows.js";

export type ItemKind = "run" | "saved";

/** A saved workflow is identified by its exact file revision, so an action never hits an edited or replaced copy. */
export type SavedItemIdentity = {
  kind: "saved";
  path: string;
  name: string;
  revision: string;
};

export type RunItemIdentity = { kind: "run"; runId: string };

export type ItemIdentity = RunItemIdentity | SavedItemIdentity;

export interface VisibleRunItem {
  kind: "run";
  identity: RunItemIdentity;
  row: RunRow;
}

export interface VisibleSavedItem {
  kind: "saved";
  identity: SavedItemIdentity;
  workflow: SavedWorkflow;
}

export type VisibleNavigatorItem = VisibleRunItem | VisibleSavedItem;

/** One coherent, filtered source of truth for every runs-pane operation. */
export interface NavigatorSnapshot {
  filter: string;
  items: VisibleNavigatorItem[];
}

export function savedIdentity(workflow: SavedWorkflow): SavedItemIdentity {
  return {
    kind: "saved",
    path: workflow.path,
    name: workflow.name,
    revision: savedWorkflowRevision(workflow),
  };
}

export function sameIdentity(a: ItemIdentity | undefined, b: ItemIdentity | undefined): boolean {
  if (!a || !b || a.kind !== b.kind) return false;
  if (a.kind === "run" && b.kind === "run") return a.runId === b.runId;
  const left = a as SavedItemIdentity;
  const right = b as SavedItemIdentity;
  return left.path === right.path && left.name === right.name && left.revision === right.revision;
}

/** Runs first, then saved workflows, keeping those that match `filter` case-insensitively. */
export function visibleItems(filter: string, runs: RunRow[], saved: SavedWorkflow[]): NavigatorSnapshot {
  const needle = filter.toLocaleLowerCase();
  const contains = (...values: unknown[]) =>
    !needle || values.some((value) => asText(value).toLocaleLowerCase().includes(needle));
  const items: VisibleNavigatorItem[] = [];
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
