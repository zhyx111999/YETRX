<!-- GENERATED from WORKFLOW_CAPABILITY_CONTRACT; do not edit by hand. -->
# Exhaustive workflow capability facts

Contract format: `2.1.0`<br>
Contract content / skill / extension: `3.13.0-lean.1`

Every exact fact below is projected from the installed extension's capability contract. Explanatory judgment belongs in the hand-written references next to this file.

<a id="agent"></a>
## agent

- Classification: `runtime-global`
- Support: `supported`
- Signature: `agent(prompt, options?) => Promise<string \| structured value \| null>`
- Argument `prompt`: non-empty string (required)
- Option shape: `agent-options`
- `label`: string (optional; default: derived from phase and call count; blank uses the default)
- `phase`: non-empty string (optional; default: current phase)
- `schema`: plain JSON Schema object (optional)
- `model`: non-empty string (optional; highest-priority exact model selector)
- `thinking`: "off" | "minimal" | "low" | "medium" | "high" | "xhigh" | "max" (optional; call-site value overrides agentType thinking; selected model suffix takes precedence)
- `tier`: non-empty string (optional; configured route name; dynamic reference: model-routes)
- `isolation`: "worktree" | false (optional; false opts out of an agentType worktree default)
- `keepWorktree`: boolean (optional; default: true)
- `cwd`: absolute directory path (optional; must exist; resolved to its real path; coding tools and session cwd use the target directory; settings and AGENTS/skill resources also use it unless explicitly injected by the embedding host; cannot combine with worktree isolation; on Windows a path segment that ends with a dot or space is rejected, because processes would start in a different directory)
- `thread`: non-empty string (optional; trimmed; same-name calls must be sequential)
- `agentType`: non-empty string (optional; must come from provided context; a name the agent registry does not define fails the call; dynamic reference: agent-types)
- Constraint: prompt must be a non-empty string; options must be an object using only the declared option names, each value matching its declared type (undefined means omitted); anything else fails before dispatch with SCRIPT_VALIDATION_ERROR listing the accepted names or values
- Constraint: recoverable failures return null after retries; nonrecoverable failures throw
- Constraint: schema noncompliance after bounded structured-output repair is nonrecoverable and bypasses agent retries
- Constraint: run limits come only from settings: recoverable failures retry defaultAgentRetries times (0..3), each attempt is bounded by defaultAgentTimeoutMs (unset means no hard timeout), and defaultConcurrency agents run at once (1..16)
- Constraint: every agent() and checkpoint() call consumes one of the run's defaultMaxAgents slots (default 1000); execution retries consume none
- Constraint: resume replays only the longest unchanged prefix; the first miss and every later call execute live
- Constraint: a named thread retains its full Pi transcript and session identity only within one uninterrupted workflow invocation
- Constraint: threaded calls are live-execution resume barriers and are never journaled
- Constraint: same-thread calls must be sequential; threads cannot use worktree isolation
- Constraint: a named thread's canonical cwd is fixed by its first call; a later call using that name with a different cwd fails validation instead of reusing the prior transcript
- Constraint: an explicit cwd must be a non-empty absolute existing directory; its real path determines coding tools and session cwd; default settings and AGENTS/skill resources follow it while embedding-host dependency overrides remain authoritative; it participates in resume identity and cannot combine with worktree isolation
- Constraint: selector priority is explicit model > agentType model > tier > phase model > metadata model > inherited main model (inheritMainModel on) > implicit medium > session default
- Constraint: an explicit model, agentType model, tier, or phase model that resolves to an unavailable model throws MODEL_NOT_FOUND naming the source (e.g. the tier and what it resolved to) instead of falling back
- Constraint: a tier that a saved model-tiers.json does not define throws MODEL_NOT_FOUND listing the defined tiers; with no model-tiers.json saved, the session's main model stands in for every tier and the run log says so once
- Constraint: only the implicit route an untagged agent falls into — the implicit default medium tier, or the inherited main model when inheritMainModel is on (no explicit model, tier, agentType, or phase model requested) — degrades to the session default when unavailable, logging a one-time run-visible warning instead of throwing
- Constraint: with the inheritMainModel setting on, an untagged agent (no explicit model, tier, agentType model, or phase model) inherits the session's main model (the model in effect when the run starts) instead of the implicit medium tier or the settings default; an unavailable inherited model degrades to the settings default with a one-time run-visible warning instead of throwing
- Constraint: requested worktree isolation fails closed before agent execution if git cannot create the worktree; it never falls back to the shared checkout
- Constraint: keepWorktree defaults true (worktree kept for merge); false deletes after the call
- Constraint: each live execution creates a uniquely owned worktree; retries or resume never reuse or overwrite a retained worktree

<a id="parallel"></a>
## parallel

- Classification: `runtime-global`
- Support: `supported`
- Signature: `parallel(thunks) => Promise<Array<unknown \| null>>`
- Constraint: requires functions rather than promises
- Constraint: result order matches input order
- Constraint: a thunk's recoverable workflow failure becomes null, as with agent(); nonrecoverable failures and errors thrown by the thunk's own code reject parallel()

<a id="pipeline"></a>
## pipeline

- Classification: `runtime-global`
- Support: `supported`
- Signature: `pipeline(items, ...stages) => Promise<Array<unknown \| null>>`
- Constraint: items run concurrently while stages per item run sequentially
- Constraint: each stage receives previousValue, originalItem, and zero-based index
- Constraint: a null stage result is passed to the next stage; authors must guard missing coverage explicitly
- Constraint: a stage's recoverable workflow failure becomes null, as with agent(); nonrecoverable failures and errors thrown by the stage's own code reject pipeline()

<a id="checkpoint"></a>
## checkpoint

- Classification: `runtime-global`
- Support: `supported`
- Signature: `checkpoint({ kind, checkpointId, payload }) => Promise<unknown>`
- Constraint: consumes one agent slot and no tokens
- Constraint: journaled answers replay only within an unchanged resume prefix
- Constraint: accepts an open kind identifier, persists its JSON payload, and pauses the run
- Constraint: subagents_control resume answers a waiting checkpoint with its exact checkpointId and a lossless-JSON response, attached durably before the run continues
- Constraint: subagents_control status shows the waiting checkpoint's ID, kind, and a bounded payload preview, never its response
- Constraint: durable checkpoint responses resume the same run ID, are journaled before continuation, and reject stale or conflicting delivery

<a id="log"></a>
## log

- Classification: `runtime-global`
- Support: `supported`
- Signature: `log(message) => void`

<a id="phase"></a>
## phase

- Classification: `runtime-global`
- Support: `supported`
- Signature: `phase(title) => void`
- Constraint: title must be a nonblank string

<a id="args"></a>
## args

- Classification: `runtime-global`
- Support: `supported`
- Signature: `args: unknown`

<a id="cwd"></a>
## cwd

- Classification: `runtime-global`
- Support: `supported`
- Signature: `cwd: string`

<a id="process"></a>
## process

- Classification: `runtime-global`
- Support: `supported`
- Signature: `process: { cwd(): string }`

<a id="console"></a>
## console

- Classification: `runtime-global`
- Support: `compatibility`
- Signature: `console: { log, info, warn, error }`
- Constraint: new workflows should use log()

<a id="subagents-input-tasks"></a>
## tasks

- Classification: `subagents-tool-input`
- Support: `supported`
- Signature: `tasks?: Array<{ label: string; task: string; model?: string }>`
- Constraint: exactly one of tasks, script, or name per call
- Constraint: one subagent per task, all in parallel; labels must be unique and non-blank
- Constraint: returns one text report with a section per label, in task order
- Constraint: subagents_control resume with an edited list reuses every task whose label, task, and model are unchanged, wherever it moved; edited and new tasks run

<a id="subagents-input-script"></a>
## script

- Classification: `subagents-tool-input`
- Support: `supported`
- Signature: `script?: string`
- Constraint: exactly one of tasks, script, or name per call
- Constraint: raw JavaScript workflow source
- Constraint: subagents_control resume with an edited script replays unchanged positional agent calls until the first changed or inserted call

<a id="subagents-input-name"></a>
## name

- Classification: `subagents-tool-input`
- Support: `supported`
- Signature: `name?: string`
- Constraint: exactly one of tasks, script, or name per call
- Constraint: resolves a project/user saved workflow first, then a built-in pattern

<a id="subagents-input-args"></a>
## args

- Classification: `subagents-tool-input`
- Support: `supported`
- Signature: `args?: object`
- Constraint: the args global of a script or named workflow; ignored with tasks

<a id="metadata"></a>
## export const meta

- Classification: `script-contract`
- Support: `supported`
- Signature: `export const meta = { name: string, description: string, phases?: Array<{ title: string; detail?: string; model?: string }>, model?: string }`
- Constraint: must be the first statement
- Constraint: name and description must be nonblank strings
- Constraint: each phase title and every model must be a nonblank string; a phase detail must be a string
- Constraint: metadata must use literal values; expressions such as string concatenation and template interpolation are rejected
- Constraint: the meta declaration is the only legal export because the remaining body executes inside an async function

<a id="return-value"></a>
## workflow return value

- Classification: `script-contract`
- Support: `supported`
- Signature: `return JSON-serializable data`
- Constraint: do not return functions, promises, cyclic objects, BigInt, or runtime handles

<a id="determinism"></a>
## deterministic script execution

- Classification: `script-contract`
- Support: `supported`
- Signature: —
- Constraint: Date.now(), Date(), no-argument new Date(), Math.random(), and Intl.DateTimeFormat formatting without an explicit date are unavailable
- Constraint: pass timestamps and randomness through args

<a id="compatibility"></a>
## whole-script Markdown fence stripping

- Classification: `compatibility-behavior`
- Support: `compatibility`
- Signature: —
- Constraint: accepted for compatibility but not recommended

<a id="vm-realm"></a>
## VM realm JavaScript substrate

- Classification: `internal-substrate`
- Support: `internal`
- Signature: —
- Constraint: Node-version-dependent globals are not project-owned workflow API
- Constraint: VM is not a security sandbox

<a id="model-routes"></a>
## model routes

- Classification: `dynamic-reference`
- Support: `supported`
- Signature: —
- Constraint: live values must not be copied into static contract data
- Dynamic reference owner: `model-tier-config`
- Item shape: `{ name: string; description?: string }`
- Live lookup: `loadModelTierConfig`
- Live values are intentionally absent from this static reference.

<a id="agent-types"></a>
## agent types

- Classification: `dynamic-reference`
- Support: `supported`
- Signature: —
- Constraint: live values must not be copied into static contract data
- Dynamic reference owner: `agent-registry`
- Item shape: `{ name: string; description?: string }`
- Live lookup: `loadAgentRegistry`
- Live values are intentionally absent from this static reference.
