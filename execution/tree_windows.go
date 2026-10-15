//go:build windows

package execution

import (
	"errors"
	"fmt"
	"math"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// tree is a Job Object. It carries the limits, contains every descendant, and
// with KILL_ON_JOB_CLOSE takes the whole tree down if the supervisor dies.
type tree struct{ job windows.Handle }

// JOBOBJECT_CPU_RATE_CONTROL_INFORMATION (the union is only used as CpuRate here).
type cpuRateControl struct {
	ControlFlags uint32
	CpuRate      uint32
}

// JOBOBJECT_BASIC_ACCOUNTING_INFORMATION.
type jobAccounting struct {
	TotalUserTime, TotalKernelTime                     int64
	ThisPeriodTotalUserTime, ThisPeriodTotalKernelTime int64
	TotalPageFaultCount, TotalProcesses                uint32
	ActiveProcesses, TotalTerminatedProcesses          uint32
}

const (
	cpuRateEnable  = 0x1
	cpuRateHardCap = 0x4
)

func newTree(cmd *exec.Cmd, l Limits) (*tree, error) {
	if uint64(l.Processes) > math.MaxUint32 {
		return nil, fmt.Errorf("%w: process limit exceeds Windows range", ErrLimitsUnavailable)
	}
	if l.CPUs > 0 && l.CPUs/float64(windows.GetActiveProcessorCount(windows.ALL_PROCESSOR_GROUPS))*10000 < 1 {
		return nil, fmt.Errorf("%w: CPU limit below Windows resolution", ErrLimitsUnavailable)
	}
	if uint64(l.MemoryBytes) > uint64(^uintptr(0)) {
		return nil, fmt.Errorf("%w: memory limit exceeds address space", ErrLimitsUnavailable)
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: create job object: %w", ErrLimitsUnavailable, err)
	}
	t := &tree{job: job}

	var ext windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	ext.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if l.MemoryBytes > 0 {
		ext.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_JOB_MEMORY
		ext.JobMemoryLimit = uintptr(l.MemoryBytes)
	}
	if l.Processes > 0 {
		ext.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS
		ext.BasicLimitInformation.ActiveProcessLimit = uint32(l.Processes)
	}
	if err := t.set(windows.JobObjectExtendedLimitInformation, unsafe.Pointer(&ext), unsafe.Sizeof(ext)); err != nil {
		t.close()
		return nil, err
	}
	if l.CPUs > 0 {
		rate := cpuRateControl{
			ControlFlags: cpuRateEnable | cpuRateHardCap,
			CpuRate:      cpuRate(l.CPUs, int(windows.GetActiveProcessorCount(windows.ALL_PROCESSOR_GROUPS))),
		}
		if err := t.set(windows.JobObjectCpuRateControlInformation, unsafe.Pointer(&rate), unsafe.Sizeof(rate)); err != nil {
			t.close()
			return nil, err
		}
	}

	// Created suspended so the child cannot run before it is inside the job.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_SUSPENDED}
	return t, nil
}

// cpuRate converts CPUs (1.0 = one logical core) to the job's hard-cap unit:
// hundredths of a percent of ALL logical processors, clamped to [1, 10000].
func cpuRate(cpus float64, logical int) uint32 {
	if logical < 1 {
		logical = 1
	}
	return uint32(min(10000, max(1, math.Floor(cpus/float64(logical)*10000))))
}

func (t *tree) set(class uint32, info unsafe.Pointer, size uintptr) error {
	if _, err := windows.SetInformationJobObject(t.job, class, uintptr(info), uint32(size)); err != nil {
		return fmt.Errorf("%w: set job limits: %w", ErrLimitsUnavailable, err)
	}
	return nil
}

// start launches the suspended child, puts it in the job, then resumes it.
func (t *tree) start(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	if err := t.adopt(uint32(cmd.Process.Pid)); err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("%w: confine process: %w", ErrLimitsUnavailable, err)
	}
	return nil
}

func (t *tree) adopt(pid uint32) error {
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(t.job, h); err != nil {
		return err
	}
	return resumeThreads(pid)
}

func resumeThreads(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snap)
	te := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	resumed := 0
	for err := windows.Thread32First(snap, &te); err == nil; err = windows.Thread32Next(snap, &te) {
		if te.OwnerProcessID != pid {
			continue
		}
		h, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, te.ThreadID)
		if err != nil {
			return err
		}
		_, err = windows.ResumeThread(h)
		windows.CloseHandle(h)
		if err != nil {
			return err
		}
		resumed++
	}
	if resumed == 0 {
		return errors.New("no thread to resume")
	}
	return nil
}

// terminate: console-less Windows processes have no SIGTERM; the stdin EOF the
// supervisor sends is the graceful signal.
func (t *tree) terminate() {}

// kill terminates every process in the job.
func (t *tree) kill() { windows.TerminateJobObject(t.job, 1) }

// drain waits until no process of the job is alive.
func (t *tree) drain() error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		var a jobAccounting
		err := windows.QueryInformationJobObject(t.job, windows.JobObjectBasicAccountingInformation,
			uintptr(unsafe.Pointer(&a)), uint32(unsafe.Sizeof(a)), nil)
		if err != nil {
			return fmt.Errorf("execution: query job: %w", err)
		}
		if a.ActiveProcesses == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("execution: %d process(es) still alive after job termination", a.ActiveProcesses)
		}
		t.kill()
		time.Sleep(10 * time.Millisecond)
	}
}

func (t *tree) close() error { return windows.CloseHandle(t.job) }
