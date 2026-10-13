import { mkdirSync } from "node:fs";
import { resolve } from "node:path";

/**
 * pi and the workflow extension both locate their configuration through
 * `PI_CODING_AGENT_DIR`. The host sets it on the process; `init.agentDir` must agree
 * with it, so a mistaken init can never point a running process at another
 * directory. When the variable is absent it is derived from the first init.
 */
export function bindAgentDir(agentDir) {
  const external = process.env.PI_CODING_AGENT_DIR;
  if (external && resolve(external) !== agentDir) {
    throw new Error(`agentDir ${agentDir} does not match PI_CODING_AGENT_DIR ${resolve(external)}`);
  }
  process.env.PI_CODING_AGENT_DIR = agentDir;
  mkdirSync(agentDir, { recursive: true, mode: 0o700 });
}
