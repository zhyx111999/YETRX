# Resource execution integration

This module owns custom command/script/http execution and MCP resource connections.
It does not change cmd, the SDK, agentexec, execution, service configuration, or
host DB/Engine tools. Integrate the execution supervisor before building.

## Stable entry points

- `server.ConfigureResourceExecution(*resourceexec.Config) error`: call before
  normal server startup/discovery. `nil` explicitly selects in-process execution.
  A configured worker failure never falls back. Startup snapshots use atomic
  replacement; argument/environment slices are copied. Live settings use
  `Runtime.SetMaxWorkers` instead, preserving all existing reservations and
  connections. Lowering capacity does not terminate live helpers; new admissions
  remain blocked until occupancy falls below the new limit. `MaxWorkers()` reads
  the current limit.
- `server.ServeResourceWorker(context.Context) error`: cmd must dispatch
  `resourceexec.WorkerCommand` (`resource-worker`) before config/PG/server startup.
  Stdout is reserved for framed RPC. The worker never opens the application's DB
  or reads user configuration.
- `server.LocalTool(tool.CoreTool) (string, json.RawMessage, error)`: inspect every
  fully assembled tool, following `UnwrapTool() tool.CoreTool`. Nonempty kind is a
  trusted `agentexec.LocalToolSpec`; empty kind falls through to SDK placement.
- `server.WorkerLocalTools() map[string]func(context.Context,json.RawMessage)
  (tool.CoreTool,error)`: constructors for `agentexec.WorkerConfig.LocalTools`.
  Copy/convert values to `agentexec.LocalToolConstructor` at registration.
- `preparedCustomTool.UnwrapTool()` returns the embedded CoreTool. There is no
  second placement decorator, accounting callback, or tool_usage.go modification.

Example placement:

```go
LocalTool: func(t tool.CoreTool) (*agentexec.LocalToolSpec, error) {
    kind, data, err := server.LocalTool(t)
    if err != nil { return nil, err }
    if kind != "" { return &agentexec.LocalToolSpec{Kind: kind, Data: data}, nil }
    return agentexec.SDKLocalTool(t)
},
```

The main server's `recordLocalToolStart(ctx, original)` belongs in
`agentexec.Config.OnLocalToolStart`. Child-local execution bypasses
`meterTool.Call`; without that integration usage is **not** recorded. The bridge
must pass the original assembled host tool and never re-execute its Call.
`guardedTool`, `overriddenTool`, and `meteredTool` must expose `UnwrapTool` (the main
integration owns these additions). Description/schema/default injection remains
with the bridge's existing metadata decorator.

## Budgets, admission and errors

`resourceexec.Config` fields:

- `Executable string`, `Args, Env []string`: trusted launcher settings. Empty
  executable uses the current binary; nil args select `resource-worker`; nil env
  inherits. Production should supply an explicit environment.
- `Limits execution.Limits`: custom/admin worker defaults 512 MiB, 1 CPU, 128
  processes. Every zero dimension takes its default; negatives/nonfinite CPU fail.
- `MCPLimits execution.Limits`: MCP worker defaults 384 MiB, 1 CPU, 128 processes.
- `MaxWorkers int`: the package default is 4; the application supplies 64 unless
  System Settings or the environment fallback specifies otherwise. Shared
  independent nonblocking capacity for MCP
  connections and one-shot custom helpers; each occupied slot remains charged
  until the entire process tree is reaped. Agent-local custom tools use the Agent
  process budget and do not consume this pool.
- `Start resourceexec.StartFunc`: optional host admission hook with exactly the
  execution.Start signature. It must enforce the supplied OS limits and return a
  supervised Process. No model arguments contain executable identity or limits.

Pool saturation returns `resourceexec.ErrBusy`; supervision failure is returned
unchanged. Main integration owns `agent.DeferredInfo.Err` and its role checks:
`wireAgentAugment` must propagate admission/limits failures and close previously
opened clients instead of logging and continuing. This module does not edit
server/assembly.go. Single-MCP duplicate tool names are rejected here; the main
assembly must reject cross-source duplicate names. LocalTool only handles custom
placement; compose SDK and interactive-shell placement in the main factory.

Each MCP helper owns one live client for initialization, paginated tools/list,
metadata and all calls. Cached proxy tools forward flags and permission checks;
MCP annotations do not override the SDK's fail-closed scheduling/approval policy.
stdio, Streamable HTTP (JSON or SSE replies), and legacy SSE support real progress
notifications. Session headers, custom auth headers/env, insecure TLS, capture and
batch raw-text calls are preserved. No env/header values or MCP stderr are logged.
HTTP errors omit request URLs/response snippets. A canceled call closes the entire
helper/connection, including descendants, so concurrent calls on that same client
also fail. Remote HTTP services are not OS children; their request is canceled.

`connectMCP` is shared by augmentation, startup discovery, page probes and
ScopeSentry batch sync. The custom-tool editor test endpoint uses a bounded
one-shot helper whenever isolation is configured. Deferred/Skill unlock handling
stays in the existing assembly layer.

## Validation and dependencies

Verified on Windows/amd64 Go 1.26.5 with actual execution.Start Job Objects, fake
stdio/HTTP/SSE MCP servers, a real Python interpreter, local HTTP/proxy/TLS
fixtures and a deterministic fake model provider. No production DB, external
model or real target was used. ARTEX_CONFIG explicitly referenced a nonexistent
file and ARTEX_PG_DSN was empty.

Tests cover PID separation, metadata/schema/flags/permissions, results/progress,
MCP protocol and tool errors, timeout/cancel, descendant termination, shared
connection use, parallel calls, duplicate rejection, busy/fail-closed startup,
script JSON stdin/scalar env, command/script defaults, cwd/session env, recording
proxy priority and CA trust, output capture and temporary script removal.
The tagged bridge test runs a real Agent child and checks local execution,
metadata/defaults and exactly one observation of the original host tool. The
main server's final DB usage callback and actual cmd deployment are integration
responsibilities; this test uses an observation counter and unwrap-capable test
decorators rather than altering their source.

Commands (with dependencies available):

```text
go test -race -tags=resourceexec_bridge ./resourceexec ./server -run 'Test(Resource|MeterTool|EnsureSchema|DetectPython|Custom|MCP|RenderTemplate|ScalarStr|ShellQuote|ExecPython|RunHTTPTool|HTTPToolTruncates)' -count=1 -timeout=150s
go vet -tags=resourceexec_bridge ./resourceexec ./server
```

The race fixture containing helper + fake MCP + descendant Go test binaries uses
an explicit 768 MiB MCP memory budget; production defaults remain 384 MiB. OS
limit unavailability is never an in-process fallback. Linux/cgroup execution was
not exercised in this Windows run. No DB migration/live suite was run.

Dependency versions used for validation:

- execution: supervisor worktree based on
  `292d500a48d370c552e166a769966ca4e9678e54`, execution sources were uncommitted.
  Normalized production-source aggregate SHA256:
  `eff8bd503dc9297ebb8322558a34ba326d07ab09e305f203c5424506acaeacb4`.
- agentexec: `184359b54189982621a79bcb2762f3474468dee2` plus its
  `agent/runtime.go`, copied only temporarily for the tagged bridge test.
  Normalized agentexec production-source aggregate SHA256:
  `1f7db6f4b200797403cd072c202599082a0e993d6bddefa8c3678662d2e7bcf4`.

Aggregate recipe: sorted non-test `*.go`, concatenate filename, NUL, file bytes
with CRLF normalized to LF, NUL; SHA256. Temporary dependency copies are excluded
from this commit and removed after validation.
