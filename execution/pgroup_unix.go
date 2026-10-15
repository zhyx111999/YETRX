//go:build unix

package execution

import (
	"errors"
	"syscall"
	"time"
)

// pgroup is the child's process group (the child is its leader). It contains
// descendants that do not call setsid/setpgid themselves.
type pgroup int

func (g pgroup) signal(sig syscall.Signal) {
	if g > 0 {
		syscall.Kill(-int(g), sig)
	}
}

// gone reports that no process of the group exists any more.
func (g pgroup) gone() bool {
	return g <= 0 || errors.Is(syscall.Kill(-int(g), 0), syscall.ESRCH)
}

// waitFor polls cond every 10ms for up to 5s and reports whether it became true.
func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}
