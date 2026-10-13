---
name: workflow-patterns
description: Argument shape for the built-in deep-research workflow, runnable via the subagents tool's name input. Not for authoring a new workflow script — see workflow-authoring.
metadata:
  version: "3.13.0-lean.1"
---

# Built-in workflow patterns

This fork ships one built-in workflow pattern, `/deep-research`. It is also
reachable from the `subagents` tool: call it with `name` set to `deep-research`
and `args` matching the shape below, instead of writing an equivalent script.

A project or user saved workflow of the same name always takes precedence
over a built-in of that name — on the slash command, too.

A script cannot start another workflow; run `deep-research` (or a saved
workflow) through the tool's top-level `name` input.

## Patterns

| `name` | When to reach for it | `args` |
| --- | --- | --- |
| `deep-research` | Research a question across the web and return sourced claims | `{ question: string, angles?: number }` — `angles` is the number of distinct search queries: an integer from 1 to 16, default 4. Any other value is rejected before the run starts |

## Result

`deep-research` returns `{ question, planning, results }`. `planning` is
`{ requestedAngles, plannedQueries, fellBackToQuestion }`: fewer planned
queries than requested, or `fellBackToQuestion: true` when the planner failed
and the question itself became the single query, means narrower coverage.
`results` has one entry per planned query, in order:

- `{ query, status: "ok", sources: [{ url, claims }] }` — the query was
  researched. Empty `sources` means nothing usable was found.
- `{ query, status: "failed" }` — its research subagent did not finish, so the
  query is not covered either way. Check the run's subagents for the cause.

Sources are raw; check the claims before relying on them.

## Example

```json
{ "name": "deep-research", "args": { "question": "What are the tradeoffs of X vs Y?" } }
```

This is a `subagents` tool call, not a script — omit `script` entirely. The run
starts in the background exactly like the slash-command form; run limits come
from the workflow settings, as for any run.

## Writing a new workflow instead

If the request is not deep-research, pass independent work as `tasks`, or
author a `script` for multi-step work — see the workflow-authoring skill.
