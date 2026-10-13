import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, resolve } from "node:path";

/** Provider name the Go relay is registered under. Models keep their raw ids. */
export const PROVIDER_ID = "yetrx";
export const PROVIDER_API = "openai-completions";

/** Built-in tools that can be switched on through `init.tools`; the rest are on by default. */
export const OPTIONAL_BUILTIN_TOOLS = ["grep", "find", "ls"];

/** Tools the workflow extension must contribute, or the bridge refuses to start. */
export const REQUIRED_WORKFLOW_TOOLS = ["subagents", "subagents_control"];

export const THINKING_LEVELS = ["off", "minimal", "low", "medium", "high", "xhigh", "max"];

const require = createRequire(import.meta.url);

/**
 * Extension and skill paths of the vendored pi-workflows-lean package, read from
 * its own `pi` manifest so this bridge never restates which skills it ships.
 */
export function resolveWorkflowPackage() {
  const manifestPath = require.resolve("@zhyx111999/pi-workflows-lean/package.json");
  const root = dirname(manifestPath);
  const manifest = JSON.parse(readFileSync(manifestPath, "utf8"));
  const resolveAll = (entries = []) => entries.map((entry) => resolve(root, entry));
  return {
    root,
    extensionPaths: resolveAll(manifest.pi?.extensions),
    skillPaths: resolveAll(manifest.pi?.skills),
  };
}
