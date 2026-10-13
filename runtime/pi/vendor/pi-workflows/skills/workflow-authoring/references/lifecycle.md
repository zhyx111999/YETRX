# Lifecycle, limits, and resume

## Bounds

Bound loops, fan-out, and semantic retries inside the script. Run limits come only from the workflow settings: `defaultConcurrency` (1–16), `defaultAgentRetries` (0–3, recoverable failures only), `defaultAgentTimeoutMs` (unset means no hard timeout), and `defaultMaxAgents` (default 1000). Neither a script nor a workflow call can override them. `defaultMaxAgents` counts logical `agent()` and `checkpoint()` calls; agent execution retries do not add slots, but every attempt of a script-level loop or semantic retry is a new `agent()` call and does. A run that stops at the cap finishes by raising the setting and resuming it. Token usage is measured and reported, never capped.

## Checkpoints

A checkpoint consumes an agent slot but no tokens. Every run starts in the background, so nothing prompts a human mid-run. `checkpoint({ kind, checkpointId, payload })` pauses the run until a response is stored for that exact checkpoint ID and the run resumes; it displays no UI. See [checkpoints](checkpoint.md) for the controller handoff and replay contract.

Checkpoint answers are journaled and can replay during an unchanged resume prefix. Do not describe checkpoints as guaranteed arbitrary forms or as remote steering.

## Retry and recoverable failure

Recoverable execution failures retry `defaultAgentRetries` times, then return `null`. Nonrecoverable failures throw without becoming `null`. A semantic retry written as a script loop is separate: each attempt is a new agent call, and the script must record and handle exhaustion itself.

Always retain `{ id, status, result }` or an equivalent ledger for each intended work unit. Filtering `null` before recording identity turns an execution failure into invisible missing coverage.

`AGENT_EMPTY_OUTPUT` (whitespace-only text from a schema-less call) is recoverable and retries like any other transient failure. Some models occasionally produce it on an otherwise-working first attempt; a fleet built on such a model needs `defaultAgentRetries` of 1–2 rather than treating one occurrence as a failed run. A `schema` call never trips this check — schema noncompliance is its own, nonrecoverable failure (see [serialization](#serialization)).

## Resume

Resume replays only the longest unchanged prefix of journaled calls. A call is matched by its position: the script body and each `parallel()` or `pipeline()` branch number their own calls in the order they make them, so which concurrent call finishes first never moves a call. Once one call is new, changed, or unusable, that call and every call made after it execute live; a branch that makes fewer calls than before also re-runs everything after its fan-out. Stable call order within each branch, prompts, labels, routing options, and inputs therefore matter, and no branch should depend on another branch of the same fan-out. Run concurrent chains of calls through `parallel()` or `pipeline()`, not `Promise.all` over async functions, whose calls after an await are numbered in whatever order they start. Semantic retry loops can cascade after an upstream miss.

To revise a paused or failed run, call `subagents_control` with `action: "resume"`, its `runId`, and the edited `script`; the run keeps its ID and its result is delivered when it finishes. Completed and stopped runs do not resume.

Only a call that finishes with a real result is journaled. A call whose every attempt was recoverable (including one that only ever produced `AGENT_EMPTY_OUTPUT`) contributes no journal entry, so resuming that run reruns exactly that call and every call made after it live; the earlier, already-succeeded prefix still replays from cache.

The runtime blocks common accidental nondeterminism, but this is not a security boundary. Pass timestamps, randomness, and external decisions through `args`.

## Serialization

The workflow's explicit return value crosses the tool boundary. Keep it JSON-serializable and preserve coverage ledgers in the returned data. Structured agent schemas must be plain JSON Schema. Schema success guarantees the downstream field shape expected by JavaScript; without a schema, treat output as text or `null`. A prompt that asks the model to "return JSON" does not change this — without `schema`, parse and validate that text defensively before reading a field, and ledger an unparseable result instead of reading `undefined` off it (see [defensive text parsing](focused-recipes.md)).
