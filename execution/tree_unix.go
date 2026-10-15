//go:build unix && !linux

package execution

import (
	"fmt"
	"os/exec"
	"runtime"
	"syscall"
)

// tree is the child's process group. There is no hard resource limiting on
// this platform and no parent-death signal: descendants that leave the group
// are not tracked, and a killed supervisor only stops the child via stdin EOF.
type tree struct{ pg pgroup }

func newTree(cmd *exec.Cmd, l Limits) (*tree, error) {
	if l != (Limits{}) {
		return nil, fmt.Errorf("%w: not supported on %s", ErrLimitsUnavailable, runtime.GOOS)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &tree{}, nil
}

func (t *tree) start(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	t.pg = pgroup(cmd.Process.Pid)
	return nil
}

func (t *tree) terminate() { t.pg.signal(syscall.SIGTERM) }
func (t *tree) kill()      { t.pg.signal(syscall.SIGKILL) }

func (t *tree) drain() error {
	if !waitFor(func() bool { t.kill(); return t.pg.gone() }) {
		return fmt.Errorf("execution: process group %d still alive after SIGKILL", t.pg)
	}
	return nil
}

func (t *tree) close() error { return nil }
