//go:build windows

package execution

import (
	"errors"
	"math"
	"os/exec"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func ignoreTestSignals()                    {}
func configureTestDescendant(cmd *exec.Cmd) {}
func testProcessAlive(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	state, err := windows.WaitForSingleObject(h, 0)
	return err == nil && state == uint32(windows.WAIT_TIMEOUT)
}

func TestWindowsJobLimits(t *testing.T) {
	if err := InitCgroup(); err != nil {
		t.Fatal(err)
	}
	ctx := testContext(t)
	p := startHelper(t, ctx, "rpc", Limits{Processes: 1, MemoryBytes: 256 << 20, CPUs: 1})
	if err := p.Peer.Call(ctx, "echo", 1, nil); err != nil {
		t.Fatalf("helper failed to enter job: %v", err)
	}
	var escaped bool
	if err := p.Peer.Call(ctx, "spawnAttempt", nil, &escaped); err != nil {
		t.Fatal(err)
	}
	if escaped {
		t.Fatal("active process limit did not prevent child execution")
	}
	t.Log("actual Job ActiveProcessLimit=1 prevented a second process from executing helper exit")
}

func TestWindowsNestedJob(t *testing.T) {
	p := startHelper(t, testContext(t), "rpc", Limits{Processes: 4})
	if err := p.Peer.Call(testContext(t), "nestedJob", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsCPUAndMemoryConfiguration(t *testing.T) {
	logical := int(windows.GetActiveProcessorCount(windows.ALL_PROCESSOR_GROUPS))
	tree, err := newTree(&exec.Cmd{}, Limits{CPUs: 1, MemoryBytes: 256 << 20, Processes: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer tree.close()
	var rate cpuRateControl
	if err := windows.QueryInformationJobObject(tree.job, windows.JobObjectCpuRateControlInformation, uintptr(unsafe.Pointer(&rate)), uint32(unsafe.Sizeof(rate)), nil); err != nil {
		t.Fatal(err)
	}
	want := uint32(math.Floor(10000 / float64(logical)))
	if rate.CpuRate != want || rate.ControlFlags != cpuRateEnable|cpuRateHardCap {
		t.Fatalf("CPU rate %+v want %d", rate, want)
	}
	t.Logf("1 core / %d logical processors => hard cap %d/10000", logical, rate.CpuRate)
	var ext windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if err := windows.QueryInformationJobObject(tree.job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&ext)), uint32(unsafe.Sizeof(ext)), nil); err != nil {
		t.Fatal(err)
	}
	if ext.JobMemoryLimit != 256<<20 || ext.BasicLimitInformation.ActiveProcessLimit != 2 || ext.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE == 0 {
		t.Fatalf("unexpected job limits: %+v", ext)
	}
	if cpuRate(1, 8) != 1250 {
		t.Fatal("one core was treated as whole machine percentage")
	}
	_, err = newTree(&exec.Cmd{}, Limits{CPUs: 0.00000001})
	if !errors.Is(err, ErrLimitsUnavailable) {
		t.Fatalf("unrepresentable limit: %v", err)
	}
}
