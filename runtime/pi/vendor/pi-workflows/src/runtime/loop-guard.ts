/**
 * Keeps a workflow script from holding Pi's thread forever. The script runs in
 * a vm context on Pi's own thread, so a loop that never awaits would freeze Pi,
 * and a stop request could never reach it. Every loop body therefore starts
 * with a call to a guard the run provides: it ends the script once the run is
 * stopped, and once the script has kept the thread for longer than a budget
 * without letting the event loop turn.
 *
 * What it does not bound: one long builtin call (a huge sort, a catastrophic
 * regular expression), and deep recursion, which ends in a RangeError instead.
 */

import type { AnyNode, Program } from "acorn";
import { WorkflowError, WorkflowErrorCode } from "../errors.js";

/**
 * The name the guard is bound to: a parameter of the function the script body
 * runs in, so it is no global the script can see in globalThis. The name is
 * reserved, so a script cannot shadow or replace it.
 */
export const LOOP_GUARD_NAME = "__workflowLoopGuard";

/** How long the script may keep the thread without yielding before it is ended. */
const SYNCHRONOUS_BUDGET_MS = 5_000;

/** How often the event loop records that it turned while a run is active. */
const HEARTBEAT_MS = 100;

const LOOPS = new Set(["WhileStatement", "DoWhileStatement", "ForStatement", "ForInStatement", "ForOfStatement"]);

/**
 * `script` with a guard call at the start of every loop body, from the tree
 * the script was parsed into. Insertions add no line breaks, so stack traces
 * keep the author's line numbers, and every one lies after the meta statement.
 */
export function guardLoops(script: string, program: Program): string {
  const insertions: Array<{ at: number; text: string }> = [];
  const call = `${LOOP_GUARD_NAME}();`;
  const pending: AnyNode[] = [program];
  while (pending.length > 0) {
    const node = pending.pop() as AnyNode;
    if (node.type === "Identifier" && node.name === LOOP_GUARD_NAME) {
      throw new WorkflowError(
        `${LOOP_GUARD_NAME} is reserved by the workflow runtime (line ${node.loc?.start.line})`,
        WorkflowErrorCode.SCRIPT_VALIDATION_ERROR,
        { recoverable: false },
      );
    }
    if (LOOPS.has(node.type)) {
      const body = (node as { body: AnyNode }).body;
      if (body.type === "BlockStatement") insertions.push({ at: body.start + 1, text: call });
      else insertions.push({ at: body.start, text: `{${call}` }, { at: body.end, text: "}" });
    }
    for (const value of Object.values(node)) {
      for (const child of Array.isArray(value) ? value : [value]) {
        if (typeof (child as AnyNode | null)?.type === "string") pending.push(child as AnyNode);
      }
    }
  }
  let out = script;
  for (const { at, text } of insertions.sort((a, b) => b.at - a.at)) out = out.slice(0, at) + text + out.slice(at);
  return out;
}

/** The guard of one run, and the heartbeat it measures against. */
export interface LoopGuard {
  /** Called at the start of every loop iteration in the script. */
  readonly check: () => void;
  /** The error that ended a runaway script, once the guard has tripped. */
  readonly tripped: () => WorkflowError | undefined;
  /** Stops the heartbeat; the run has ended. */
  readonly dispose: () => void;
}

/**
 * A guard for one run. `throwIfStopped` ends the script when the run is
 * stopped, paused or failed. Once the budget trips, every later loop turn
 * throws again, so a script cannot catch its way back into the loop.
 */
export function createLoopGuard(throwIfStopped: () => void, budgetMs = SYNCHRONOUS_BUDGET_MS): LoopGuard {
  let lastTurn = Date.now();
  const heartbeat = setInterval(() => {
    lastTurn = Date.now();
  }, HEARTBEAT_MS);
  heartbeat.unref?.();
  let runaway: WorkflowError | undefined;
  return {
    check: () => {
      if (runaway) throw runaway;
      throwIfStopped();
      if (Date.now() - lastTurn <= budgetMs) return;
      runaway = new WorkflowError(
        `The workflow script ran for over ${Math.round(budgetMs / 1000)}s without awaiting anything, which blocks Pi. Await work inside long loops, or move heavy computation into an agent.`,
        WorkflowErrorCode.SCRIPT_VALIDATION_ERROR,
        { recoverable: false },
      );
      throw runaway;
    },
    tripped: () => runaway,
    dispose: () => clearInterval(heartbeat),
  };
}
