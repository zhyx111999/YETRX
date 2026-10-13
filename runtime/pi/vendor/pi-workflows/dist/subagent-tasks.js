/**
 * Task-list runs: the `subagents` tool's `tasks` input starts one subagent per
 * task through a fixed script that receives the list as `args`. The tasks are
 * independent, so a resume with an edited list reuses every unchanged task
 * wherever it moved instead of only an unchanged prefix.
 */
/**
 * The script every task-list run executes. The list travels as `args` rather
 * than being embedded, so task text can never trip script validation (for
 * example a task that mentions Date.now()). The result is one text report
 * because delivery shows strings in full.
 */
export const SUBAGENT_TASKS_SCRIPT = [
    'export const meta = { name: "subagents", description: "One subagent per task, run in parallel" }',
    "const results = await parallel(args.map((t) => () => agent(t.task, { label: t.label, model: t.model })))",
    'return results.map((result, i) => "## " + args[i].label + "\\n\\n" + (result ?? "(no result)")).join("\\n\\n")',
].join("\n");
/** True for a run started from a task list rather than a script or a name. */
export function isTaskListScript(script) {
    return script === SUBAGENT_TASKS_SCRIPT;
}
/**
 * Validate a task list: a non-empty array of `{ label, task, model? }` with
 * unique non-blank labels and non-blank tasks. Labels and models are trimmed;
 * a null model counts as omitted because some providers fill every optional
 * field.
 */
export function parseSubagentTasks(value) {
    if (!Array.isArray(value) || value.length === 0) {
        throw new Error("tasks must be a non-empty array of { label, task, model? }");
    }
    const labels = new Set();
    return value.map((item, index) => {
        if (!item || typeof item !== "object" || Array.isArray(item))
            throw new Error(`tasks[${index}] must be an object`);
        const { label, task, model, ...rest } = item;
        const extra = Object.keys(rest)[0];
        if (extra !== undefined)
            throw new Error(`tasks[${index}] does not accept ${extra}`);
        if (typeof label !== "string" || !label.trim())
            throw new Error(`tasks[${index}].label must be a non-empty string`);
        if (typeof task !== "string" || !task.trim())
            throw new Error(`tasks[${index}].task must be a non-empty string`);
        const name = label.trim();
        if (labels.has(name))
            throw new Error(`tasks label "${name}" is used more than once; labels must be unique`);
        labels.add(name);
        if (model === undefined || model === null)
            return { label: name, task };
        if (typeof model !== "string" || !model.trim()) {
            throw new Error(`tasks[${index}].model must be a non-empty string when given`);
        }
        return { label: name, task, model: model.trim() };
    });
}
/**
 * Where each unchanged task of `previous` sits in `next`, as old index → new
 * index. A task is unchanged when its label, task, and model all match; the
 * index is the task's call index in the fixed script.
 */
export function unchangedTaskMoves(previous, next) {
    const nextIndex = new Map(next.map((task, index) => [taskIdentity(task), index]));
    const moves = new Map();
    previous.forEach((task, index) => {
        const moved = nextIndex.get(taskIdentity(task));
        if (moved !== undefined)
            moves.set(index, moved);
    });
    return moves;
}
function taskIdentity(task) {
    return JSON.stringify([task.label, task.task, task.model ?? null]);
}
