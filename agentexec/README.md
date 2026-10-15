# Agent process adapter

This package uses the separately maintained `execution` supervisor. It does not
implement transport, OS limits or process-tree control. The integration tree must
include `execution` before building this commit.

## Host integration

```go
// In cmd, BEFORE ordinary application/config/server/PG startup:
if len(os.Args) > 1 && os.Args[1] == agentexec.WorkerCommand {
    err := agentexec.ServeWorker(context.Background(), agentexec.WorkerConfig{
        LocalTools: constructors,
    })
    // Return/exit here; never continue into server startup.
}

// During server setup, after tool construction/decoration is available:
agent.SetSessionFactory(agentexec.NewFactory(agentexec.Config{
    Executable: executable,
    Env: workerEnvironment,
    Limits: limits,
    LocalTool: placement,
    OnLocalToolStart: recordLocalToolUsage,
}))
```

`RuntimeSession` exposes `Messages`, `Prompt`, `Resume`, `Close`.
`SetSessionFactory` is synchronized; nil explicitly selects the original in-process
SDK. A nonnil factory error is returned, never replaced with in-process execution.
All existing session creation paths under `agent/`, including connection testing,
now use the factory. Server/cmd integration is deliberately outside this commit.

`Config.Args == nil` selects `agent-session-worker`; an explicit args slice is
passed to the executable. `Env == nil` inherits the environment. The caller must
supply production limits/environment policy. `execution.Start` enforces the OS
limits before work and owns tree termination. Tests use Windows Job limits of
768 MiB, one CPU, and 32 processes. Linux requires the supervisor's usable cgroup
configuration; this adapter does not substitute advisory limits.

## Tool placement and usage

`LocalToolSpec{Kind string, Data json.RawMessage}` is trusted factory output.
`Config.LocalTool func(tool.CoreTool) (*LocalToolSpec, error)` receives the final
assembled host tool. Nil spec means host. With no placement callback, SDK
DefaultTools names are reconstructed in the child; other tools stay host-side.
A custom callback can compose with `SDKLocalTool(t)` for the SDK subset.

`WorkerConfig.LocalTools map[string]LocalToolConstructor` registers constructors.
A constructor has signature
`func(context.Context, json.RawMessage) (tool.CoreTool, error)`; its context lasts
for the worker process lifetime. Its returned tool must have the assembled name.
Unknown kinds fail initialization. The reserved `sdk` kind reconstructs SDK
DefaultTools. Descriptions, prompt, schema, flags, permissions and raw-input
metadata come from the assembled host tool. Local execution reapplies
`agent.DecorateTool`, including scalar parameter default injection.

`OnLocalToolStart func(context.Context, tool.CoreTool)` runs on the host before
child-local `Call`, synchronously over RPC. It receives the original assembled
host tool and original Prompt identity; it must only record usage, never execute
`Call` again. Both `guardedTool` and `overriddenTool` expose
`UnwrapTool() tool.CoreTool`; server can traverse that interface to locate its
metered wrapper. No result observer or result-based counting is retained. A
child crash after the start callback still leaves the count recorded. Callbacks
must be safe for concurrent tools, as the original tools were.

MCP/custom/script tools without specs remain host calls. Resource-processes must
supply their independent constructors/helper and server preparation changes.
This package alone does not isolate those execution paths. Background task
manager/tools and SDK file/Bash calls run in the child. Host ToolContext retains
working directory, tool-use ID, output capture limits/path, Bash environment,
progress delivery and tool deadlines. `AgentID` is empty, matching agentcore's
QueryInput. `ToolContext.Tasks` cannot cross a process boundary: host tools that
require that concrete manager must be given a local spec. Existing application
host tools do not use it.

## State and ordering

Provider and QueryDeps callbacks remain in the host (including llmrec,
sidequestion, route/rate/quota/identity wrappers). Child provider calls are pulled
one event at a time. Model/compactor/boundary callbacks are pumped on the host
Prompt consumer thread. The next provider event cannot advance before the prior
harness event's consumer returns. This preserves capture flush and single-response
OnResponseEnd order without putting application types on the wire.

The ba855428 response-boundary agent implementation is included and adapted to
`newControlledSession(ctx, opts, control) (RuntimeSession, *steeredSession, error)`.
OnPrompt runs only after child Prompt accepted its message and the host snapshot
was synchronized. Drained batches use the same acceptance path. Cancellation
before acceptance does not acknowledge them. Host history snapshots update at
Resume, Prompt acceptance, before each next model request (before ContextView
projection), terminal result and completed Prompt; history is not copied per token.
SDK transcript persistence order is unchanged: assistant/tool turns are committed
by the SDK loop, so OnResponseEnd is an activity boundary and does not claim that
pending tool results have already been written to the transcript.

The transcript directory is derived through the public `Store.MainPath` API;
child uses that shared directory for Store/Resume. Host must serialize concurrent
writers to the same session ID; Store's mutex is not an inter-process file lock.
Skill/AskUser stay host tools. UnlockSet additions are synchronized after host
tools and before every Prompt, including after Resume history reconstruction.
TodoWrite commits the original host TodoStore in the pre-tool phase, then the
child's SDK mirror maintains its normal reminder behavior. A fresh child receives
the current host TodoStore. No background polling of external TodoStore mutations
is performed during one active Prompt.

Errors are explicit wire envelopes, including Terminal.Err. Host-produced errors
retain session-owned references and recover the same error object on return to
the host, preserving steering/quota errors.Is. Child-native errors carry text and
cancellation/deadline categories. RPC errors remain explicit transport errors.
Close waits only for the supervised process. An uncooperative host provider can
leave its own callback goroutine pending until it cooperates; Close never joins
that iterator. Uncooperative child tools are terminated by the supervisor.

## Options and limits

Every current Options field is classified explicitly. Unknown future SDK fields
(and QueryDeps fields) fail closed. Plain options include system segments/cache
boundary, compaction configuration, settlement/budgets, working/output directories,
environment, all web search/fetch settings, concurrency and streaming mode.
Hooks (including optional lifecycle hooks), approval, reminder, NewID, OnWarn,
Compactor and ContextView use the host broker. Noa uses the same host Compactor,
ContextView and assembled Compress tool.

Non-nil `Options.Plan` and `Options.Memory` are explicitly rejected together by
name; these are not used by the current production agent options. Provider is
required. The process bridge is runtime/resource isolation, not a filesystem or
network sandbox; child SDK tools retain the configured working directory and
normal SDK access. Large messages remain subject to execution's frame/backlog
limits. Pulling each event adds IPC overhead; throughput has not been benchmarked.

## Validation

Tests use fake providers and actual supervised test-executable children, with
`ARTEX_CONFIG` pointing at a nonexistent path and `ARTEX_PG_DSN` empty. No real
model or user database is needed. Coverage includes streaming/nonstreaming,
child Bash parent PID and defaults, host ctx/ToolContext/progress/deadlines,
permission deny/approval/hooks, Resume/history/Skill unlock/TodoStore, AskUser,
settlement/budget, error identity, ContextView snapshots and Noa smoke setup,
response flush/steering/acceptance/canceled batches, local pre-call metering,
cancel/crash/uncooperative child and provider Close behavior. Noa's actual model
compression and configured real web-search backends are not integration tested.

Validated in this worktree (Windows, controlled providers, temporary uncommitted
copy of the supervisor):

```text
ARTEX_CONFIG=C:/nonexistent-agentexec-test-config ARTEX_PG_DSN= go test ./agentexec ./agent -count=1 -timeout=120s
ARTEX_CONFIG=C:/nonexistent-agentexec-test-config ARTEX_PG_DSN= CGO_ENABLED=1 go test -race ./agentexec ./agent -count=1 -timeout=180s
```

Both passed. The copied `execution/` is removed before commit and is not part of
this module's changes. Re-run after merging the supervisor and server/resource
integration; this branch alone intentionally does not build without `execution`.
