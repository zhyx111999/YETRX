// How a recorded run starts again as a new run.

import type { PersistedRunState } from "../run-persistence.js";
import type { ExecOptions } from "./run-execution.js";

/** The startInBackground() arguments that repeat a recorded run. */
export interface RestartRequest {
  script: string;
  args: unknown;
  exec: ExecOptions;
}

/**
 * The start request that repeats `run`, or undefined when the record kept no
 * script. Tools are functions and never reach the record, so its toolset name
 * is passed on: the new run resolves the same tools (deep-research's web tools)
 * the way resume() does.
 */
export function restartRequest(
  run: Pick<PersistedRunState, "script" | "args" | "toolset">,
): RestartRequest | undefined {
  if (!run.script) return undefined;
  return { script: run.script, args: run.args, exec: { toolset: run.toolset } };
}
