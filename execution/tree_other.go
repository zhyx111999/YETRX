//go:build !unix && !windows

package execution

import (
	"fmt"
	"os/exec"
	"runtime"
)

type tree struct{ cmd *exec.Cmd }

func newTree(cmd *exec.Cmd, l Limits) (*tree, error) {
	if l != (Limits{}) {
		return nil, fmt.Errorf("%w: not supported on %s", ErrLimitsUnavailable, runtime.GOOS)
	}
	return &tree{cmd: cmd}, nil
}
func (t *tree) start(cmd *exec.Cmd) error { return cmd.Start() }
func (t *tree) terminate()                {}
func (t *tree) kill() {
	if t.cmd.Process != nil {
		_ = t.cmd.Process.Kill()
	}
}
func (t *tree) drain() error { return nil }
func (t *tree) close() error { return nil }
