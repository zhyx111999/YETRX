/**
 * Configuration constants for pi-dynamic-workflows.
 */
/** Default cap on logical agent()/checkpoint() calls per run (setting: defaultMaxAgents). */
export const DEFAULT_MAX_AGENTS = 1000;
/** Default timeout for a single agent in milliseconds. null means no hard timeout. */
export const DEFAULT_AGENT_TIMEOUT_MS = null;
/** Maximum concurrent agents (matches Claude Code limit). */
export const MAX_CONCURRENCY = 16;
/** Maximum automatic retry attempts after a recoverable agent failure. */
export const MAX_AGENT_RETRIES = 3;
/** User-level model tiers config file, relative to the home directory. */
export const MODEL_TIERS_FILE = ".pi/workflows/model-tiers.json";
/** User-level workflow extension settings file, relative to the home directory. */
export const WORKFLOW_SETTINGS_FILE = ".pi/workflows/settings.json";
/**
 * Project directory of agent definitions, relative to the project (cwd/.pi/agents).
 * User definitions live in `~/.pi/agent/agents/` (see agent-registry.ts); a
 * project definition wins on a name collision. Each `*.md` file is one
 * definition (frontmatter + body prompt).
 */
export const AGENTS_DIR = ".pi/agents";
