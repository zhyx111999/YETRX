/**
 * Interactive `/workflows` navigator:
 *
 *   runs ──enter──▶ phases ──enter──▶ agents ──enter──▶ agent detail ──enter──▶ full pager
 *        ◀──esc───        ◀──esc────         ◀──esc────              ◀──esc────
 *        ◀── (saved items in runs view) ──enter──▶ saved detail
 *
 * Keys: ↑/↓ (or j/k) select · enter/→ drill in · esc/← back (esc at top closes)
 *       On runs: / filter · p pause · x stop · r restart · s save · q quit
 *       On saved: r rename · x delete · q quit
 *
 * How the pieces fit:
 * - NavigatorModel reads runs and saved workflows, preferring live snapshots.
 * - NavigatorState is the view stack, selection, input mode, and pending
 *   confirmation; it never touches the terminal.
 * - renderNavigator draws one frame from the two (views live in ./navigator/).
 * - keyToAction maps a key to an action; ./navigator/key-input.ts applies it.
 * - openWorkflowNavigator wires them into a pi-tui overlay that redraws on
 *   manager events.
 */

import {
  type ExtensionAPI,
  type ExtensionUIContext,
  getMarkdownTheme,
  type Theme,
} from "@earendil-works/pi-coding-agent";
import type { Component, Focusable, MarkdownTheme, TUI } from "@earendil-works/pi-tui";
import { parseKey } from "@earendil-works/pi-tui";
import type { ThemeLike, WorkflowAgentSnapshot } from "./display.js";
import { errorMessage } from "./errors.js";
import { asText } from "./navigator/as-text.js";
import {
  type ConfirmationContext,
  type ConfirmKind,
  type ConfirmRequest,
  type PendingConfirmation,
  sameConfirmationContext,
} from "./navigator/confirmation.js";
import { FrameMemo } from "./navigator/frame-memo.js";
import { dropLastGrapheme } from "./navigator/input-text.js";
import {
  type ItemIdentity,
  type ItemKind,
  type NavigatorSnapshot,
  type SavedItemIdentity,
  sameIdentity,
  savedIdentity,
  type VisibleNavigatorItem,
  visibleItems,
} from "./navigator/items.js";
import { handleConfirmKey, handleTextInput, runBrowseAction } from "./navigator/key-input.js";
import { subscribeToRunUpdates } from "./navigator/live-updates.js";
import { boxOverlayLines, overlayLayout, overlayOptions } from "./navigator/overlay-box.js";
import { persistedToSnapshot } from "./navigator/persisted-snapshot.js";
import { NavigatorTextRenderCache } from "./navigator/render-cache.js";
import { renderNavigatorFrame } from "./navigator/render-frame.js";
import {
  type AgentRow,
  agentRowsByPhase,
  agentRowsInPhase,
  type PhaseRow,
  phaseRows,
  type RunRow,
  type RunSnapshot,
  runRow,
} from "./navigator/rows.js";
import { currentSnapshot, type NavigatorSession } from "./navigator/session.js";
import type { PhaseKey } from "./phase-groups.js";
import type { PersistedRunState } from "./run-persistence.js";
import type { WorkflowManager } from "./workflow-manager.js";
import type { SavedWorkflow, SavedWorkflowMutationResult, WorkflowStorage } from "./workflow-saved.js";

export type ViewKind = "runs" | "phases" | "agents" | "detail" | "savedDetail";
export type NavigatorInputMode = "browse" | "filter" | "rename" | "confirm";

type NavigatorStorage = Pick<WorkflowStorage, "list" | "delete" | "rename">;

/** Reads run/phase/agent data from the manager, preferring live snapshots. */
export class NavigatorModel {
  private readonly frame = new FrameMemo();
  private readonly getStorage: () => NavigatorStorage | undefined;
  // Rehydrating a persisted run stringifies every agent's result, so keep the
  // last one keyed by the parsed record object: browsing an unchanged record
  // reuses it, and a fresh disk read (a new object) invalidates it.
  private rehydrated?: { record: PersistedRunState; value: RunSnapshot };

  constructor(
    private readonly manager: Pick<WorkflowManager, "listRuns" | "getRun">,
    storage?: NavigatorStorage | (() => NavigatorStorage | undefined),
  ) {
    this.getStorage = typeof storage === "function" ? storage : () => storage;
  }

  /** Share persisted data across all model lookups performed by one render. */
  withRenderFrame<T>(render: () => T): T {
    return this.frame.run(render);
  }

  private persistedRuns(): PersistedRunState[] {
    return this.frame.get("runs", () => this.manager.listRuns());
  }

  private snapshot(runId: string): RunSnapshot | undefined {
    return this.frame.get(`snapshot:${runId}`, () => {
      const live = this.manager.getRun(runId);
      if (live) return { snapshot: live.snapshot, status: live.status };
      const record = this.persistedRuns().find((r) => r.runId === runId);
      if (!record) return undefined;
      if (this.rehydrated?.record !== record) {
        this.rehydrated = { record, value: { snapshot: persistedToSnapshot(record), status: record.status } };
      }
      return this.rehydrated.value;
    });
  }

  runs(): RunRow[] {
    return this.persistedRuns().map((record) => runRow(record, this.manager.getRun(record.runId)));
  }

  /** Return saved workflows sorted by name, or [] when no storage configured. */
  saved(): SavedWorkflow[] {
    const storage = this.getStorage();
    if (!storage) return [];
    return this.frame.get("saved", () => storage.list().sort((a, b) => a.name.localeCompare(b.name)));
  }

  /** Build the sole filtered item list used by list rendering, footer, drill, and actions. */
  visible(filter: string): NavigatorSnapshot {
    return visibleItems(filter, this.runs(), this.saved());
  }

  /** Delete exactly the currently visible saved source. */
  deleteSaved(workflow: SavedWorkflow): SavedWorkflowMutationResult {
    const storage = this.getStorage();
    if (!storage) return { ok: false, code: "io-error", message: "Saving is not available (no storage)." };
    return storage.delete(workflow);
  }

  /** Rename exactly the currently visible saved source. */
  renameSaved(workflow: SavedWorkflow, name: string): SavedWorkflowMutationResult {
    const storage = this.getStorage();
    if (!storage) return { ok: false, code: "io-error", message: "Saving is not available (no storage)." };
    return storage.rename(workflow, name);
  }

  runName(runId: string): string {
    return asText(this.snapshot(runId)?.snapshot.name ?? runId);
  }

  runStatus(runId: string): string {
    return asText(this.snapshot(runId)?.status ?? "unknown");
  }

  phases(runId: string): PhaseRow[] {
    const snap = this.snapshot(runId)?.snapshot;
    return snap ? phaseRows(snap) : [];
  }

  agents(runId: string, phase: PhaseKey): AgentRow[] {
    const snap = this.snapshot(runId)?.snapshot;
    return snap ? agentRowsInPhase(snap, phase) : [];
  }

  /** All agents grouped by phase in one pass, for views that render every phase. */
  agentsByPhase(runId: string): Map<PhaseKey, AgentRow[]> {
    const snap = this.snapshot(runId)?.snapshot;
    return snap ? agentRowsByPhase(snap) : new Map();
  }

  agentDetail(runId: string, agentId: number): WorkflowAgentSnapshot | undefined {
    return this.snapshot(runId)?.snapshot.agents.find((a) => a.id === agentId);
  }
}

/** One level of the view stack. In the runs pane the selection is an item
 * identity; the cursor only records where that identity is drawn. */
type StackFrame = {
  kind: ViewKind;
  cursor: number;
  runId?: string;
  /** The drilled-into phase; undefined while no phase is open. */
  phase?: PhaseKey;
  agentId?: number;
  savedName?: string;
  savedIdentity?: SavedItemIdentity;
  selected?: ItemIdentity;
};

/** Navigation state machine: view stack, selection, text input, and pending confirmation. */
export class NavigatorState {
  private stack: StackFrame[] = [{ kind: "runs", cursor: 0 }];
  private pending?: PendingConfirmation;
  private renameTarget?: SavedItemIdentity;
  private filterSelection?: ItemIdentity;
  mode: NavigatorInputMode = "browse";
  filter = "";
  draft = "";
  scroll = 0;
  tailing = false;
  pagerOpen = false;
  private pageSize = 1;
  private top(): StackFrame {
    return this.stack[this.stack.length - 1];
  }
  get kind(): ViewKind {
    return this.top().kind;
  }
  get cursor(): number {
    return this.top().cursor;
  }
  set cursor(val: number) {
    this.cancelConfirmation();
    this.top().cursor = val;
  }
  get runId(): string | undefined {
    return this.top().runId;
  }
  get phase(): PhaseKey | undefined {
    return this.top().phase;
  }
  get agentId(): number | undefined {
    return this.top().agentId;
  }
  get savedName(): string | undefined {
    return this.top().savedName;
  }
  get confirmation(): ConfirmRequest | undefined {
    return this.pending?.request;
  }

  /** Reconcile selection after any list/filter/manager change without drifting. */
  reconcile(snapshot: NavigatorSnapshot): void {
    if (this.kind !== "runs") return;
    // A pending confirmation survives refreshes on purpose: token updates fire
    // several times per second per streaming agent, so cancelling here would
    // make the second keypress impossible. confirm() re-checks the target.
    const top = this.top();
    const index = snapshot.items.findIndex((item) => sameIdentity(top.selected, item.identity));
    if (index >= 0) {
      top.cursor = index;
      return;
    }
    // Preserve the nearest row when filtering, deletion, rename, or a manager
    // refresh removes the previous identity. A non-empty list always has one.
    top.selected = undefined;
    top.cursor = snapshot.items.length ? Math.max(0, Math.min(top.cursor, snapshot.items.length - 1)) : 0;
    if (snapshot.items.length) top.selected = snapshot.items[top.cursor]?.identity;
  }
  /** The selected item for drawing; a vanished selection moves to its nearest row. */
  currentItem(snapshot: NavigatorSnapshot): VisibleNavigatorItem | undefined {
    this.reconcile(snapshot);
    return this.selectedItem(snapshot);
  }
  /**
   * The item an action applies to: exactly the one the user last saw selected.
   * Unlike currentItem() it never moves the selection, so a run or workflow
   * that vanished since the last redraw (deleted, renamed, filtered out) gives
   * no target rather than its neighbour.
   */
  selectedItem(snapshot: NavigatorSnapshot): VisibleNavigatorItem | undefined {
    // Nothing is selected before the first draw; the row under the cursor is then the selection.
    if (!this.top().selected) this.reconcile(snapshot);
    const selected = this.top().selected;
    return selected ? snapshot.items.find((item) => sameIdentity(selected, item.identity)) : undefined;
  }
  /** The kind of the row selected in the runs list, whether or not it is still listed. */
  selectedKind(snapshot: NavigatorSnapshot): ItemKind | undefined {
    if (this.kind !== "runs") return undefined;
    return this.selectedItem(snapshot)?.kind ?? this.top().selected?.kind;
  }
  /** Whether the runs list's selection names an item that is no longer listed. */
  selectionVanished(snapshot: NavigatorSnapshot): boolean {
    return this.kind === "runs" && this.top().selected !== undefined && !this.selectedItem(snapshot);
  }
  clamp(count: number) {
    const top = this.top();
    top.cursor = count <= 0 ? 0 : Math.max(0, Math.min(top.cursor, count - 1));
  }
  moveRuns(delta: number, snapshot: NavigatorSnapshot): void {
    this.cancelConfirmation();
    this.reconcile(snapshot);
    if (!snapshot.items.length) return;
    const current = snapshot.items.findIndex((item) => sameIdentity(this.top().selected, item.identity));
    const base = current >= 0 ? current : this.cursor;
    const next = (base + delta + snapshot.items.length) % snapshot.items.length;
    this.top().cursor = next;
    this.top().selected = snapshot.items[next]?.identity;
  }
  /** Detail pages scroll their body; moving in the agent detail opens its pager, and scrolling up stops tailing. */
  private scrollBody(delta: number): void {
    if (this.kind === "detail") this.pagerOpen = true;
    if (delta < 0) this.tailing = false;
    this.scroll = Math.max(0, this.scroll + delta);
  }
  move(delta: number, count: number, snapshot?: NavigatorSnapshot) {
    if (this.kind === "detail" || this.kind === "savedDetail") {
      this.scrollBody(delta);
      return;
    }
    if (count <= 0) return;
    this.cancelConfirmation();
    const top = this.top();
    top.cursor = (top.cursor + delta + count) % count;
    if (this.kind === "runs" && snapshot) top.selected = snapshot.items[top.cursor]?.identity;
  }
  setPageSize(rows: number) {
    this.pageSize = Math.max(1, rows);
  }
  movePage(direction: -1 | 1, count: number, snapshot?: NavigatorSnapshot) {
    const delta = direction * Math.max(1, this.pageSize - 1);
    if (this.kind === "detail" || this.kind === "savedDetail") {
      this.scrollBody(delta);
      return;
    }
    if (count <= 0) return;
    this.cancelConfirmation();
    this.top().cursor = Math.max(0, Math.min(count - 1, this.cursor + delta));
    if (this.kind === "runs" && snapshot) this.top().selected = snapshot.items[this.cursor]?.identity;
  }
  jump(edge: "start" | "end", count: number, snapshot?: NavigatorSnapshot) {
    if (this.kind === "detail" || this.kind === "savedDetail") {
      if (this.kind === "detail") this.pagerOpen = true;
      this.tailing = this.kind === "detail" && edge === "end";
      this.scroll = edge === "start" ? 0 : Number.MAX_SAFE_INTEGER;
      return;
    }
    this.cancelConfirmation();
    this.cursor = edge === "start" || count <= 0 ? 0 : count - 1;
    if (this.kind === "runs" && snapshot) this.top().selected = snapshot.items[this.cursor]?.identity;
  }
  openPager(): boolean {
    if (this.kind !== "detail") return false;
    if (!this.pagerOpen) {
      this.pagerOpen = true;
      this.scroll = 0;
    }
    return true;
  }
  togglePager(): boolean {
    if (this.kind !== "detail") return false;
    if (!this.pagerOpen) return this.openPager();
    this.pagerOpen = false;
    this.scroll = 0;
    this.tailing = false;
    return false;
  }
  toggleTail(): boolean {
    if (this.kind !== "detail") return false;
    this.pagerOpen = true;
    this.tailing = !this.tailing;
    if (this.tailing) this.scroll = Number.MAX_SAFE_INTEGER;
    return this.tailing;
  }
  drill(model: NavigatorModel, provided?: NavigatorSnapshot): boolean {
    const top = this.top();
    if (top.kind === "runs") {
      const snapshot = provided ?? model.visible(this.filter);
      const item = this.selectedItem(snapshot);
      if (!item) return false;
      this.cancelConfirmation();
      if (item.kind === "run") {
        this.filter = "";
        this.mode = "browse";
        this.stack.push({ kind: "phases", cursor: 0, runId: item.row.runId });
      } else {
        this.scroll = 0;
        this.tailing = false;
        this.pagerOpen = false;
        this.stack.push({
          kind: "savedDetail",
          cursor: 0,
          savedName: item.workflow.name,
          savedIdentity: item.identity,
        });
      }
      return true;
    }
    if (top.kind === "phases" && top.runId) {
      const phase = model.phases(top.runId)[top.cursor];
      if (!phase) return false;
      this.stack.push({ kind: "agents", cursor: 0, runId: top.runId, phase: phase.phase });
      return true;
    }
    // phase("") is a legal phase, so test for presence rather than truthiness.
    if (top.kind === "agents" && top.runId && top.phase !== undefined) {
      const agent = model.agents(top.runId, top.phase)[top.cursor];
      if (!agent) return false;
      this.scroll = 0;
      this.tailing = false;
      this.pagerOpen = false;
      this.stack.push({ kind: "detail", cursor: 0, runId: top.runId, phase: top.phase, agentId: agent.id });
      return true;
    }
    return false;
  }
  back(): boolean {
    this.cancelConfirmation();
    if (this.mode === "filter" || this.mode === "rename") {
      this.cancelInput();
      return true;
    }
    // Esc first clears an applied filter while keeping the navigator open. A
    // second Esc with no filter follows the ordinary stack/back behavior.
    if (this.kind === "runs" && this.filter) {
      this.filter = "";
      this.draft = "";
      return true;
    }
    if (this.kind === "detail" && this.pagerOpen) {
      this.pagerOpen = false;
      this.scroll = 0;
      this.tailing = false;
      return true;
    }
    if (this.stack.length <= 1) return false;
    this.stack.pop();
    this.scroll = 0;
    this.tailing = false;
    this.pagerOpen = false;
    return true;
  }
  activeRunId(model: NavigatorModel, snapshot?: NavigatorSnapshot): string | undefined {
    if (this.runId) return this.runId;
    if (this.kind !== "runs") return undefined;
    const item = this.selectedItem(snapshot ?? model.visible(this.filter));
    return item?.kind === "run" ? item.row.runId : undefined;
  }
  activeSaved(snapshot: NavigatorSnapshot, allSaved: SavedWorkflow[]): SavedWorkflow | undefined {
    if (this.kind === "runs") {
      const item = this.selectedItem(snapshot);
      return item?.kind === "saved" ? item.workflow : undefined;
    }
    if (this.kind === "savedDetail") {
      const identity = this.top().savedIdentity;
      return allSaved.find((workflow) => sameIdentity(identity, savedIdentity(workflow)));
    }
    return undefined;
  }
  beginFilter(): void {
    if (this.kind !== "runs") return;
    this.cancelConfirmation();
    this.mode = "filter";
    this.draft = this.filter;
    this.filterSelection = this.top().selected;
  }
  /** Query used to render the live draft while filter text is being edited. */
  effectiveFilter(): string {
    return this.mode === "filter" ? this.draft : this.filter;
  }
  beginRename(workflow: SavedWorkflow): void {
    this.cancelConfirmation();
    this.mode = "rename";
    this.draft = workflow.name;
    this.renameTarget = savedIdentity(workflow);
  }
  /** `text` is already decoded from terminal input (see navigator/input-text.ts). */
  appendInput(text: string): void {
    if (this.mode === "filter" || this.mode === "rename") this.draft += text;
  }
  backspaceInput(): void {
    if (this.mode === "filter" || this.mode === "rename") this.draft = dropLastGrapheme(this.draft);
  }
  applyFilter(model: NavigatorModel): NavigatorSnapshot {
    this.filter = this.draft;
    this.mode = "browse";
    this.draft = "";
    const snapshot = model.visible(this.filter);
    this.reconcile(snapshot);
    this.filterSelection = undefined;
    return snapshot;
  }
  takeRename(): { target: SavedItemIdentity; name: string } | undefined {
    if (this.mode !== "rename" || !this.renameTarget) return undefined;
    const result = { target: this.renameTarget, name: this.draft };
    this.mode = "browse";
    this.draft = "";
    this.renameTarget = undefined;
    return result;
  }
  replaceSavedIdentity(previous: SavedItemIdentity, workflow: SavedWorkflow): void {
    const next = savedIdentity(workflow);
    for (const frame of this.stack) {
      if (frame.selected && sameIdentity(frame.selected, previous)) frame.selected = next;
      if (frame.savedIdentity && sameIdentity(frame.savedIdentity, previous)) {
        frame.savedIdentity = next;
        frame.savedName = workflow.name;
      }
    }
  }

  cancelInput(): void {
    // Canceling a filter edit must never discard the last query that was
    // applied. Restore the identity selected when editing began as well; the
    // draft may briefly have filtered it out while the user was typing.
    if (this.mode === "filter" && this.filterSelection) this.top().selected = this.filterSelection;
    this.mode = "browse";
    this.draft = "";
    this.filterSelection = undefined;
    this.renameTarget = undefined;
  }
  private confirmationContext(): ConfirmationContext {
    const frame = this.top();
    return {
      kind: frame.kind,
      runId: frame.runId,
      phase: frame.phase,
      agentId: frame.agentId,
      savedIdentity: frame.savedIdentity,
    };
  }
  beginConfirmation(request: ConfirmRequest, snapshot: NavigatorSnapshot): boolean {
    if (this.mode !== "browse") return false;
    this.pending = {
      request,
      cursor: this.cursor,
      filter: snapshot.filter,
      context: this.confirmationContext(),
    };
    this.mode = "confirm";
    return true;
  }
  confirm(action: ConfirmKind, snapshot: NavigatorSnapshot): ConfirmRequest | undefined {
    const pending = this.pending;
    this.pending = undefined;
    this.mode = "browse";
    if (
      !pending ||
      pending.request.action !== action ||
      pending.cursor !== this.cursor ||
      pending.filter !== snapshot.filter ||
      !sameConfirmationContext(this.confirmationContext(), pending.context)
    )
      return undefined;
    const { request } = pending;
    if (this.kind === "savedDetail") {
      return sameIdentity(this.top().savedIdentity, request.target) ? request : undefined;
    }
    if (this.kind === "runs") {
      return sameIdentity(this.selectedItem(snapshot)?.identity, request.target) ? request : undefined;
    }
    // Drilled views are bound to the immutable frame runId, not runs selection.
    return request.target.kind === "run" && request.target.runId === this.runId ? request : undefined;
  }
  cancelConfirmation(): void {
    if (this.mode === "confirm") this.mode = "browse";
    this.pending = undefined;
  }
}

const PLAIN: ThemeLike = { fg: (_c, t) => t, bold: (t) => t };

/** Build the lines for the current view. Depends only on state, model, and theme, so it renders without a terminal. */
export function renderNavigator(
  state: NavigatorState,
  model: NavigatorModel,
  width: number,
  theme: ThemeLike = PLAIN,
  viewportRows = 24,
  markdownTheme?: MarkdownTheme,
): string[] {
  return model.withRenderFrame(() =>
    renderNavigatorFrame(state, model, width, theme, viewportRows, markdownTheme, undefined),
  );
}

/** What a key press should do. */
export type NavAction =
  | { type: "move"; delta: number }
  | { type: "page"; direction: -1 | 1 }
  | { type: "jump"; edge: "start" | "end" }
  | { type: "toggleTail" }
  | { type: "togglePager" }
  | { type: "openPager" }
  | { type: "drill" }
  | { type: "back" }
  | { type: "close" }
  | { type: "pause" }
  | { type: "stop" }
  | { type: "restart" }
  | { type: "save" }
  | { type: "deleteSaved" }
  | { type: "filter" }
  | { type: "rename" }
  | { type: "none" };

/** Pure mapping from a parsed key id to an action; a key's meaning depends on the view and the selected item's kind. */
export function keyToAction(keyId: string | undefined, kind: ViewKind, itemKind?: ItemKind): NavAction {
  switch (keyId) {
    case "up":
    case "k":
      return { type: "move", delta: -1 };
    case "down":
    case "j":
      return { type: "move", delta: 1 };
    case "pageUp":
    case "ctrl+u":
    case "ctrl+b":
      return { type: "page", direction: -1 };
    case "pageDown":
    case "ctrl+d":
    case "ctrl+f":
      return { type: "page", direction: 1 };
    case "space":
      return kind === "detail" || kind === "savedDetail" ? { type: "page", direction: 1 } : { type: "none" };
    case "home":
    case "g":
      return { type: "jump", edge: "start" };
    case "end":
    case "G":
    case "shift+g":
      return { type: "jump", edge: "end" };
    case "t":
      return kind === "detail" ? { type: "toggleTail" } : { type: "none" };
    case "enter":
    case "return":
      if (kind === "detail") return { type: "togglePager" };
      if (kind === "savedDetail") return { type: "none" };
      return { type: "drill" };
    case "right":
      if (kind === "detail") return { type: "openPager" };
      if (kind === "savedDetail") return { type: "none" };
      return { type: "drill" };
    case "escape":
    case "esc":
    case "left":
      return { type: "back" };
    case "q":
      return { type: "close" };
    case "p":
      return { type: "pause" };
    case "x":
      if (kind === "savedDetail" || itemKind === "saved") return { type: "deleteSaved" };
      return { type: "stop" };
    case "/":
      return kind === "runs" ? { type: "filter" } : { type: "none" };
    case "r":
      return kind === "savedDetail" || itemKind === "saved" ? { type: "rename" } : { type: "restart" };
    case "s":
      if (itemKind === "saved") return { type: "none" };
      return { type: "save" };
    default:
      return { type: "none" };
  }
}

export interface NavigatorOptions {
  /** The storage in effect now; the extension can replace it after session_start. */
  getStorage?: () => WorkflowStorage | undefined;
  /**
   * The manager a saved `/name` command registered from the navigator runs on,
   * so it uses the current session's manager rather than this snapshot.
   */
  getManager?: () => WorkflowManager;
}

/**
 * Open the interactive `/workflows` navigator as a focused overlay. Resolves when
 * the user closes it (esc at the top level, or `q`).
 */
export function openWorkflowNavigator(
  pi: ExtensionAPI,
  manager: WorkflowManager,
  ui: ExtensionUIContext,
  opts: NavigatorOptions = {},
): Promise<void> {
  const model = new NavigatorModel(manager, () => opts.getStorage?.());
  const state = new NavigatorState();
  const session: NavigatorSession = { pi, manager, ui, options: opts, model, state };

  return ui.custom<void>(
    (tui: TUI, theme: Theme, _keybindings, done: (r: undefined) => void) => {
      const rerender = () => tui.requestRender();
      const markdownTheme = getMarkdownTheme();
      const renderCache = new NavigatorTextRenderCache();
      const unsubscribe = subscribeToRunUpdates(manager, state, rerender);
      const close = () => {
        unsubscribe();
        done(undefined);
      };

      // Route each key by input mode; every handled key ends in one redraw.
      const act = (data: string) => {
        const key = parseKey(data);
        try {
          if (state.mode === "filter" || state.mode === "rename") handleTextInput(session, key, data);
          else if (state.mode === "confirm") handleConfirmKey(session, key);
          else {
            // The key means what it meant for the row the user saw, even if that row has since gone.
            if (
              !runBrowseAction(
                session,
                keyToAction(key, state.kind, state.selectedKind(currentSnapshot(session))),
                close,
              )
            )
              return;
          }
        } catch (error) {
          ui.notify(`Workflow action failed: ${errorMessage(error)}`, "error");
        }
        rerender();
      };

      let focused = false;
      const component: Component & Focusable & { dispose?(): void } = {
        get focused(): boolean {
          return focused;
        },
        set focused(v: boolean) {
          focused = v;
        },
        render: (width: number) => {
          const layout = overlayLayout(width, tui.terminal?.rows ?? 24);
          const lines = model.withRenderFrame(() =>
            renderNavigatorFrame(
              state,
              model,
              layout.contentWidth,
              theme,
              layout.contentRows,
              markdownTheme,
              renderCache,
            ),
          );
          return boxOverlayLines(lines, layout, theme, focused);
        },
        handleInput: (data: string) => act(data),
        // The host invalidates on theme changes; cached rich text holds the old colors.
        invalidate: () => renderCache.clear(),
        dispose: () => unsubscribe(),
      };
      return component;
    },
    { overlay: true, overlayOptions: overlayOptions() },
  );
}
