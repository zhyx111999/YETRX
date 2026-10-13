const TOOL_LINES = {
  read: "read: read a file (text or image).",
  bash: "bash: run a shell command in the working directory.",
  edit: "edit: replace exact text in an existing file.",
  write: "write: create or overwrite a file.",
  grep: "grep: search file contents.",
  find: "find: find files by name pattern.",
  ls: "ls: list a directory.",
  subagents:
    "subagents: start background subagents; it returns a run id at once and the result is delivered back into this conversation when the run finishes.",
  subagents_control: "subagents_control: check, read, message, stop or resume a background run.",
};

/**
 * The default prompt: a short statement of the environment and a one-line description
 * of each tool. Pi appends the working directory, project context files and the skill
 * list after it; nothing else is added.
 *
 * @param {string[]} extraTools optional built-ins enabled for this session
 */
export function buildSystemPrompt(extraTools = []) {
  const names = ["read", "bash", "edit", "write", ...extraTools, "subagents", "subagents_control"];
  return [
    "You work inside the user's workspace and act through the tools below. Do what was asked, check results before reporting them, and say plainly what you did and what you could not do.",
    "",
    "Tools:",
    ...names.map((name) => `- ${TOOL_LINES[name]}`),
    "",
    "Skills listed below are loaded on demand: read a skill's SKILL.md when its description matches the task.",
  ].join("\n");
}

/** Keep the host's latest prompt for both human turns and automatic background delivery. */
export function createHostPrompt(initialPrompt = "", extraTools = []) {
  const fallback = buildSystemPrompt(extraTools);
  const normalize = (value) => (value.trim() ? value : fallback);
  let current = normalize(initialPrompt);

  /** @type {import("@earendil-works/pi-coding-agent").ExtensionFactory} */
  const extension = (pi) => {
    pi.on("before_agent_start", (event) => {
      // Only replace the prefix; SDK context, skills, tools and other extension edits survive.
      event.systemPromptOptions.customPrompt = current;
    });
  };

  return {
    extension,
    get: () => current,
    set: (value) => {
      current = normalize(value);
    },
  };
}
