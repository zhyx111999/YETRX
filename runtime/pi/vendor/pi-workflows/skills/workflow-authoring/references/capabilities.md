<!-- GENERATED from WORKFLOW_CAPABILITY_CONTRACT; do not edit by hand. -->
# Workflow capability index

Contract format: `2.1.0`<br>
Contract content / skill / extension: `3.13.0-lean.1`

This compact generated index covers supported runtime globals and `subagents` tool inputs. For constraints, compatibility behavior, internal boundaries, and dynamic-reference ownership, follow the [exhaustive generated facts](capability-details.md).

## Supported capability index

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
