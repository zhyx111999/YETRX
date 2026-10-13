/**
 * Finds the host AgentSession this extension instance is bound to and returns a
 * confirmed send for it. pi.sendMessage itself cannot carry results: it is
 * fire-and-forget and swallows rejections.
 *
 * The only route to a session is this extension's own pi.sendMessage, which Pi
 * forwards to the session whose runner loaded the extension. A session that
 * never loaded it (a subagent child, a session started after the extension was
 * disabled, one an SDK caller built) is never reached, so it is never captured.
 * The host prototype is wrapped only while that synchronous probe runs and is
 * restored before the probe returns: nothing stays installed between probes,
 * across /reload, or after the extension is unloaded, and extension generations
 * or module instances cannot stack wrappers.
 */

import { AgentSession, type ExtensionAPI } from "@earendil-works/pi-coding-agent";
import { confirmedSessionSend, type HostSession } from "./confirmed-send.js";
import type { DeliverySend } from "./delivery-endpoints.js";

/** Custom type of the capture-only probe. The wrapper swallows it: nothing is appended, persisted, or turned. */
export const DELIVERY_PROBE_CUSTOM_TYPE = "workflow-delivery-probe";

/** A class whose prototype carries the host sessions' sendCustomMessage. */
export interface HostSessionClass {
  readonly prototype: object;
}

let hostSessionClass: HostSessionClass = AgentSession;

/**
 * Probe through another session class. The default is the AgentSession of the
 * running Pi (host-modules.ts routes this module's import to it); tests
 * substitute a session double.
 */
export function _useHostSessionClassForTests(sessionClass: HostSessionClass): void {
  hostSessionClass = sessionClass;
}

/**
 * The confirmed send of the session `pi` is bound to, or undefined when `pi` is
 * stale or not yet bound, the host shape changed, or the bound session is not
 * `sessionId`. Callers stay fail-closed on undefined.
 */
export function captureSessionSend(pi: ExtensionAPI, sessionId: string): DeliverySend | undefined {
  const session = probeBoundSession(pi);
  if (!session) return undefined;
  try {
    if (session.sessionManager?.getSessionId?.() !== sessionId) return undefined;
  } catch {
    return undefined;
  }
  return confirmedSessionSend(session, sessionId);
}

function probeBoundSession(pi: ExtensionAPI): HostSession | undefined {
  const prototype = hostSessionClass.prototype as { sendCustomMessage?: unknown };
  const original = prototype.sendCustomMessage;
  // Without a method to wrap, the probe would reach a real send and be persisted.
  if (typeof original !== "function") return undefined;
  const ownDescriptor = Object.getOwnPropertyDescriptor(prototype, "sendCustomMessage");

  let probing = true;
  let bound: HostSession | undefined;
  const intercept = function interceptDeliveryProbe(this: HostSession, ...args: unknown[]) {
    const message = args[0] as { customType?: unknown } | undefined;
    if (probing && message?.customType === DELIVERY_PROBE_CUSTOM_TYPE) {
      bound = this;
      return Promise.resolve();
    }
    return original.apply(this, args);
  };
  try {
    Object.defineProperty(prototype, "sendCustomMessage", { value: intercept, writable: true, configurable: true });
  } catch {
    return undefined;
  }

  try {
    // Pi forwards this synchronously to the bound session's sendCustomMessage.
    pi.sendMessage({ customType: DELIVERY_PROBE_CUSTOM_TYPE, content: "", display: false }, { triggerTurn: false });
  } catch {
    // A stale or not yet bound pi throws; nothing is captured.
  } finally {
    probing = false;
    // Restore only our own layer. A wrapper stacked on top during the probe
    // keeps working, and ours inside it is now a plain pass-through.
    if (prototype.sendCustomMessage === intercept) {
      if (ownDescriptor) Object.defineProperty(prototype, "sendCustomMessage", ownDescriptor);
      else delete prototype.sendCustomMessage;
    }
  }
  return bound;
}

/** @internal test helper — probe the SDK class again. */
export function _resetHostSessionCaptureForTests(): void {
  hostSessionClass = AgentSession;
}
