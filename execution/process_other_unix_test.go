//go:build unix && !linux

package execution

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

func configureEscapedTestProcess(cmd *exec.Cmd) {}
func testZombie(pid int) bool                   { return false }
func TestUnsupportedLimits(t *testing.T) {
	if err := InitCgroup(); err != nil {
		t.Fatal(err)
	}
	_, err := Start(testContext(t), os.Args[0], helperArgs("rpc"), helperEnv(), Limits{Processes: 1}, nil)
	if !errors.Is(err, ErrLimitsUnavailable) {
		t.Fatal(err)
	}
}
