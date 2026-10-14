//go:build !windows

package workflow

import (
	"os"
	"os/exec"
	"syscall"
	"time"
)

func configureGitProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		pid := cmd.Process.Pid
		if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
			return err
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if syscall.Kill(-pid, 0) != nil {
				return nil
			}
			time.Sleep(50 * time.Millisecond)
		}
		return syscall.Kill(-pid, syscall.SIGKILL)
	}
}
