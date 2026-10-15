package execution

import (
	"errors"
	"math"
	"os"
	"os/exec"
	"testing"
)

// A budget intentionally has no reset/close API. Exercise each startup in a
// fresh supervisor so the ordinary process tests keep their own startup state.
func isolatedBudgetTest(t *testing.T) bool {
	t.Helper()
	const marker = "EXECUTION_BUDGET_TEST"
	if os.Getenv(marker) == t.Name() {
		return true
	}
	cmd := exec.CommandContext(testContext(t), os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v", "-test.timeout=60s")
	cmd.Env = append(helperEnv(), marker+"="+t.Name())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated budget test: %v\n%s", err, out)
	}
	t.Logf("%s", out)
	return false
}

func TestBudgetStartupContract(t *testing.T) {
	if !isolatedBudgetTest(t) {
		return
	}
	for _, l := range []Limits{{MemoryBytes: -1}, {Processes: -1}, {CPUs: math.NaN()}, {CPUs: math.Inf(1)}} {
		if err := ConfigureBudget(l); err == nil {
			t.Fatalf("accepted invalid limits %+v", l)
		}
	}
	if err := ConfigureBudget(Limits{}); err != nil {
		t.Fatal(err)
	}
	p := startHelper(t, testContext(t), "rpc", Limits{})
	if err := p.Peer.Call(testContext(t), "echo", 1, nil); err != nil {
		t.Fatal(err)
	}
	if err := ConfigureBudget(Limits{Processes: 128}); !errors.Is(err, ErrLimitsUnavailable) {
		t.Fatalf("late budget: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ConfigureBudget(Limits{Processes: 128}); !errors.Is(err, ErrLimitsUnavailable) {
		t.Fatalf("post-close budget: %v", err)
	}
}
