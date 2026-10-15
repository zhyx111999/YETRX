//go:build linux

package execution

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
)

// tree is the child's process group plus, when limits or InitCgroup ask for it,
// a cgroup v2 that holds the limits and contains every descendant.
type tree struct {
	cg *cgroup
	pg pgroup
}

func newTree(cmd *exec.Cmd, l Limits) (*tree, error) {
	cg, err := newCgroup(l)
	if err != nil {
		return nil, err
	}
	attr := &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if cg != nil {
		// clone3(CLONE_INTO_CGROUP): the child is born inside the cgroup, limits already set.
		attr.UseCgroupFD, attr.CgroupFD = true, int(cg.fd.Fd())
	}
	cmd.SysProcAttr = attr
	return &tree{cg: cg}, nil
}

func (t *tree) start(cmd *exec.Cmd) error {
	// Pdeathsig fires when the forking *thread* exits. The calling goroutine
	// (Process.supervise) stays locked to it until the tree is gone.
	runtime.LockOSThread()
	err := cmd.Start()
	if t.cg != nil {
		t.cg.fd.Close()
		t.cg.fd = nil
	}
	if err != nil {
		if t.cg != nil {
			return unavailable(err, "clone into cgroup")
		}
		return err
	}
	t.pg = pgroup(cmd.Process.Pid)
	return nil
}

func (t *tree) terminate() { t.pg.signal(syscall.SIGTERM) }

func (t *tree) kill() {
	if t.cg != nil {
		t.cg.kill()
	}
	t.pg.signal(syscall.SIGKILL)
}

func (t *tree) drain() error {
	if !waitFor(func() bool { t.kill(); return t.empty() }) {
		return fmt.Errorf("execution: process tree (pgid %d) still alive after SIGKILL", t.pg)
	}
	return nil
}

func (t *tree) empty() bool {
	if t.cg != nil {
		return t.cg.empty()
	}
	// Zombies are already dead; their reaping belongs to their new parent.
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return t.pg.gone()
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		pgid, err := syscall.Getpgid(pid)
		if err == nil && pgid == int(t.pg) && !isZombie(pid) {
			return false
		}
		if err != nil && !errors.Is(err, syscall.ESRCH) {
			return false
		}
	}
	return true
}

func (t *tree) close() error {
	runtime.UnlockOSThread()
	if t.cg != nil {
		return t.cg.release()
	}
	return nil
}
