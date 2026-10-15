//go:build linux

package execution

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const workersNode = "execution-workers"

func configureBudget(l Limits) error {
	cgMu.Lock()
	defer cgMu.Unlock()
	if cgParent == "" {
		return unavailable(nil, "call InitCgroup before ConfigureBudget")
	}
	// Never adopt or move existing runners, including another supervisor's.
	entries, err := os.ReadDir(cgParent)
	if err != nil {
		return unavailable(err, "inspect delegation root")
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), childPrefix) {
			return unavailable(nil, "existing runner cgroup %s prevents budget setup", entry.Name())
		}
	}
	if err := preflightCgroup(cgParent, 0); err != nil {
		return err
	}
	c := &cgroup{dir: filepath.Join(cgParent, workersNode)}
	// Exclusive creation avoids modifying any existing processes or budget.
	if err := os.Mkdir(c.dir, 0755); err != nil {
		return unavailable(err, "create aggregate cgroup")
	}
	setup := func() error {
		if err := c.configure(l); err != nil {
			return err
		}
		// Select an individual runner OOM group, never kill every runner together.
		if err := writeCgroup(c.dir, "memory.oom.group", "0"); err != nil {
			return unavailable(err, "set aggregate OOM policy")
		}
		if err := writeCgroup(c.dir, "cgroup.subtree_control", "+memory +cpu +pids"); err != nil {
			return unavailable(err, "enable aggregate subtree controllers")
		}
		return nil
	}
	if err := setup(); err != nil {
		return errors.Join(err, c.remove()) // Empty node only; never signal processes.
	}
	cgParent = c.dir
	return nil
}
