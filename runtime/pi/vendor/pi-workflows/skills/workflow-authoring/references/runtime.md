# Runtime authoring

Use this page for routine scripts. Open the generated capability index only when a signature, default, support boundary, or installed-version fact is missing here.

## Script envelope

Start with the only legal export: `export const meta = { name, description, phases?: [{ title, detail?, model? }] }`. Values are nonblank literals; declare only used phases and call `phase()` before each phase's work. The remaining body already runs inside an async function: write helpers as ordinary declarations; `export default` and other exports are invalid. Return the result explicitly.

The runtime supplies `agent`, `parallel`, `pipeline`, `checkpoint`, `phase`, `log`, `args`, `cwd`, and restricted `process.cwd()`. `args` is a deeply frozen copy, so copy before sorting or editing it: `[...args.items].sort()`. Imports, `require()`, filesystem modules, `Date.now()`, `Math.random()`, and no-argument `new Date()` are unavailable. The Node VM realm is implementation substrate, not a security boundary or public API. The script shares Pi's thread: a stretch of more than 5 seconds that awaits nothing (an endless loop) fails the run, and `__workflowLoopGuard` is a reserved name.

## Topology

- `parallel()` takes thunks, runs independent work, and preserves input order. Await the whole array before whole-set synthesis.
- `pipeline()` runs stages sequentially per item while items proceed concurrently. Each stage receives `(previousValue, originalItem, index)` and forwards `null` to the next stage, so guard missing coverage first.
- Loops, retries, and stopping rules are plain JavaScript around `agent()` calls; see [pattern selection](pattern-selection.md) and [focused recipes](focused-recipes.md).

## Data and failure

Call `agent(prompt, { label, schema? })`; it returns text, a schema-validated value, or recoverable `null`. Nonrecoverable limit and validation failures throw. Record each intended work ID before filtering. A `null` means missing coverage, never a negative finding.

In `parallel()` and `pipeline()`, a branch becomes `null` only through a recoverable workflow failure, as with `agent()`. A nonrecoverable failure, or an error thrown by the thunk or stage code itself (such as a `TypeError` from reading a field of `null`), rejects the whole call and fails the run unless the script catches it.

Await every `agent()`, `parallel()`, `pipeline()`, and `checkpoint()` promise, directly or through `Promise.all`. One the script never handles still finishes before the run ends: its checkpoint pauses the run, its nonrecoverable failure fails the run, and any other failure is only logged. A promise the script makes itself that rejects and is never handled, such as an un-awaited async helper that throws, fails the run.

Use `agent(prompt, { thread: "implementer" })` when the same subagent must receive a later follow-up with its complete conversation intact, such as implementer → separate reviewer → implementer revision. Reuse a thread name sequentially; never put same-thread calls in one `parallel()` batch. Later turns on one thread may change `model`, `tier`, or `agentType`: the transcript is kept and the next turn runs on the newly resolved model, which is intended for mid-loop escalation. Threads exist only during the current uninterrupted workflow invocation, cannot use worktree isolation, and restart from the beginning after pause/resume because threaded results are not journaled.

When JavaScript reads fields, pass a small plain JSON Schema. Schema noncompliance after repair throws and bypasses agent retries. Catch it only to return an explicit incomplete outcome without reading missing fields. Return objects, arrays, strings, numbers, booleans, and `null`—not functions, promises, cycles, `BigInt`, or runtime handles.

## Routing and support

Selector priority is explicit `model` > `agentType` model > `tier` > phase model > metadata model > inherited main model (when the `inheritMainModel` setting is on) > implicit `medium` > session default. An unavailable EXPLICIT selector (`model`, `agentType` model, `tier`, or phase model) throws instead of falling back — catch it if the script needs to degrade gracefully. Only the implicit route an untagged agent falls into — the default `medium` tier, or the inherited main model with `inheritMainModel` on — degrades to the session default when unavailable, with a one-time warning logged into the run. Use exact `model`, nonstandard `tier`, or `agentType` only when context supplies its name and purpose. Requested worktree isolation fails closed before the agent starts if Git cannot create it; it never falls back to the shared checkout. Each live execution owns a fresh worktree, including a resume miss; journal replay does not start an agent or create one. See [registry ownership](registry-ownership.md).

Generated entries marked `supported` are authoring API. `console` and whole-script Markdown fences are compatibility-only. VM realm facilities are internal. Active model routes and agent types are dynamic. Use `log()` in new scripts.
