//go:build linux

package execution

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Run only in a disposable delegated service; see README.md. A fresh test
// supervisor keeps the startup-only budget independent of other tests.
func TestLinuxAggregateBudget(t *testing.T) {
	if os.Getenv("EXECUTION_TEST_CGROUP") != "1" {
		t.Skip("requires dedicated cgroup v2 delegation; see README.md")
	}
	// Park the outer test harness in the manager leaf before spawning the
	// isolated supervisor; otherwise it would occupy the delegation root.
	if os.Getenv("EXECUTION_BUDGET_TEST") != t.Name() {
		if err := InitCgroup(); err != nil {
			t.Fatal(err)
		}
	}
	if !isolatedBudgetTest(t) {
		return
	}
	if err := ConfigureBudget(Limits{Processes: 1024}); !errors.Is(err, ErrLimitsUnavailable) {
		t.Fatalf("missing InitCgroup: %v", err)
	}
	if err := InitCgroup(); err != nil {
		t.Fatal(err)
	}
	root := cgParent
	// An existing exec-* node must not be adopted or modified.
	occupied := filepath.Join(root, childPrefix+"budget-preflight")
	if err := os.Mkdir(occupied, 0755); err != nil {
		t.Fatal(err)
	}
	err := ConfigureBudget(Limits{Processes: 1024})
	removeErr := os.Remove(occupied)
	if !errors.Is(err, ErrLimitsUnavailable) || removeErr != nil {
		t.Fatalf("existing runner guard: %v; cleanup: %v", err, removeErr)
	}
	limits := Limits{MemoryBytes: 3 << 30, CPUs: 4, Processes: 1024}
	if err := ConfigureBudget(limits); err != nil {
		t.Fatal(err)
	}
	workers := filepath.Join(root, workersNode)
	t.Cleanup(func() {
		if err := os.Remove(workers); err != nil {
			t.Errorf("remove empty aggregate node: %v", err)
		}
	})
	if err := InitCgroup(); err != nil {
		t.Fatal(err)
	}
	if cgParent != workers {
		t.Fatalf("InitCgroup reset budget parent to %s", cgParent)
	}
	manager, err := ownCgroup()
	if err != nil || manager != filepath.Join(root, managerLeaf) {
		t.Fatalf("website manager must be budget sibling: %s %v", manager, err)
	}
	check := func(dir, file, want string) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil || strings.TrimSpace(string(b)) != want {
			t.Fatalf("%s/%s=%q want %q: %v", dir, file, b, want, err)
		}
	}
	for file, want := range map[string]string{"memory.max": "3221225472", "cpu.max": "400000 100000", "pids.max": "1024", "memory.oom.group": "0", "memory.swap.max": "0", "cgroup.procs": ""} {
		check(workers, file, want)
	}
	if missing := missingFrom(workers, "cgroup.subtree_control", []string{"memory", "cpu", "pids"}); len(missing) > 0 {
		t.Fatalf("missing subtree controllers: %v", missing)
	}
	ctx := testContext(t)
	local := Limits{MemoryBytes: 512 << 20, CPUs: 1, Processes: 128}
	a := startHelper(t, ctx, "rpc", local)
	b := startHelper(t, ctx, "rpc", local)
	for _, p := range []*Process{a, b} {
		if err := p.Peer.Call(ctx, "echo", 1, nil); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile("/proc/" + strconv.Itoa(p.PID()) + "/cgroup")
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(cgroupRoot, strings.TrimSpace(strings.TrimPrefix(string(raw), "0::")))
		if filepath.Dir(dir) != workers || !strings.HasPrefix(filepath.Base(dir), childPrefix) {
			t.Fatalf("runner outside shared parent: %s", dir)
		}
		for file, want := range map[string]string{"memory.max": "536870912", "cpu.max": "100000 100000", "pids.max": "128", "memory.oom.group": "1"} {
			check(dir, file, want)
		}
	}
	if err := ConfigureBudget(limits); err != nil {
		t.Fatal(err)
	}
	if err := ConfigureBudget(Limits{}); !errors.Is(err, ErrLimitsUnavailable) {
		t.Fatalf("runtime change: %v", err)
	}
	check(workers, "memory.max", "3221225472")
	check(workers, "cgroup.procs", "")
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Peer.Call(ctx, "echo", 1, nil); err != nil {
		t.Fatalf("sibling affected by Close: %v", err)
	}
	t.Log("two runner cgroups share aggregate limits; manager remains sibling; repeated InitCgroup preserves budget; per-runner OOM groups stay isolated")
}
