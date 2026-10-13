/**
 * Background-run UX, mirroring Claude Code:
 *  - A live task panel below the input lists in-progress runs while you keep
 *    working. It is informational; /workflows opens the full navigator.
 *  - When a background run finishes, fails, or pauses for the model, the result
 *    is delivered back into the conversation that started it, so the paused task
 *    continues with the outcome.
 *
 * Delivery is routed per session because Pi's ExtensionRunner.bindCore() stores
 * `runtime.sendMessage` on a runtime object shared by every session: the session
 * constructed last wins, so pi.sendMessage at completion time would land in
 * whichever session was built last. Instead each session binds an endpoint that
 * holds its own host AgentSession.sendCustomMessage, which returns a real
 * Promise (actions.sendMessage is void and swallows rejections). A result is
 * saved as a pending marker first, sent only through its run's session
 * endpoint, and cleared only after that send is confirmed. A missing or
 * suspended endpoint leaves it pending; nothing falls back to pi.sendMessage.
 *
 * The internals live in ./task-panel/: panel rendering, session endpoints, host
 * session capture, confirmed sends, pending markers, retries, and dispatch.
 */

import type { ExtensionAPI, ExtensionUIContext } from "@earendil-works/pi-coding-agent";
import { fmtCompact, fmtCost, fmtTokenSegment, tokenFigures } from "./display.js";
import type { PendingDeliveryMarker, PersistedRunState } from "./run-persistence.js";
import { _resetConfirmedSendsForTests, cancelUnsentWaiters } from "./task-panel/confirmed-send.js";
import {
  _resetDeliveryDispatchForTests,
  flushSessionDiskPending,
  routeBackgroundDelivery,
  setPendingTextRenderer,
} from "./task-panel/delivery-dispatch.js";
import {
  type DeliverySend,
  resolveDeliverySessionId,
  type SessionDeliveryEndpoint,
  sessionEndpoints,
} from "./task-panel/delivery-endpoints.js";
import { _resetDeliveryRetriesForTests, cancelSessionRetries } from "./task-panel/delivery-retry.js";
import { _resetHostSessionCaptureForTests, captureSessionSend } from "./task-panel/host-session-capture.js";
import { panelWidget } from "./task-panel/panel-widget.js";
import { deliveredAwaitingClear, forgetAwaitingClear } from "./task-panel/pending-markers.js";
import {
  deliveredMaxChars,
  failedRunText,
  type PausedEvent,
  pausedRunText,
  persistedResultPath,
  summarizeResult,
} from "./task-panel/result-text.js";
import type { ManagedRun, WorkflowManager } from "./workflow-manager.js";
import type { WorkflowSettings } from "./workflow-settings.js";

// ─── Live panel ────────────────────────────────────────────────────────────────

/**
 * Install the live "workflows running" panel below the editor. It redraws on
 * every manager event and is informational only — the user opens the navigator
 * with /workflows. `loadSettings` is read fresh (with a short TTL cache) on each
 * render, so `/workflows-progress` takes effect without a restart; without it
 * the panel is always compact.
 */
export function installTaskPanel(
  manager: WorkflowManager,
  ui: ExtensionUIContext,
  loadSettings?: () => WorkflowSettings,
): void {
  ui.setWidget("workflow-tasks", panelWidget(manager, loadSettings), { placement: "belowEditor" });
}

// ─── Delivered messages ────────────────────────────────────────────────────────

/** The message delivered when a background run completes, ending with a pointer to the full result. */
export function deliverText(run: ManagedRun, opts: { resultPath?: string; maxChars?: number } = {}): string {
  const summary = summarizeResult(run.result?.result, opts.maxChars);
  const tu = run.result?.tokenUsage;
  const cost = tu?.cost ? ` · ${fmtCost(tu.cost)}` : "";
  const segment = fmtTokenSegment(tokenFigures(tu), fmtCompact);
  const tokens = `${segment ? ` · ${segment}` : ""}${cost}`;
  const agents = run.result?.agentCount ?? run.snapshot.agentCount;
  const duration = run.result?.durationMs ? ` · ${(run.result.durationMs / 1000).toFixed(1)}s` : "";
  const lines = [
    `✓ Background workflow "${run.snapshot.name}" finished (${agents} agents${tokens}${duration}).`,
    "",
    summary,
  ];
  // Always point at the full persisted result so the tail is never lost — even when
  // the summary above is a complete verdict/summary field or an untruncated dump.
  if (opts.resultPath) lines.push("", `↳ Full result: ${opts.resultPath}`);
  return lines.join("\n");
}

/** Message for a pending marker; a complete result is rebuilt from the live run or its record. */
function pendingResultText(
  manager: WorkflowManager,
  runId: string,
  marker: PendingDeliveryMarker,
  loadSettings?: () => WorkflowSettings,
  run?: ManagedRun,
  persisted?: PersistedRunState,
): string | undefined {
  if (marker.kind === "text") return marker.text;
  if (run) {
    return deliverText(run, {
      resultPath: persistedResultPath(manager, runId, run.result?.result),
      maxChars: deliveredMaxChars(loadSettings),
    });
  }
  if (persisted) {
    return deliverText(
      {
        snapshot: { name: persisted.workflowName, agentCount: persisted.agents?.length ?? 0 },
        result: {
          result: persisted.result,
          tokenUsage: persisted.tokenUsage,
          agentCount: persisted.agents?.length ?? 0,
          durationMs: persisted.durationMs,
        },
      } as ManagedRun,
      {
        resultPath: persistedResultPath(manager, runId, persisted.result),
        maxChars: deliveredMaxChars(loadSettings),
      },
    );
  }
  return undefined;
}

setPendingTextRenderer(pendingResultText);

// ─── Session endpoints ─────────────────────────────────────────────────────────

/**
 * Register or refresh the delivery endpoint for a pi session, then flush what is
 * pending for that session only. Requires a confirming send: the host session
 * `pi` is bound to, captured afresh on every bind, or `stableSend`. A durable
 * appendCustomMessageEntry is not an ACK because it starts no turn.
 *
 * Call from session_start, after Pi's bindCore.
 */
export function bindSessionDelivery(
  sessionId: string,
  pi: ExtensionAPI,
  opts: {
    loadSettings?: () => WorkflowSettings;
    /** UI-safe, rate-limited warning for actionable delivery failures only. */
    reportWarning?: (message: string) => void;
    manager?: WorkflowManager;
    /** A confirming send already in hand (captured, or a test double); skips the capture. */
    stableSend?: DeliverySend;
    /** Used only to check the session identity; its append is not an ACK channel. */
    sessionManager?: {
      getSessionId?: () => string;
      appendCustomMessageEntry?: (
        customType: string,
        content: string | unknown[],
        display: boolean,
        details?: unknown,
      ) => string;
    };
  } = {},
): void {
  if (!sessionId) return;

  // Refuse to bind when the session manager reports a different session.
  try {
    const liveId = opts.sessionManager?.getSessionId?.();
    if (liveId && liveId !== sessionId) return;
  } catch {
    // getSessionId unavailable — continue
  }

  const send = opts.stableSend ?? captureSessionSend(pi, sessionId);
  const prev = sessionEndpoints.get(sessionId);
  // Already-started sends keep their lock until they settle: a reload is not an
  // SDK abort barrier.
  cancelUnsentWaiters(sessionId, "session delivery rebound before send");
  const endpoint: SessionDeliveryEndpoint = {
    sessionId,
    send,
    loadSettings: opts.loadSettings ?? prev?.loadSettings,
    suspended: false,
    generation: (prev?.generation ?? 0) + 1,
    manager: opts.manager ?? prev?.manager,
    reportWarning: opts.reportWarning ?? prev?.reportWarning,
  };
  sessionEndpoints.set(sessionId, endpoint);

  if (endpoint.manager) flushSessionDiskPending(endpoint.manager, sessionId, endpoint);
}

/**
 * Suspend delivery for a session. Completions only mark disk pending until
 * {@link bindSessionDelivery} runs again.
 */
export function suspendSessionDelivery(sessionId: string | undefined): void {
  if (!sessionId) return;
  const endpoint = sessionEndpoints.get(sessionId);
  if (endpoint) endpoint.suspended = true;
  cancelUnsentWaiters(sessionId, "session delivery suspended");
  cancelSessionRetries(sessionId);
}

/**
 * Forget a session that will not come back (quit / discard, a stranded handoff,
 * or the old id after a successful replacement bind). Dropping its endpoint
 * releases the host session its captured send holds; pending markers stay on
 * disk.
 */
export function dropSessionDelivery(sessionId: string | undefined): void {
  if (!sessionId) return;
  suspendSessionDelivery(sessionId);
  forgetAwaitingClear(sessionId);
  sessionEndpoints.delete(sessionId);
}

/**
 * Stop live sends for the manager's currently bound session. In-flight
 * completions only leave disk pending until the next bind/resume.
 *
 * Call from session_shutdown before handoff or discard, so a completion racing
 * the teardown cannot deliver into the outgoing session.
 */
export function suspendResultDelivery(manager: WorkflowManager): void {
  suspendSessionDelivery(manager.getSessionId());
}

// ─── Result delivery ───────────────────────────────────────────────────────────

/**
 * Managers whose delivery listeners are installed, with the last loadSettings
 * seen on install (used when binding endpoints).
 */
let deliveryInstalls = new WeakMap<WorkflowManager, { loadSettings?: () => WorkflowSettings }>();

// turn_end is registered once per ExtensionAPI instance (pi.on has no off()), and
// each handler looks up that instance's latest manager: several live pi
// instances can interleave installs, and a cross-project rebuild must not stack
// another handler.
let turnEndDeliveryManagers = new WeakMap<ExtensionAPI, { manager: WorkflowManager }>();

/**
 * When a background run finishes, fails, or pauses for the model, deliver it
 * into the originating conversation and continue the turn so the assistant can
 * act on it, without blocking the user meanwhile:
 *
 *  - Delivery goes to the session the run's record names as its owner.
 *  - `triggerTurn: true` starts a fresh turn when the agent is idle.
 *  - `deliverAs: "followUp"` queues behind an in-flight turn — never interrupts.
 *  - The durable pending marker clears only after the send is confirmed.
 *
 * One set of listeners per manager: installing again, from this or a reloaded
 * copy of the module, replaces the previous set. Across session replacement the
 * manager survives via the handoff path; each new session calls
 * {@link bindSessionDelivery} on session_start.
 */
export function installResultDelivery(
  pi: ExtensionAPI,
  manager: WorkflowManager,
  opts: { loadSettings?: () => WorkflowSettings } = {},
): void {
  const install = deliveryInstalls.get(manager) ?? {};
  install.loadSettings = opts.loadSettings;
  deliveryInstalls.set(manager, install);

  // After each turn, bind the session if its send was not captured yet, then
  // flush whatever is still pending for it.
  const existingTurnEnd = turnEndDeliveryManagers.get(pi);
  if (existingTurnEnd) {
    existingTurnEnd.manager = manager;
  } else {
    turnEndDeliveryManagers.set(pi, { manager });
    pi.on?.("turn_end", (_event: unknown, ctx?: { sessionManager?: { getSessionId?: () => string } }) => {
      const activeManager = turnEndDeliveryManagers.get(pi)?.manager;
      if (!activeManager) return;
      let sid: string | undefined;
      try {
        sid = ctx?.sessionManager?.getSessionId?.() ?? activeManager.getSessionId();
      } catch {
        sid = activeManager.getSessionId();
      }
      if (!sid) return;

      const endpoint = sessionEndpoints.get(sid);
      if (!endpoint || typeof endpoint.send !== "function") {
        // Bind only once a send is captured; a failed capture leaves the
        // session as it was for the next turn to retry. Binding flushes.
        const send = captureSessionSend(pi, sid);
        if (send) {
          bindSessionDelivery(sid, pi, {
            loadSettings: deliveryInstalls.get(activeManager)?.loadSettings,
            manager: activeManager,
            sessionManager: ctx?.sessionManager,
            stableSend: send,
          });
        }
        return;
      }
      if (!endpoint.suspended && endpoint.manager) flushSessionDiskPending(endpoint.manager, sid, endpoint);
    });
  }

  // A newly-created manager can replace the current project while this pi and
  // session endpoint remain live. Refresh only the routing pointers; preserve
  // the session transport, generation, and suspension state until bindCore.
  const sid = manager.getSessionId();
  if (sid) {
    const endpoint = sessionEndpoints.get(sid);
    if (endpoint) {
      endpoint.loadSettings = opts.loadSettings ?? endpoint.loadSettings;
      endpoint.manager = manager;
    }
  }

  replaceDeliveryListeners(manager, [
    [
      "complete",
      ({ runId }: { runId: string }) => {
        const run = manager.getRun(runId);
        if (!run) return;
        const sessionId = resolveDeliverySessionId(run, manager);
        const endpoint = sessionId ? sessionEndpoints.get(sessionId) : undefined;
        const content = deliverText(run, {
          resultPath: persistedResultPath(manager, runId, run.result?.result),
          maxChars: deliveredMaxChars(endpoint?.loadSettings ?? install.loadSettings),
        });
        routeBackgroundDelivery(manager, run, { kind: "complete" }, content);
      },
    ],
    [
      "error",
      ({ runId, error }: { runId: string; error?: { message?: string } }) => {
        const run = manager.getRun(runId);
        if (!run) return;
        const text = failedRunText(runId, error);
        routeBackgroundDelivery(manager, run, { kind: "text", text }, text);
      },
    ],
    // Two pauses need the model: a provider usage limit (the run is resumable
    // once the limit resets, not dead) and a script waiting at a durable
    // checkpoint (it stays paused until someone answers). A manual pause()
    // carries no reason and is the user's own action, so it delivers nothing.
    [
      "paused",
      (event: PausedEvent) => {
        const run = manager.getRun(event.runId);
        if (!run) return;
        const text = pausedRunText(event);
        if (text) routeBackgroundDelivery(manager, run, { kind: "text", text }, text);
      },
    ],
  ]);
}

/**
 * Where a manager keeps its delivery listeners, under a key every loaded copy of
 * this module shares. A host that reloads the module graph hands the same
 * manager to the new copy, whose install then replaces the old copy's listeners
 * instead of stacking a second set that would act on stale module state.
 */
const DELIVERY_LISTENERS = Symbol.for("pi-workflows-lean.result-delivery-listeners");
type DeliveryListener = [event: string, listener: (...args: any[]) => void];

function replaceDeliveryListeners(manager: WorkflowManager, listeners: DeliveryListener[]): void {
  const owner = manager as unknown as { [DELIVERY_LISTENERS]?: DeliveryListener[] };
  for (const [event, listener] of owner[DELIVERY_LISTENERS] ?? []) manager.off(event, listener);
  for (const [event, listener] of listeners) manager.on(event, listener);
  owner[DELIVERY_LISTENERS] = listeners;
}

// ─── Test helpers ──────────────────────────────────────────────────────────────

/** @internal test helper — reset process-wide delivery registries between cases. */
export function _resetDeliveryRegistriesForTests(): void {
  _resetConfirmedSendsForTests();
  _resetHostSessionCaptureForTests();
  _resetDeliveryDispatchForTests();
  _resetDeliveryRetriesForTests();
  deliveredAwaitingClear.clear();
  sessionEndpoints.clear();
  turnEndDeliveryManagers = new WeakMap<ExtensionAPI, { manager: WorkflowManager }>();
  deliveryInstalls = new WeakMap<WorkflowManager, { loadSettings?: () => WorkflowSettings }>();
}

/** @internal test helper — inspect a session's endpoint state. */
export function _getSessionDeliveryEndpointForTests(
  sessionId: string,
): { suspended: boolean; generation: number; hasSend: boolean } | undefined {
  const ep = sessionEndpoints.get(sessionId);
  if (!ep) return undefined;
  return {
    suspended: ep.suspended,
    generation: ep.generation,
    hasSend: typeof ep.send === "function",
  };
}
