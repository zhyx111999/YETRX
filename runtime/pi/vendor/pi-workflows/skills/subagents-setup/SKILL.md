---
name: subagents-setup
description: Install and configure pi-workflows-lean subagents. Use when the user wants to set up subagents, change subagent models or tiers, concurrency, retries, timeouts, max agents, or the progress panel, define agent roles, or save a reusable workflow.
metadata:
  version: "3.13.0-lean.1"
---

# Subagents setup

Edit these files for the user. Every file is optional; invalid values are ignored.

**Install:** `pi install git:github.com/zhyx111999/pi-workflows-lean`, then `/reload`.

**`<key>`:** project folder name in lowercase with runs of characters outside `a-z0-9._-` turned into `-`, then `-`, then the first 12 hex digits of SHA-256 of the absolute project path. After a first run it exists under `~/.pi/workflows/projects/`.

## Model tiers

`~/.pi/workflows/model-tiers.json`; `~/.pi/workflows/projects/<key>/model-tiers.json` overrides per tier.

```json
{ "tiers": { "small": "<provider>/<model>:low", "medium": "<provider>/<model>", "big": "<provider>/<model>:high" } }
```

Suffix sets thinking: `off|minimal|low|medium|high|xhigh|max`. An empty value voids the file. Untiered subagents use `medium`.

## settings.json

Later files win per key: `~/.pi/workflows/settings.json`, `<project>/.pi/workflows/settings.json`, `~/.pi/workflows/projects/<key>/settings.json`.

A change in an earlier file is shadowed while a later file sets the same key. `/workflows-progress` saves globally and, for keys either project file sets, to the project override.

| Key | Default | Meaning |
|---|---|---|
| `defaultConcurrency` | 8 (≤16) | parallel subagents |
| `defaultAgentRetries` | 0 (≤3) | retries after recoverable failures |
| `defaultAgentTimeoutMs` | none | ms per subagent; `null` clears an earlier file |
| `defaultMaxAgents` | 1000 | subagents per run |
| `progressPanelMode` | `"compact"` | or `"detailed"` |
| `progressPanelMaxAgents` | 8 | agents per phase when detailed |
| `persistAgentSessions` | false | keep subagent transcripts as Pi sessions |
| `inheritMainModel` | false | untiered subagents use the main model |
| `deliveredResultMaxChars` | 400 | cap on a result's JSON fallback |
| `excludeSubagentTools` | [] | extra tools denied to subagents; `[]` clears an earlier file |
| `providerMiddlewareExtensions` | [] | trusted provider/auth extensions for subagents |

## Agent roles

`<project>/.pi/agents/<name>.md` wins over `~/.pi/agent/agents/<name>.md`. Scripts use `agentType: "<name>"`. All fields are optional; `name` defaults to the file name.

```markdown
---
tools: read, bash
disallowedTools: write
model: <provider>/<model>
thinking: high
isolation: worktree
---
Role instructions.
```

## Saved workflows

`~/.pi/workflows/saved/<name>.json`; `~/.pi/workflows/projects/<key>/saved/<name>.json` wins.

```json
{ "name": "<name>", "description": "One line", "script": "<workflow script>" }
```

`name` equals the file name exactly (case included), at most 128 characters, with no whitespace, control or format characters (such as bidi controls), slashes, `<>:"|?*`, trailing dot, or Windows device name such as `CON` or `COM1`; other files are skipped with a warning. Write `script` per the workflow-authoring skill; `"scriptPath": "<file>.js"` in the same folder can replace it. Optional `"toolset": "web-research"` gives its runs web tools. Optional `"parameters": { "<key>": { "default": <value>, "description": "..." } }`: `/<name> key=value …` passes `args`, and a `default` fills a key the command leaves out. Run with the `subagents` tool's `name` and `args`.

## When changes apply

- Next `subagents` run: tiers, roles, saved scripts, run limits.
- Live: progress panel; `deliveredResultMaxChars` on the next result.
- After `/reload` or a new session: the other settings, run limits for slash-command runs, and a new saved workflow's `/<name>`.
- After restarting Pi: plugin updates. `/reload` keeps the loaded build and warns that it changed; runs active across the restart pause and resume from `/workflows`.
