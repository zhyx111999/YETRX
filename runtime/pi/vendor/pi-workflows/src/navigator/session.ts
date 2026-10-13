/** What one open navigator overlay acts on, shared by its input handlers. */

import type { ExtensionAPI, ExtensionUIContext } from "@earendil-works/pi-coding-agent";
import type { WorkflowManager } from "../workflow-manager.js";
import type { WorkflowStorage } from "../workflow-saved.js";
import type { NavigatorModel, NavigatorOptions, NavigatorState } from "../workflow-ui.js";
import type { NavigatorSnapshot } from "./items.js";

export interface NavigatorSession {
  pi: ExtensionAPI;
  manager: WorkflowManager;
  ui: ExtensionUIContext;
  options: NavigatorOptions;
  model: NavigatorModel;
  state: NavigatorState;
}

/** The storage in effect now; the extension can replace it after session start. */
export function currentStorage(session: NavigatorSession): WorkflowStorage | undefined {
  return session.options.getStorage?.();
}

/** The runs-pane items as the user currently sees them, including a filter being typed. */
export function currentSnapshot(session: NavigatorSession): NavigatorSnapshot {
  return session.model.visible(session.state.effectiveFilter());
}
