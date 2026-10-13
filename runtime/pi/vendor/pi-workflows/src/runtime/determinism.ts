/** Keeps workflow scripts replayable by rejecting and disabling nondeterministic builtins. */

import type { AnyNode, Program } from "acorn";
import { WorkflowError, WorkflowErrorCode } from "../errors.js";

/**
 * Parse-time feedback for authors: reject direct calls that read the clock or
 * the random source. Only real call and `new` expressions count, matched by the
 * builtin's name, so prose that mentions an API in a string or comment is fine.
 * Aliased or computed access is left to DETERMINISM_PRELUDE, the actual
 * enforcement. `program` must be parsed with `locations: true`.
 */
export function assertDeterministicProgram(program: Program): void {
  const found: Array<{ call: string; node: AnyNode }> = [];
  const pending: AnyNode[] = [program];
  while (pending.length > 0) {
    const node = pending.pop() as AnyNode;
    const call = nondeterministicCall(node);
    if (call) found.push({ call, node });
    for (const value of Object.values(node)) {
      for (const child of Array.isArray(value) ? value : [value]) {
        if (isNode(child)) pending.push(child);
      }
    }
  }
  if (found.length === 0) return;

  const calls = found
    .sort((a, b) => a.node.start - b.node.start)
    .map(({ call, node }) => `${call} (line ${node.loc?.start.line})`);
  throw new WorkflowError(
    `Workflow scripts must be deterministic: ${calls.join(", ")} ${calls.length === 1 ? "is" : "are"} unavailable; pass timestamps and randomness through args`,
    WorkflowErrorCode.SCRIPT_VALIDATION_ERROR,
    { recoverable: false },
  );
}

/** AST children are the property values that carry a node `type`; locations and literal values do not. */
function isNode(value: unknown): value is AnyNode {
  return typeof (value as AnyNode | null)?.type === "string";
}

/** The blocked call `node` makes, named as authors write it, or undefined. */
function nondeterministicCall(node: AnyNode): string | undefined {
  if (node.type === "NewExpression") {
    return isIdentifier(node.callee, "Date") && node.arguments.length === 0 ? "new Date()" : undefined;
  }
  if (node.type !== "CallExpression") return undefined;
  const callee = node.callee;
  if (isIdentifier(callee, "Date")) return "Date()";
  if (callee.type !== "MemberExpression" || callee.computed || callee.property.type !== "Identifier") return undefined;
  if (isIdentifier(callee.object, "Date") && callee.property.name === "now") return "Date.now()";
  if (isIdentifier(callee.object, "Math") && callee.property.name === "random") return "Math.random()";
  return undefined;
}

function isIdentifier(node: AnyNode, name: string): boolean {
  return node.type === "Identifier" && node.name === name;
}

/**
 * Runs inside the vm realm before the script body. Resume replays a script and
 * matches calls against the journal, so values that differ between runs would
 * break replay. Every way the realm offers to read the clock or the random
 * source throws instead:
 *   - Math.random()
 *   - Date.now(), Date() and no-argument new Date(), including through a Date
 *     instance's `constructor`; new Date(arg), Date.UTC and Date.parse still work
 *   - Intl.DateTimeFormat format()/formatToParts() without a date, which format
 *     the current time; formatRange already requires both dates
 * It uses the realm's own Math/Date/Intl/Reflect, so it hands the script no
 * host `Function`. vm is not a security sandbox (an injected bridge function's
 * `.constructor` is still the host Function), so this guards against accidental
 * nondeterminism in trusted scripts, not against a deliberate bypass.
 */
export const DETERMINISM_PRELUDE = `"use strict";
{
  const unavailable = (what, hint) => {
    throw new Error(what + " is unavailable in a workflow (it breaks resume); " + hint);
  };
  const noClock = (what) => unavailable(what, "pass a timestamp via args");
  Math.random = () => unavailable("Math.random()", "pass randomness via args or vary by index");

  const RealDate = Date;
  const SafeDate = function Date(...args) {
    if (!new.target) noClock("Date()");
    if (args.length === 0) noClock("new Date()");
    return Reflect.construct(RealDate, args, new.target);
  };
  SafeDate.UTC = RealDate.UTC;
  SafeDate.parse = RealDate.parse;
  SafeDate.now = () => noClock("Date.now()");
  SafeDate.prototype = RealDate.prototype;
  RealDate.prototype.constructor = SafeDate;
  globalThis.Date = SafeDate;

  const dateFormat = Intl.DateTimeFormat.prototype;
  const boundFormat = Object.getOwnPropertyDescriptor(dateFormat, "format").get;
  const formatToParts = dateFormat.formatToParts;
  Object.defineProperty(dateFormat, "format", {
    configurable: true,
    get() {
      const format = Reflect.apply(boundFormat, this, []);
      return (date) => (date === undefined ? noClock("Intl.DateTimeFormat format() without a date") : format(date));
    },
  });
  dateFormat.formatToParts = function (date) {
    if (date === undefined) noClock("Intl.DateTimeFormat formatToParts() without a date");
    return Reflect.apply(formatToParts, this, [date]);
  };
}`;
