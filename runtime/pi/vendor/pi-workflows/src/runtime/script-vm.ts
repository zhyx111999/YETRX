/** Builds the vm context that exposes exactly the declared workflow globals and runs a script body in it. */

import vm from "node:vm";
import type { WorkflowLogger } from "../logger.js";
import { WORKFLOW_CAPABILITY_CONTRACT } from "../workflow-capability-contract.js";
import { DETERMINISM_PRELUDE } from "./determinism.js";
import { LOOP_GUARD_NAME } from "./loop-guard.js";

/** Its own script, so it adds no lines in front of the body. */
const PRELUDE = new vm.Script(DETERMINISM_PRELUDE, { filename: "workflow-prelude.js" });

/** Bind the runtime implementations to the global names the capability contract declares, in a deterministic realm. */
export function createScriptContext(implementations: Readonly<Record<string, unknown>>, logger: WorkflowLogger) {
  const { globals, diagnostics } = WORKFLOW_CAPABILITY_CONTRACT.assembleRuntimeBindings(implementations);
  for (const diagnostic of diagnostics) logger.warn(diagnostic.message);
  // Built-ins (Object, Array, JSON, Math, Date, Promise...) come from the vm
  // realm itself: a host built-in's .constructor would be the host Function,
  // which would bypass the determinism prelude.
  const context = vm.createContext({ ...globals });
  PRELUDE.runInContext(context);
  return context;
}

/** The Promise.prototype of the script's own realm, which every promise the script makes itself inherits from. */
export function realmPromisePrototype(context: vm.Context): object {
  return PROMISE_PROTOTYPE.runInContext(context) as object;
}

const PROMISE_PROTOTYPE = new vm.Script("Promise.prototype");

/**
 * Compile and start the script body as an async function. A syntax error throws
 * synchronously; the returned promise settles with the script's return value.
 *
 * There is deliberately no vm timeout: it would bound only the code before the
 * first await, and bounding the later continuations (microtaskMode
 * "afterEvaluate") terminates code inside a microtask, which aborts the whole
 * Node process whenever async hooks are active (AsyncLocalStorage on Node 22,
 * node:test, APM agents). A runaway loop is bounded by the loop guard instead.
 */
export function runScript(
  context: vm.Context,
  body: string,
  filename: string,
  loopGuard: () => void,
): Promise<unknown> {
  // The body starts one line below the wrapper header, so stack traces show the
  // author's line numbers. The loop guard is a parameter, not a global.
  const wrapped = `"use strict";(${LOOP_GUARD_NAME}) => (async () => {\n${body}\n})()`;
  const start = new vm.Script(wrapped, { filename, lineOffset: -1 }).runInContext(context) as (
    guard: () => void,
  ) => Promise<unknown>;
  return start(loopGuard);
}
