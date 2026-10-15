# execution integration and validation

This package is independent of agent/server/cmd and application initialization.
No application configuration, database, model provider, or logging backend is
loaded by the tests. The public API is unchanged:

```go
type Handler func(context.Context, string, json.RawMessage,
    func(json.RawMessage) error) (json.RawMessage, error)

func NewPeer(io.ReadCloser, io.WriteCloser, Handler) *Peer
// Peer: Call, Stream, Close, Done, Err
func Start(context.Context, string, []string, []string, Limits, Handler) (*Process, error)
// Process: Peer *Peer, PID(), Wait(), Close(), StderrTail()
type Limits struct { MemoryBytes int64; CPUs float64; Processes int }
func InitCgroup() error
```

## Startup contract

* Child stdin/stdout carry only the length-prefixed JSON protocol; send diagnostics
  to stderr. Create its `NewPeer(os.Stdin, os.Stdout, handler)` and wait on `Done()`.
* Handler must reject unknown methods with `MethodNotFound(method)`. A nil handler
  rejects all methods. Handlers run concurrently and must honor their context.
  `Stream` invokes callbacks sequentially on the caller's goroutine; callbacks
  may call the same Peer, including while the original handler emits more events.
  Do not hold application locks across RPC if a reverse RPC needs those locks.
* `Limits.CPUs=1` means one logical core, including on Windows. Zero leaves only
  that dimension unlimited. Linux `Processes` limits tasks (threads included),
  so leave runtime thread headroom. Windows counts processes.
* Windows always creates the child suspended, assigns it to a Job Object, then
  resumes the initial thread. Job memory is aggregate committed memory; CPU is
  a hard cap relative to all active logical processors; active process count and
  KILL_ON_JOB_CLOSE are enabled as requested. Nested jobs are supported where
  the enclosing job permits them. Assignment/resume failure kills and reaps the
  suspended process and returns `ErrLimitsUnavailable`.
* Linux production: set `Delegate=yes` on the service and call `InitCgroup()` in
  the supervisor before starting any runners. It moves only the calling process
  to `execution-manager`, enables delegated memory/cpu/pids controllers on the
  parent, and is idempotent. It checks controllers and other root PIDs before moving
  itself. It does not move other processes or sweep old jobs. A concurrent
  process joining during initialization can still make controller enablement
  fail after the move; do initialization before admitting concurrent work.
  An error is fatal to deployment with limits. A preexisting process in the
  delegation root can prevent enabling controllers.
* Each subsequent Linux Start creates a sibling `exec-<pid>-<sequence>` cgroup,
  configures limits, then uses clone3(CLONE_INTO_CGROUP). There is no spawn-then-
  move interval. Unsupported clone3, missing controllers, permissions, and limits
  below OS resolution return `ErrLimitsUnavailable`; no unlimited retry occurs.
  Production Linux 7.0 supports these interfaces. Other deployments need cgroup
  v2 and clone3 support (including permission through container/seccomp policy).
* Without InitCgroup, zero-limit Linux Start uses only process-group containment;
  descendants that call setsid can escape it. Nonzero limits still require a
  writable suitable cgroup. Full descendant cleanup requires InitCgroup.
* `InitCgroup()` is a no-op outside Linux. Other Unix platforms use process
  groups and reject nonzero limits. Other platforms compile and provide root
  process termination only; they also reject nonzero limits.

## Failure and resource behavior

Cancellation closes protocol pipes, requests termination on Unix, and after
three seconds kills the tree. Root exit also kills remaining descendants. Wait
joins root-exit/lifetime errors with cleanup errors; Close returns cleanup errors
and is idempotent. Both expose cleanup failures as `*CleanupError` via
`errors.As`, preserving the underlying cause with Unwrap. Linux uses recursive cgroup kill plus process-group/Pdeathsig
assistance. The supervisor's launch thread remains pinned until cleanup, then
is unlocked. Zombies are dead, though their final reap belongs to their new
parent. Cgroup directories are removed after tasks die. Windows waits until the
job reports zero active processes.

A 16 MiB frame limit is checked against the length header before receive-buffer
allocation. The 64 MiB per-peer backlog budget includes inbound parameters and
methods, pending calls and encoded parameters, queued events, replies/errors, and events/replies waiting to write;
items have an additional 1 KiB accounting cost. Queued event references are
cleared when consumed. An overflowing stream fails with ErrBacklog; excess
inbound calls receive busy. There is one writer, one reader, and one fixed
64-entry control-message queue; control overflow closes the connection.
Parameter/frame encoding is serialized. A blocked write cancellation closes the
connection because partial frames cannot be reused. Provided ReadCloser and
WriteCloser must support Close interrupting blocked I/O (OS pipes/net.Conn do).

The budget covers retained protocol data, not total process RSS: caller-owned
values, handler/callback state, custom JSON marshalers, JSON encoding scratch,
and runtime overhead are outside it. Use OS limits for total process resources.
A handler that ignores cancellation or a callback that blocks forever cannot be
forcibly interrupted as a Go goroutine; Process cancellation can terminate its
runner. No payload/key is logged. Panic diagnostics are a fixed message, and the
stderr tail is at most 16 KiB (invalid UTF-8 is replaced with `?`). The caller
chooses whether and where to expose stderr, which can contain child diagnostics.

These are ordinary process reliability controls, not a hostile-code sandbox.
Same-user code with access to the delegated parent can change cgroups. On Linux,
Pdeathsig kills the immediate child when the supervisor dies; abrupt supervisor
SIGKILL can leave descendants until systemd cleans the service cgroup. Keep
systemd `KillMode=control-group`. InitCgroup intentionally does not kill processes
left by other supervisors. Windows KILL_ON_JOB_CLOSE covers supervisor death.

## Local tests (Windows)

From the repository root in PowerShell:

```powershell
$env:ARTEX_CONFIG = 'C:\nonexistent-execution-test-config'
$env:ARTEX_PG_DSN = ''
go test ./execution -v -count=1 -timeout=120s
go test -race ./execution -count=1 -timeout=120s
go vet ./execution
```

Helpers execute the current test binary with `-test.run=^TestProcessHelper$` and
an explicit marker, then use os.Exit so the testing harness cannot print PASS
onto protocol stdout. No application binary is used. Every runner has a bounded
lifetime and registered cleanup. Temporary PID files use t.TempDir.

Coverage includes concurrent bidirectional calls, ordered streaming and nested
callbacks, cancellation reaching the remote handler, blocked writes (net.Pipe
and real process pipes), large frames, truncated frames, EOF, unknown methods,
bounded replies/events/control queues, short writes, actual process crashes and
nonzero status, one runner surviving another's crash, bounded stderr, stubborn
child/grandchild cleanup, actual Windows active-process-limit denial, nested
Job startup, and queried CPU/memory/KILL_ON_CLOSE settings. Windows memory is
queried rather than exhausted; CPU tests check the installed cap rather than
using noisy wall-time benchmarks.

## Linux deployment acceptance (must run on target)

Linux hard limits have **not been executed on the Windows development host**.
Build the test binary on Linux or cross-compile it with `GOOS=linux`,
`GOARCH=amd64`, `CGO_ENABLED=0`, `go test -c ./execution -o <path>`. Place it where
Yezi can execute it. Run the binary directly as the service process, so `go test`
or a shell does not remain in the same cgroup root:

```sh
sudo systemd-run --unit=execution-integration --wait --collect --pipe \
  -p User=Yezi -p Delegate=yes -p KillMode=control-group \
  /usr/bin/env ARTEX_CONFIG=/nonexistent-execution-test-config ARTEX_PG_DSN= \
  EXECUTION_TEST_CGROUP=1 /absolute/path/execution-linux.test \
  -test.v -test.timeout=180s
```

This opt-in modifies only that disposable service's delegated cgroup. The
Linux integration test checks InitCgroup/idempotency, the actual child cgroup
and memory.max/cpu.max/pids.max values, memory exhaustion ending in SIGKILL,
pids.max=1 refusing Go runtime threads, cgroup removal, and cancellation of a
SIGTERM-ignoring child plus a setsid grandchild. The ordinary tests also run.
Without EXECUTION_TEST_CGROUP=1 this integration test explicitly skips; a skip
is not evidence that production limits work. Delete the copied test binary
when acceptance is complete. Do not run the opt-in test inside the live service.

Linux/Darwin cross-compilation proves compilation only. Darwin resource limits
are unsupported and neither Darwin execution nor Linux limits were tested here.

## Aggregate production budget handoff

Per-runner Limits do not impose an aggregate budget across runners. Production
must add an aggregate parent before admitting runners. The Linux private state
is `cgParent string`, protected by `cgMu`, in cgroup_linux.go. InitCgroup sets it
to the delegation root; newCgroup creates exec-* children there. The integrating
supervisor can create/configure execution-workers below that root, then update
cgParent under cgMu before any Start calls. execution-manager remains its sibling.
An aggregate ConfigureBudget API is outside this commit and is being implemented
by the integrating supervisor; do not treat per-runner limits as website resource
reservation.
