//go:build linux

package execution

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

func configureEscapedTestProcess(cmd *exec.Cmd) {
	if own, err := ownCgroup(); err == nil && os.Getenv("EXECUTION_TEST_CGROUP") == "1" && strings.HasPrefix(filepath.Base(own), childPrefix) {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	}
}
func testZombie(pid int) bool { return isZombie(pid) }

// Run in a dedicated delegated service/scope, with no other processes in its
// initial cgroup. This opt-in test intentionally moves only this test process.
func TestLinuxCgroupIntegration(t *testing.T) {
	if os.Getenv("EXECUTION_TEST_CGROUP") != "1" {
		t.Skip("requires dedicated cgroup v2 delegation; see README.md")
	}
	own, err := ownCgroup()
	if err != nil {
		t.Fatal(err)
	}
	if err := InitCgroup(); err != nil {
		t.Fatal(err)
	}
	now, err := ownCgroup()
	if err != nil {
		t.Fatal(err)
	}
	if now != filepath.Join(own, managerLeaf) && now != own {
		t.Fatalf("unexpected manager %s", now)
	}
	if err := InitCgroup(); err != nil {
		t.Fatal(err)
	}
	p := startHelper(t, testContext(t), "rpc", Limits{MemoryBytes: 128 << 20, CPUs: 0.5, Processes: 128})
	b, err := os.ReadFile("/proc/" + strconv.Itoa(p.PID()) + "/cgroup")
	if err != nil {
		t.Fatal(err)
	}
	path := strings.TrimSpace(strings.TrimPrefix(string(b), "0::"))
	dir := filepath.Join(cgroupRoot, path)
	if !strings.HasPrefix(filepath.Base(dir), childPrefix) {
		t.Fatalf("child outside execution cgroup: %s", dir)
	}
	for file, want := range map[string]string{"memory.max": "134217728", "cpu.max": "50000 100000", "pids.max": "128"} {
		b, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil || strings.TrimSpace(string(b)) != want {
			t.Fatalf("%s: %s %v", file, b, err)
		}
	}
	_ = p.Peer.Call(testContext(t), "memory", nil, nil)
	var exit *exec.ExitError
	if err := p.Wait(); !errors.As(err, &exit) {
		t.Fatalf("memory exhaustion: %v", err)
	}
	if status, ok := exit.Sys().(syscall.WaitStatus); !ok || status.Signal() != syscall.SIGKILL {
		t.Fatalf("expected cgroup OOM SIGKILL: %v", exit)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cgroup not removed: %v", err)
	}
	limited := startHelper(t, testContext(t), "rpc", Limits{Processes: 1})
	if err := limited.Wait(); err == nil || !strings.Contains(limited.StderrTail(), "failed to create new OS thread") {
		t.Fatalf("pids.max=1 did not refuse runtime threads: %v", err)
	}
	testProcessCleanup(t, Limits{MemoryBytes: 256 << 20, CPUs: 1, Processes: 128})
}

func TestLinuxCgroupPreflightPreservesOthers(t *testing.T) {
	dir := t.TempDir()
	caller := os.Getpid()
	others := strconv.Itoa(caller) + "\n999999\n"
	if err := os.WriteFile(filepath.Join(dir, "cgroup.controllers"), []byte("memory cpu pids"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(others), 0600); err != nil {
		t.Fatal(err)
	}
	if err := preflightCgroup(dir, caller); !errors.Is(err, ErrLimitsUnavailable) {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil || string(b) != others {
		t.Fatal("preflight modified other processes")
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.procs"), []byte(strconv.Itoa(caller)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := preflightCgroup(dir, caller); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cgroup.controllers"), []byte("cpu pids"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := preflightCgroup(dir, caller); !errors.Is(err, ErrLimitsUnavailable) {
		t.Fatal(err)
	}
}
