# runtime/pi — YETRX bridge to the real pi SDK

`node runtime/pi/bridge.mjs` embeds `@earendil-works/pi-coding-agent@1.1.0` in a long-lived
process. The Go host talks to it over stdin/stdout; every LLM call goes to the Go relay.

```
npm ci            # in runtime/pi; Node >= 22.19, install scripts are disabled by .npmrc
npm test          # no real model, no network: a local fake OpenAI server stands in for the relay
```

## Wire format

Strict JSONL: one JSON object per line, split on **LF only** (CR before LF is tolerated;
U+2028/U+2029 inside strings are not boundaries). stdout carries nothing but records;
diagnostics go to stderr, and anything else that writes to stdout is redirected to stderr.
Every command is answered exactly once and the answer carries the command's `id`.

### Commands (stdin)

| command | fields | answer |
|---|---|---|
| `init` | `id, cwd, agentDir, sessionFile, baseUrl, apiKey, models:[{id,name,contextWindow,maxTokens}], modelId, thinkingLevel, history?, tools?, trustProject?` | `{type:"ready",id,tools,skills,modelId}` or `{type:"response",id,ok:false,error}` |
| `prompt` | `id, message, streamingBehavior?` | `{type:"response",id,ok}` when the run has finished |
| `abort` | `id` | `{type:"response",id,ok}` once the run is idle |
| `set_model` | `id, modelId, thinkingLevel?` | `{type:"response",id,ok}` |
| `set_models` | `id, models, modelId, thinkingLevel?` | `{type:"response",id,ok}` |
| `shutdown` | `id` | `{type:"response",id,ok}`, then exit 0 |

Failures are `{type:"response",id,ok:false,error}`. Closing stdin (or SIGTERM/SIGINT) shuts down the
same way as `shutdown`.

* `init.history` (pi-compatible `user` / `assistant` / `toolResult` messages) is imported with
  `SessionManager.appendMessage` **only when `sessionFile` does not exist yet**. An existing file is
  never imported into. The whole history is validated before anything is written.
* `prompt` answers `ok:false` when the run ended in a provider error (after pi's own retries) and
  `ok:true` otherwise, including an aborted run. If a run is already streaming (for example one a
  background result started) the message is queued behind it (`streamingBehavior`: `followUp`
  default, or `steer`). Input is never expanded as a prompt template or slash command.
* `set_models` re-declares the `yetrx` provider's model list in place. The session, its history and
  every background workflow keep running; `modelId` must be in the new list. See *Limits*.
* A second `init` ends the live session (extensions get `session_shutdown`, background runs are
  paused) and opens the new one in the same process. `init.agentDir` must equal
  `PI_CODING_AGENT_DIR` when that is set.

### Records (stdout)

`ready`, `response`, and `{type:"event", event}`. Events are pi's own session events, converted by
pi's own `toJsonEvent`: `message_update` is delta-only (no cumulative `message`, no `partial`), so use
`message_end` for complete messages. Tool lifecycle (`tool_execution_*`), `turn_*`, `agent_end` and
`agent_settled` are unchanged; `agent_settled` arrives before the `prompt` response.

Background workflow results are delivered by the vendored extension as `workflow-result` custom
messages (`message_start`/`message_end` with `role:"custom"`), followed by a spontaneous
`agent_start … agent_settled` run, with **no command and no response** — consume events for the life
of the process.

Bridge-originated events (RPC-mode shapes): `extension_ui_request` (`notify`, `setStatus`,
`setTitle`, text `setWidget`; dialogs are auto-cancelled and marked `autoCancelled`),
`extension_error`, `extension_shutdown_request` (reported, not obeyed).

## What runs

* Real SDK objects only: `createAgentSession`, `SessionManager.open(sessionFile)`, a file-backed
  `SettingsManager` in `agentDir`, `DefaultResourceLoader`, `bindExtensions`.
* Provider `yetrx` (`openai-completions`) on an in-memory `ModelRuntime`; model ids are sent as-is;
  the token is a runtime credential and is never written to disk, a session or a log.
* Tools: `read bash edit write` plus the workflow extension's `subagents`, `subagents_control`;
  `init.tools` may add `grep find ls`.
* Extension: `vendor/pi-workflows` (pi-workflows-lean, see `vendor/VENDOR.md`) and its three skills.
  No other extension, no web-search extension, no Tavily.
* System prompt: one short built-in text plus pi's cwd / context-file / skill sections. `SYSTEM.md`,
  `APPEND_SYSTEM.md`, prompt templates, themes and `~/.agents/skills` are not read.
* Workspace trust: `<cwd>/.pi` extensions, settings and skills are **not** loaded unless
  `init.trustProject` is `true`. `AGENTS.md`/`CLAUDE.md` context files always load.
* Subagents inherit the main model: the bridge records it as the agent directory's default model.

## Limits and decisions

* `set_models`: pi has no remove/replace API for a running session; re-registering a provider is the
  documented way and takes effect immediately. Subagents already running keep the model they started
  with; the main session switches only if its model changed or was edited; a model that is not in the
  list can no longer be selected. `baseUrl` and token cannot change (no field for them).
* One live session per process. Background runs belong to it and are paused when it is replaced or
  the process exits (resume with `subagents_control`). Use a process per concurrently live session.
* Workflow run records and settings live in `os.homedir()/.pi/workflows` (hard-coded by the package,
  not under `agentDir`). Start the process with `HOME`/`USERPROFILE` pointing at a private directory
  to isolate them; the `bash` tool inherits that environment.
* The built-in `deep-research` workflow uses the package's own `web_search` (scrapes Bing HTML) and
  `web_fetch` (Node `fetch`). They run inside this process and reach the internet directly, not through
  the relay, and are best-effort. They are not given to the main agent.
* Pi's built-in providers still exist; if the process environment holds their API keys (for example
  `OPENAI_API_KEY`) the bridge logs a warning, because a workflow `model:` tag could then bypass the
  relay. Start the process with a scrubbed environment.
* The task-panel widget (a TUI component) has no wire form; one-line run progress is available through
  `setStatus` events and `subagents_control`.
* The shutdown path waits 200 ms before exiting (Windows libuv assertion on closing sockets).
