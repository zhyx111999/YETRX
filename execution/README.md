# execution integration and validation

This package is independent of agent/server/cmd and application initialization.
No application configuration, database, model provider, or logging backend is
loaded by the tests. The public API includes:

```go
type Handler func(context.Context, string, json.RawMessage,
    func(json.RawMessage) error) (json.RawMessage, error)

func NewPeer(io.ReadCloser, io.WriteCloser, Handler) *Peer
// Peer: Call, Stream, Close, Done, Err
func Start(context.Context, string, []string, []string, Limits, Handler) (*Process, error)
// Process: Peer *Peer, PID(), Wait(), Close(), StderrTail()
type Limits struct { MemoryBytes int64; CPUs float64; Processes int }
func InitCgroup() error
func ConfigureBudget(Limits) error
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
* Without an aggregate budget, each subsequent Linux Start creates a sibling
  `exec-<pid>-<sequence>` cgroup,
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
Job startup, and queried CPU/memory/KILL_ON_CLOSE settings. Per-tree memory limits are queried; the aggregate test below also exercises
actual commit denial. CPU tests check the installed cap rather than using noisy
wall-time benchmarks.

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

## Aggregate execution budget

Call `InitCgroup()` then `ConfigureBudget(Limits{MemoryBytes: 3 << 30,
CPUs: 4, Processes: 1024})` before the first `Start`. ConfigureBudget excludes
this supervisor/website and covers every subsequent runner and descendant,
even when a runner has zero local Limits. Zero dimensions are unlimited;
all-zero configuration requires no OS support. Identical configurations are
idempotent. Changing an installed budget (including disabling it), or enabling
one after any runner has been prepared, returns `ErrLimitsUnavailable`, even
after runners close. Invalid negative/nonfinite limits return validation errors.
Failed OS setup returns `ErrLimitsUnavailable`; callers must handle failures.
No aggregate runtime hints or automatic concurrency reductions are applied.

Linux creates an empty `execution-workers` inner node below the delegation root,
writes memory.max/cpu.max/pids.max, and enables memory/cpu/pids subtree controllers.
`execution-manager` stays its sibling; every new exec-* goes below workers.
Repeated InitCgroup preserves this parent. Existing exec-* nodes or an existing
workers node cause setup to fail without adopting or moving any processes.
The aggregate memory.oom.group is 0 and each runner's is 1, so aggregate OOM
selects a runner tree instead of killing all runners as one group. The workers
node remains for the delegated service lifetime; systemd removes it on teardown.

Windows retains a shared, non-inheritable parent Job until supervisor exit,
with KILL_ON_JOB_CLOSE and aggregate commit-memory/CPU/process caps. Each child
is created suspended, assigned first to that parent then to its individual Job,
and resumed only after both assignments succeed. Closing a runner closes only
its own Job. Nested CPU rates account for the parent's installed CPU capacity.
Other platforms reject nonzero budgets with `ErrLimitsUnavailable`.

The Windows `TestWindowsAggregateBudget` queries limits and membership, verifies
the supervisor is excluded, and runs two actual runners: each fits individually,
but combined process creation and native committed-memory requests exceed the
shared cap and are refused. Closing one leaves its sibling alive and frees memory
budget for it. Memory allocation bypasses Go hints. Race-runtime overhead is
included in the test budget. Startup-state tests use isolated supervisors.

Linux `TestLinuxAggregateBudget` is opt-in with `EXECUTION_TEST_CGROUP=1`.
Use the disposable systemd-run invocation above, optionally adding
`-test.run=^TestLinuxAggregateBudget$`. It checks two runner parent paths,
aggregate and local memory/CPU/pids values, subtree controllers, empty workers
node, OOM policies, the manager sibling, InitCgroup reentry, configuration
immutability, and closing one runner without affecting the other. The outer
test harness first enters execution-manager, then launches an isolated test
supervisor there. No unrelated process is moved. **This Linux test has not been
executed on the Windows development host; cross-compilation is not acceptance.**

Production callers can use 512 MiB/1 core/128 tasks per Agent and
384 MiB/1 core/128 tasks per helper, with 8 Agents/16 helpers as their scheduling
defaults. The aggregate 3 GiB/4-core/1024-task budget is shared, not the sum of
local maxima; Linux tasks include threads. This package never silently lowers
tool concurrency to fit a process cap. OS admission failures remain explicit.
