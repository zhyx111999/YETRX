//go:build windows

package execution

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestWindowsBudgetWorker(t *testing.T) {
	if os.Getenv("EXECUTION_BUDGET_WORKER") != "1" {
		return
	}
	peer := NewPeer(os.Stdin, os.Stdout, func(ctx context.Context, method string, params json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
		if method != "allocate" {
			return echoHandler(ctx, method, params, emit)
		}
		var size uintptr
		if err := json.Unmarshal(params, &size); err != nil {
			return nil, err
		}
		// Native committed memory bypasses all Go runtime resource hints. Retained
		// until process exit so the shared Job must account for both runners.
		_, err := windows.VirtualAlloc(0, size, windows.MEM_RESERVE|windows.MEM_COMMIT, windows.PAGE_READWRITE)
		return json.Marshal(err == nil)
	})
	<-peer.Done()
	os.Exit(0)
}

func TestWindowsAggregateBudget(t *testing.T) {
	if !isolatedBudgetTest(t) {
		return
	}
	ctx := testContext(t)
	limits := Limits{MemoryBytes: 2 << 30, CPUs: 4, Processes: 2}
	if err := ConfigureBudget(limits); err != nil {
		t.Fatal(err)
	}
	if err := ConfigureBudget(limits); err != nil {
		t.Fatal(err)
	}
	for _, changed := range []Limits{{}, {Processes: 3}} {
		if err := ConfigureBudget(changed); !errors.Is(err, ErrLimitsUnavailable) {
			t.Fatalf("changed budget: %v", err)
		}
	}
	var ext windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(budgetJob.job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&ext)), uint32(unsafe.Sizeof(ext)), nil); err != nil {
		t.Fatal(err)
	}
	if ext.JobMemoryLimit != uintptr(limits.MemoryBytes) || ext.BasicLimitInformation.ActiveProcessLimit != 2 || ext.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE == 0 {
		t.Fatalf("aggregate limits: %+v", ext)
	}
	var rate cpuRateControl
	if err := windows.QueryInformationJobObject(budgetJob.job, windows.JobObjectCpuRateControlInformation, uintptr(unsafe.Pointer(&rate)), uint32(unsafe.Sizeof(rate)), nil); err != nil {
		t.Fatal(err)
	}
	if rate.CpuRate != cpuRate(4, int(windows.GetActiveProcessorCount(windows.ALL_PROCESSOR_GROUPS))) || rate.ControlFlags != cpuRateEnable|cpuRateHardCap {
		t.Fatalf("aggregate CPU: %+v", rate)
	}
	// Query an actual nested tree Job: its rate is relative to the parent's
	// installed capacity, so one core must not be capped a second time.
	nested, err := prepareTree(&exec.Cmd{}, Limits{CPUs: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer nested.close()
	var nestedRate cpuRateControl
	if err := windows.QueryInformationJobObject(nested.job, windows.JobObjectCpuRateControlInformation, uintptr(unsafe.Pointer(&nestedRate)), uint32(unsafe.Sizeof(nestedRate)), nil); err != nil {
		t.Fatal(err)
	}
	capacity := float64(windows.GetActiveProcessorCount(windows.ALL_PROCESSOR_GROUPS)) * float64(rate.CpuRate) / 10000
	wantRate := uint32(math.Min(10000, math.Floor(10000/capacity)))
	if nestedRate.CpuRate != wantRate {
		t.Fatalf("nested one-core rate=%d want %d", nestedRate.CpuRate, wantRate)
	}
	membership := func(pid int, want bool) {
		t.Helper()
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
		if err != nil {
			t.Fatal(err)
		}
		defer windows.CloseHandle(h)
		var member int32
		ok, _, err := windows.NewLazySystemDLL("kernel32.dll").NewProc("IsProcessInJob").Call(uintptr(h), uintptr(budgetJob.job), uintptr(unsafe.Pointer(&member)))
		if ok == 0 {
			t.Fatal(err)
		}
		inJob := member != 0
		if inJob != want {
			t.Fatalf("pid %d in aggregate Job=%v, want %v", pid, inJob, want)
		}
	}
	membership(os.Getpid(), false) // website/supervisor is excluded
	p := startHelper(t, ctx, "rpc", Limits{MemoryBytes: 2 << 30, Processes: 2})
	membership(p.PID(), true)
	var spawned bool
	if err := p.Peer.Call(ctx, "spawnAttempt", nil, &spawned); err != nil || !spawned {
		t.Fatalf("single runner should fit: %v %v", spawned, err)
	}
	q := startHelper(t, ctx, "rpc", Limits{MemoryBytes: 2 << 30, Processes: 2})
	membership(q.PID(), true)
	if err := p.Peer.Call(ctx, "spawnAttempt", nil, &spawned); err != nil || spawned {
		t.Fatalf("aggregate process cap: %v %v", spawned, err)
	}
	extra, err := Start(ctx, os.Args[0], helperArgs("rpc"), helperEnv(), Limits{Processes: 2}, nil)
	if extra != nil {
		extra.Close()
		t.Fatal("third runner admitted")
	}
	if !errors.Is(err, ErrLimitsUnavailable) {
		t.Fatalf("third runner failure: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Peer.Call(ctx, "echo", 1, nil); err != nil {
		t.Fatalf("sibling killed: %v", err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	worker := func() *Process {
		t.Helper()
		p, err := Start(ctx, os.Args[0], []string{"-test.run=^TestWindowsBudgetWorker$"}, append(helperEnv(), "EXECUTION_BUDGET_WORKER=1"), Limits{MemoryBytes: 2 << 30, Processes: 2}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := p.Close(); err != nil {
				t.Error(err)
			}
		})
		membership(p.PID(), true)
		return p
	}
	a, b := worker(), worker()
	if err := windows.QueryInformationJobObject(budgetJob.job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&ext)), uint32(unsafe.Sizeof(ext)), nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("two-worker baseline peak job commit: %d MiB", ext.PeakJobMemoryUsed>>20)
	allocate := func(p *Process, want bool) {
		t.Helper()
		var allocated bool
		if err := p.Peer.Call(ctx, "allocate", 1152<<20, &allocated); err != nil || allocated != want {
			t.Fatalf("allocation=%v want %v: %v", allocated, want, err)
		}
	}
	allocate(a, true)
	allocate(b, false) // each fits alone, together exceed the shared commit cap
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	allocate(b, true)
	membership(os.Getpid(), false)
	if err := ConfigureBudget(limits); err != nil {
		t.Fatal(err)
	}
	t.Log("shared Job membership/CPU/memory/process limits verified; aggregate memory and process excess refused; sibling survives Close; website excluded")
}
