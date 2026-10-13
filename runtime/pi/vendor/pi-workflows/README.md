# pi-workflows-lean

<p>
  <a href="#license"><img src="https://img.shields.io/badge/license-MIT-blue" alt="MIT license"></a>
  <a href="https://pi.dev"><img src="https://img.shields.io/badge/for-Pi-7c3aed" alt="Built for Pi"></a>
</p>

An independently maintained lean fork of [pi-dynamic-workflows](https://github.com/QuintinShaw/pi-dynamic-workflows) 3.13.0. It does not follow upstream releases and is not published to npm.

Turn one request into subagents that work in parallel — a task list for independent jobs, or a JavaScript orchestration script for multi-step work — with each task routed to the right model and one combined result delivered back. Intermediate work stays in subagent sessions instead of filling your chat context.

Built for work that is too broad for one agent and one context window. The built-in pattern left in this fork is `/deep-research`.

![A workflow run showing parallel agents and live progress](docs/media/demo.gif)

## Install

Requires Pi 0.84.0 or newer. `dist/` is committed, so Pi installs and runs it straight from git:

```bash
pi install git:github.com/zhyx111999/pi-workflows-lean
```

A local clone works too: `pi install /absolute/path/to/pi-workflows-lean`. The extension always runs on the Pi that loads it, even when the clone has its own copies of Pi's packages installed for development.

Run `/reload` after a first install (restart Pi after an update), then ask naturally:

```text
Run a workflow to audit every route under src/routes/ for missing auth checks.
```

Pi starts subagents in the background — a task list for independent work, or a workflow script for multi-step work. A live panel tracks progress while you keep working, and the final result is delivered back into the conversation automatically.

Each message you type carries a hidden note for the model, not shown in the chat, on when delegating to subagents is worthwhile. `/workflows run <prompt>` starts subagents explicitly. Configuration is plain JSON that the model can edit for you; the packaged `subagents-setup` skill tells it where everything lives.

## How it works

1. **Orchestrate** — Pi lists independent tasks, or writes a deterministic JavaScript workflow with `agent()`, `parallel()`, `pipeline()`, and `phase()` when steps depend on each other.
2. **Fan out** — fresh subagent sessions run concurrently, optionally on different models or isolated git worktrees.
3. **Return** — completed work is journaled for resume, and one result is delivered back into the conversation.

The orchestration itself is plain JavaScript:

```js
export const meta = {
  name: 'auth_audit',
  description: 'Find routes missing auth checks',
  phases: [{ title: 'Scan' }, { title: 'Review' }, { title: 'Report' }],
}

phase('Scan')
const files = await agent('List every route file under src/routes/.', { tier: 'small' })

phase('Review')
const findings = await parallel(
  files.split('\n').filter(Boolean).map((file) =>
    () => agent(`Audit ${file} for missing auth checks.`, {
      tier: 'medium',
      isolation: 'worktree',
    }),
  ),
)

phase('Report')
return await agent(
  'Combine these findings into one report:\n' + findings.join('\n\n'),
  { tier: 'big' },
)
```

## Why use it

- **Real parallel orchestration** — fan out up to 16 concurrent and, by default, 1000 total subagents from one orchestration script.
- **Per-agent model routing** — use `small`, `medium`, or `big` tiers, or choose an exact provider/model and thinking level.
- **Task lists without a script** — pass `tasks: [{ label, task, model? }]` and each task runs in its own subagent, all in parallel.
- **Journaled resume** — replay completed agents after interruption without rerunning them or spending their tokens again. The orchestrator can also resume with an **edited task list or script**: unchanged work replays from cache and only edited/new work re-runs — so a single bad prompt no longer means paying to re-run the whole run.
- **Steer running subagents** — queue an extra instruction into a subagent while it works, without restarting it.
- **Git worktree isolation** — parallel agents edit on separate branches with `isolation: "worktree"`. Kept by default for merge; pass `keepWorktree: false` to delete (tests).
- **Measured usage** — report real tokens and cost from each subagent session. When a provider session ends without reporting usage, the affected totals are heuristic character estimates and UI token surfaces render them with a `~` prefix (e.g. `~640 tok`) — never silently as metered figures.
- **Visible background runs** — track phases, agents, models, fresh/cache tokens, cost, and live tok/s from the progress panel or `/workflows` navigator.
- **Plain control flow** — loops, semantic retries, and stopping rules are ordinary JavaScript around `agent()`; `checkpoint()` is the only pause helper. Cross-check helpers are not included.
- **Reusable workflows** — save any run as a command and run saved workflows by name.

## Supported workflow capabilities

The installed extension generates this compact index from its executable capability contract. Read the [workflow authoring guide](docs/workflow-authoring.md) or use the packaged `workflow-authoring` skill for constraints, lifecycle guidance, and adaptable examples; configured route and agent-type values remain environment-specific.

<!-- BEGIN GENERATED SUPPORTED WORKFLOW CAPABILITIES -->
| Name | Classification | Signature | Options and defaults |
| --- | --- | --- | --- |
| agent | runtime-global | `agent(prompt, options?) => Promise<string \| structured value \| null>` | `label`: string (optional; default: derived from phase and call count)<br>`phase`: non-empty string (optional; default: current phase)<br>`schema`: plain JSON Schema object (optional)<br>`model`: non-empty string (optional)<br>`thinking`: "off" \| "minimal" \| "low" \| "medium" \| "high" \| "xhigh" \| "max" (optional)<br>`tier`: non-empty string (optional)<br>`isolation`: "worktree" \| false (optional)<br>`keepWorktree`: boolean (optional; default: true)<br>`cwd`: absolute directory path (optional)<br>`thread`: non-empty string (optional)<br>`agentType`: non-empty string (optional) |
| parallel | runtime-global | `parallel(thunks) => Promise<Array<unknown \| null>>` | — |
| pipeline | runtime-global | `pipeline(items, ...stages) => Promise<Array<unknown \| null>>` | — |
| checkpoint | runtime-global | `checkpoint({ kind, checkpointId, payload }) => Promise<unknown>` | — |
| log | runtime-global | `log(message) => void` | — |
| phase | runtime-global | `phase(title) => void` | — |
| args | runtime-global | `args: unknown` | — |
| cwd | runtime-global | `cwd: string` | — |
| process | runtime-global | `process: { cwd(): string }` | — |
| tasks | subagents-tool-input | `tasks?: Array<{ label: string; task: string; model?: string }>` | — |
| script | subagents-tool-input | `script?: string` | — |
| name | subagents-tool-input | `name?: string` | — |
| args | subagents-tool-input | `args?: object` | — |
<!-- END GENERATED SUPPORTED WORKFLOW CAPABILITIES -->

## Built-in workflows

```text
/deep-research <question>   web research that returns sourced claims
```

`/deep-research` is also reachable by name without a slash command. A saved workflow of the same name wins over the built-in.

## Tools and run control

The model works through two tools, so you never need to type a command:

| Tool | Purpose |
| --- | --- |
| `subagents` | Start a background run from exactly one of `tasks` (one subagent per `{ label, task, model? }`, all in parallel, unique labels), `script` (a JavaScript workflow), or `name` (a saved or built-in workflow, with `args`). Returns the run ID at once; the result is delivered back into the conversation when the run finishes. |
| `subagents_control` | `list` runs; `status` of one run (each subagent's state, the current phase, a waiting checkpoint with its payload); `read` the last 8000 characters of one subagent (default: latest run, latest updated subagent); `stop` a run; `resume` a run; `message` a running subagent. |

`resume` continues a paused or failed run. With an edited `tasks` list, every task whose label, task, and model are unchanged reuses its result wherever it moved, and only edited or new tasks run. With an edited `script`, unchanged `agent()` calls replay up to the first edit (see [Determinism and limits](#determinism-and-limits)). A run waiting at a checkpoint resumes with its `checkpointId` and a JSON `response`, which is stored durably before the run continues. Completed and stopped runs do not resume; start a new run instead.

`message` queues text into a running subagent's session with Pi's steering; it arrives before the subagent's next model call. A subagent that finishes first never sees it, and finished work changes only through `resume`.

Some providers fill in every optional tool field. Fields that do not apply to the chosen mode or action are dropped, and null or empty values count as omitted; unknown fields and missing required fields are errors.

Deleting a run is final across processes. Deletion removes the record, its sidecars, exported results, and run log. Another Pi session that still holds the run in memory drops its copy and its undelivered result instead of recreating the record.

| Command | Purpose |
| --- | --- |
| `/workflows` | Open the interactive run navigator |
| `/workflows run <prompt>` | Send the prompt with an instruction to run it as a workflow now |
| `/workflows status <id>` | Watch a run and print its result when complete |
| `/workflows pause\|resume\|stop\|rm <id>` | Control a run |
| `/workflows save <name>` | Save the latest script as a reusable command; replacing a different workflow of that name asks first |
| `/workflows-progress compact\|detailed\|status\|max <N>` | Live-panel detail level (and max agents shown per phase in detailed mode) |
| `/workflows-models` | Map model tiers and thinking levels |

In the navigator: `↑/↓` select · `PgUp/PgDn` page · `Home/End` jump · `/` filter runs by name, ID, or status and saved workflows by name or description · `enter/→` open · `esc/←` back. Filter text updates the visible list immediately; `enter` commits the draft filter. In filter-edit mode, `esc` cancels the draft and keeps the committed query; in browse mode, `esc` first clears an existing filter without closing the navigator, and only a second `esc` with no filter backs/closes normally. On a run, `p` pauses (press `p` again to confirm), `x` stops (press `x` again to confirm), `r` restarts it as a new run with the same arguments and toolset, and `s` saves its script (replacing a different saved workflow of the same name needs a second `s`); these lifecycle controls remain bound to the run while viewing its phases, agents, or detail. On a saved workflow (including its detail view), `r` renames and `x` deletes (press `x` again to confirm). Rename `enter` commits and `esc` cancels; names must work as file names on every platform: no whitespace, controls, slashes, or `<>:"|?*`, no trailing dot, and no Windows device name such as `CON` or `COM1`. `q` quits.

Agent details use a compact summary by default: completed agents show their final result, while active agents show the prompt and two latest history events. Press `enter` to open the full syntax-highlighted pager. In the pager, use `j/k` or `↑/↓` for lines, `PgUp/PgDn` for pages, `g/G` for the ends, and `t` to toggle live tail mode.

## Runtime reference

| Global | What it does |
| --- | --- |
| `agent(prompt, opts)` | Spawn an isolated subagent; optionally validate its result with JSON Schema |
| `parallel(thunks)` | Run `() => agent(...)` thunks concurrently and preserve input order |
| `pipeline(items, ...stages)` | Fan items through sequential stages |
| `phase(title)` | Group work in the live view |
| `checkpoint({ kind, checkpointId, payload })` | Durable pause until a controller supplies a response |

| Agent option | Description |
| --- | --- |
| `tier` | `small`, `medium`, or `big` model routing |
| `model` | Exact `provider/modelId` or `provider/modelId:thinking`; overrides `tier` |
| `agentType` | Named role, tool, and model definition |
| `isolation` | `"worktree"` for conflict-free parallel edits; `false` opts out of an agentType default |
| `keepWorktree` | Default `true` (kept for merge). `false` deletes after the call (test runs) |
| `schema` | JSON Schema for a validated structured result |
| `label` / `phase` | Display label and phase override |

Host setting / SDK option (not an `agent()` call option): `providerMiddlewareExtensions: string[]` opts child sessions into trusted provider/auth extensions; default `[]`.

`agent()` resolves to plain text unless `schema` is set — a prompt that merely asks the model to "return JSON" does not change that, and reading a field off unparsed text fails silently (`undefined`, not an error), which can make a whole `parallel()` fleet look "successful" while every result is unusable. Parse and validate defensively when a schema isn't set, and flag what doesn't parse instead of dropping it:

```js
function parseOrFlag(text, requiredKeys) {
  const fence = text.match(/```(?:json)?\s*([\s\S]*?)```/i);
  try {
    const value = JSON.parse(fence ? fence[1] : text);
    if (value && typeof value === "object" && requiredKeys.every((k) => k in value)) return { ok: true, value };
  } catch {
    // fall through
  }
  return { ok: false, raw: text };
}
```

Prefer `schema` (JSON Schema validation with bounded repair) over ad hoc parsing whenever the result's shape matters downstream.

The [workflow authoring guide](docs/workflow-authoring.md) and the packaged `workflow-authoring` skill cover script options, structured output, determinism, and saved workflows.

<details>
<summary><strong>Model tiers and run controls</strong></summary>

Model tiers live at `~/.pi/workflows/model-tiers.json`. A project file at `~/.pi/workflows/projects/<project>/model-tiers.json` overlays the global map (project keys win). They accept Pi CLI-style thinking suffixes:

```json
{
  "tiers": {
    "small": "openai-codex/gpt-5.4-mini:low",
    "medium": "openai-codex/gpt-5.4:medium",
    "big": "openai-codex/gpt-5.5:xhigh"
  }
}
```

Use `/workflows-models` to edit them interactively; it is cwd-aware and can save to the project or global file. A project save writes the resolved map, including keys currently inherited from global. Without a config, the extension ranks authenticated models by capability hints and assigns distinct models when possible.

Untagged `agent()` calls (no `model`, no `tier`, and no agentType/phase routing) use the configured `medium` tier, or the Pi settings default when no tiers are saved. Set `"inheritMainModel": true` in `~/.pi/workflows/settings.json` (or a project overlay) to instead have untagged agents inherit the orchestrating session's main model — the model in effect when the run starts; a mid-run `/model` switch applies to subsequent runs; when the session has no main model, the medium tier or settings default applies — with no tier config required. Explicit `model`/`tier` tags are unaffected, and an unavailable inherited model degrades to the settings default with a run-visible warning instead of throwing. A `tier` the saved tiers do not define fails the agent instead of running another model; with no tiers saved, the session's main model stands in for every tier, with a note in the run log.

Run limits come only from settings; neither a tool call nor a script can override them:

| Setting | Default | Meaning |
| --- | --- | --- |
| `defaultConcurrency` | 8 | Agents running at once, clamped to 1–16 |
| `defaultMaxAgents` | 1000 | Logical `agent()` and `checkpoint()` calls per run; retries add none |
| `defaultAgentRetries` | 0 | Retries after a recoverable agent failure, clamped to 0–3 |
| `defaultAgentTimeoutMs` | none | Hard timeout per agent attempt; unset or `null` means no timeout |

Changes apply after `/reload` or in a new session. Every execution, including a resumed one, uses the current values, so a run that stopped at the agent cap finishes after you raise `defaultMaxAgents` and resume it. Programmatic runs can tune the retry pacing via `agentRetryBackoffMs?: (failedAttempt: number) => number` (default `min(250*2^(N-1), 2000)` ms; the retry holds its concurrency slot during the wait). Settings load in order (later wins): global `~/.pi/workflows/settings.json` → repo-local `<cwd>/.pi/workflows/settings.json` → the existing external project override `~/.pi/workflows/projects/<project>/settings.json`. Repo-local defaults can be version-controlled and shared as common repository configuration; external project overrides remain user-specific.

Child sessions intentionally disable host extensions to avoid per-child factory churn and recursive orchestration. Some providers require extension request/auth hooks in addition to a shared model registry. To opt in, set `"providerMiddlewareExtensions": ["example-provider-adapter"]` in `~/.pi/workflows/settings.json` (also available on `WorkflowAgent`, `runWorkflow`, and `WorkflowManager` SDK options). Omitted or `[]` keeps all host extensions disabled. Names match an extension filename without its JS/TS suffix, an exact npm package identity, or a resolved local/Git package source name, case-insensitively; ancestor directory names do not authorize descendants; only enabled resources are considered, and project resources still require Pi project trust. `pi-dynamic-workflows`, `workflow`, and `pi-subagents` are always excluded, even if allowlisted. Only allow trusted, child-safe middleware: the allowlist enables whole extensions, not a hook-only sandbox, and opted-in factories and runtimes are isolated per child session and receive session_shutdown before disposal. Extension-free resource loaders remain shared. Explicit SDK resource-loader injection remains authoritative and bypasses this discovery filter.

A schema-less agent call that comes back as whitespace-only text is a recoverable `AGENT_EMPTY_OUTPUT` failure and retries like any other. Some models occasionally hit this on an otherwise-fine first attempt; if a fleet is built on one of them, set `defaultAgentRetries` to 1–2 rather than treating an isolated empty output as a failed run. Because an exhausted recoverable failure resolves to `null` rather than throwing, a run whose **every** agent came back empty still reports `completed`; when that happens the runtime logs a prominent `⚠ Workflow produced no usable results` warning (naming the empty agents and pointing at `defaultAgentRetries` and output-token limits) so an all-null fleet can't be mistaken for success.

Token totals stay cumulative across a pause and resume.

Programmatic hosts can set `drainAbortGraceMs` on `runWorkflow` or manager execution options to bound the wait for agents that ignore cancellation: the final wait of a cancelled run, and the wait after an agent attempt times out. A timed-out agent still running after the grace is abandoned without a retry and ends as a timeout failure, freeing its concurrency slot. The default is 10,000 ms; `Infinity` waits without a bound. Finite values from 1 through 2,147,483,647 are rounded down; other values use the default. This is a host-only option, not a tool input or persisted setting. It does not limit a successful run's final wait or a checkpoint suspension unless the run is also cancelled. After abandonment, already-reported terminal usage is retained, provisional usage is rolled back, and late agent callbacks cannot modify the settled run.

</details>

<details>
<summary><strong>Storage, resume, and persisted sessions</strong></summary>

Extension state lives outside the repository under `~/.pi/workflows`:

- global settings and tiers: `~/.pi/workflows/settings.json` and `model-tiers.json`
- project runs, journals, locks, saved overrides, and optional tier overlay: `~/.pi/workflows/projects/<project>/`
- repo-local `<cwd>/.pi/workflows/settings.json` is also read for shared defaults, between global settings and the existing external project override `~/.pi/workflows/projects/<project>/settings.json`. This path is relative to the supplied project cwd; it does not search parent directories. Missing, corrupt, or invalid files are ignored. `/workflows-progress` saves to the global settings and, for every key the repo-local file or the external project override already sets, also to the external project override (creating it), so the saved value is the one that loads; it never writes the repo-local file.

Subagents are in-memory by default. Set `persistAgentSessions: true` to retain full transcripts in Pi's standard session directory, with one file per unthreaded call or named thread. Persisted child session headers link back to the originating host session through `parentSession` when the parent has a persisted file. Workflow run JSON records the original parent session ID/file and each call's child session ID/file; those links and historical call timestamps survive journal replay on resume. Delivery may move to a replacement host session, or to a session that explicitly resumes the run, without rewriting the original parent lineage. In-memory children still have session IDs, but no session files for reconstructing historical per-message usage. Persisted transcripts may store sensitive material that an agent read, so enable them deliberately.

`PI_WORKFLOW_AGENT_CACHE_RETENTION` sets the prompt-cache retention tier for agent sessions, overriding `PI_CACHE_RETENTION` for them only. Anthropic charges more per cache write on the 1h tier than the 5m one; a long-lived parent conversation earns that back by surviving idle gaps, but agent sessions are short-lived and rarely idle long enough to claim the longer window, so a wide fan-out pays the higher write price without the benefit. Set `PI_CACHE_RETENTION=long PI_WORKFLOW_AGENT_CACHE_RETENTION=short` to keep the longer window for your own conversation only. Unset by default, so agents inherit the parent's retention and behaviour is unchanged unless you opt in. The override is applied to each agent session's own stream function rather than to `process.env`, so a background run cannot change retention for a parent turn streaming at the same time.

Run storage uses a small versioned `<runId>.json` index head plus a `<runId>.json.events.jsonl` change log that is appended to, and rewritten as one snapshot once its history outgrows the run. Only bytes committed by the head participate in replay. A record in any other format is refused rather than guessed at. See [the storage protocol](docs/run-storage.md) for recovery details.

Completed background runs retain their full result in run storage. Conversation delivery also creates an immutable JSON result artifact (`<runId>.json.result-<content-hash>`) and links to it, so a shortened summary still has a directly readable full result. These artifacts are removed with the run.

In-process session replacements (`/reload`, `/new`, resume, fork) keep the live workflow manager. Active background runs therefore continue streaming progress, remain controllable, and deliver their result into the replacement session. Node keeps the extension's compiled modules for the life of the Pi process, so `/reload` keeps running the build Pi started with; after updating the package, restart Pi to load it (a new session warns when the package on disk differs from the code it is running). A handoff between two different builds, such as an updated package or two installed copies, or a process exit pauses active runs onto the journal recovery path instead of mixing code or burning tokens after teardown. A process restart uses the same durable journal path, recovering an interrupted running workflow as paused so it can be resumed safely.

Finished runs (completed, failed, or aborted) are retained in full on disk, capped at the 300 most recent per project — older ones are evicted first; running, paused, leased and undelivered runs are protected. Only a smaller number (20 by default) also stay fully loaded in the live manager. History listings and startup recovery read small index heads; details hydrate on demand through a cache capped at eight records and a 16 MiB serialized-weight budget. The navigator retains only its most recently selected historical snapshot. Library embedders can tune `maxTerminalRunsInMemory` on `WorkflowManager` and `maxTerminalRunsOnDisk` on the run-persistence layer.

</details>

<details>
<summary><strong>How it maps to Claude Code dynamic workflows</strong></summary>

| Claude Code dynamic workflows | pi-workflows-lean on Pi |
| --- | --- |
| Code-mode orchestration | JavaScript `agent()` / `parallel()` / `pipeline()` / `phase()` in a VM realm (for determinism, not a security boundary) |
| Isolated subagent contexts | Fresh in-memory Pi sessions; results remain in variables |
| Structured outputs | JSON Schema validation with bounded repair |
| Background runs | Non-blocking run, live panel, and automatic result delivery |
| Resume | Journaled replay, including edit-and-resume with a revised task list or script |
| Model selection | Per-agent and per-phase routing across authenticated providers |
| Additional Pi features | Worktree isolation, real cost accounting, and deep research |

</details>

## Determinism and limits

Workflow scripts run in a Node `vm` sandbox. `Date.now()`, `Math.random()`, `new Date()`, `require`, `import`, filesystem access, and network access are unavailable inside the orchestration script. Subagents use their assigned tools; keeping the orchestrator deterministic is what makes journal replay reliable.

Script journal replay — including edit-and-resume with an edited script — matches cached agent results by **call position**: the script body and each branch of a `parallel()` or `pipeline()` number their own calls in the order they make them, so a call keeps its position whichever concurrent call finishes first. Editing an `agent()` prompt in place reuses the cache up to that call and re-runs it and every call made after it. Inserting, removing, or reordering an `agent()` call shifts the later calls of its branch, and of the script after that fan-out, and invalidates the cache from that point on (mismatched calls simply re-run — no crash). To preserve the cached prefix, keep the earlier still-good `agent()` calls unchanged and in the same order, and keep the branches of one fan-out independent of each other.

Only a call that finishes with a real result is journaled — a call whose every attempt ended in a recoverable failure (including one that only ever produced `AGENT_EMPTY_OUTPUT`) is never cached. Resuming such a run therefore replays every earlier, already-succeeded call from cache and re-runs only that one call and every call made after it — cheap and exactly targeted, not a full re-run of the fleet.

Task lists do not depend on position: their tasks are independent, so every task with a journaled result replays on resume wherever it sits in the list, and only the rest run.

## Development

```bash
npm install
npm test     # Biome, type check, generated-doc and context-size checks, build, unit tests
```

The capability table above and the skill's capability references are generated from `src/workflow-capability-contract.ts`; regenerate them with `npm run docs:generate`, and the context-size measurement with `npm run context:generate`. See [CONTRIBUTING.md](./CONTRIBUTING.md) for the full conventions, and the [architecture guide (Chinese)](docs/architecture.zh-CN.md) for how the modules fit together.

## Credits

The code-mode orchestration idea comes from [Michael Livs' original pi-dynamic-workflows](https://github.com/Michaelliv/pi-dynamic-workflows) and Anthropic's [dynamic workflows in Claude Code](https://claude.com/blog/introducing-dynamic-workflows-in-claude-code). [QuintinShaw's pi-dynamic-workflows](https://github.com/QuintinShaw/pi-dynamic-workflows), the base of this fork, added model routing, journaled resume, worktree isolation, measured usage, an interactive TUI, and built-in workflows.

## License

MIT — see [LICENSE](./LICENSE).
