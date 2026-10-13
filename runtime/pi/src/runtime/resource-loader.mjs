import { homedir } from "node:os";
import { isAbsolute, join, relative, resolve } from "node:path";
import { DefaultResourceLoader } from "@earendil-works/pi-coding-agent";
import { resolveWorkflowPackage } from "../config.mjs";

function isInside(parent, path) {
  const rel = relative(resolve(parent), resolve(path));
  return rel === "" || (!rel.startsWith("..") && !isAbsolute(rel));
}

/**
 * pi always scans `~/.agents/skills` (the operator's personal skills, found through the
 * same `HOME || homedir()` rule pi uses) and tags them like any top-level skill, so the
 * only way to keep them out is by location.
 */
function isPersonalSkill(skill) {
  return isInside(join(process.env.HOME || homedir(), ".agents", "skills"), skill.filePath);
}

/**
 * Standard pi discovery (agent directory, project context files, trusted project
 * resources) with exactly two substitutions: the system prompt is ours and nothing is
 * appended to it, and the vendored workflow extension and its skills are always present.
 * Themes and prompt templates are skipped; nothing here renders or expands them.
 */
export async function createResourceLoader({ cwd, agentDir, settingsManager, systemPrompt }) {
  const workflows = resolveWorkflowPackage();
  const loader = new DefaultResourceLoader({
    cwd,
    agentDir,
    settingsManager,
    additionalExtensionPaths: workflows.extensionPaths,
    additionalSkillPaths: workflows.skillPaths,
    noThemes: true,
    noPromptTemplates: true,
    systemPromptOverride: () => systemPrompt,
    appendSystemPromptOverride: () => [],
    skillsOverride: (base) => ({
      skills: base.skills.filter((skill) => !isPersonalSkill(skill)),
      diagnostics: base.diagnostics,
    }),
  });
  await loader.reload();
  return loader;
}
