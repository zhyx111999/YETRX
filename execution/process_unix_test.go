//go:build unix

package execution

import (
	"os/exec"
	"os/signal"
	"syscall"
)

func ignoreTestSignals() { signal.Ignore(syscall.SIGTERM, syscall.SIGINT) }

// Escape the process group only when a Linux cgroup is explicitly under test.
func configureTestDescendant(cmd *exec.Cmd) { configureEscapedTestProcess(cmd) }
func testProcessAlive(pid int) bool         { return syscall.Kill(pid, 0) == nil && !testZombie(pid) }
