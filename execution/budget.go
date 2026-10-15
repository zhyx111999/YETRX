package execution

import (
	"fmt"
	"os/exec"
	"sync"
)

var budget struct {
	sync.Mutex
	limits     Limits
	configured bool
	started    bool
}

// ConfigureBudget installs a shared hard OS budget for all subsequent Start
// children and their descendants, excluding the supervisor (and website).
// Call at startup, after InitCgroup on Linux and before the first Start.
// Zero dimensions are unlimited; all-zero limits need no OS support.
// An installed budget lasts until supervisor exit. Identical calls are
// idempotent; changing an installed budget or enabling one after Start returns
// ErrLimitsUnavailable. OS setup failures also return ErrLimitsUnavailable.
func ConfigureBudget(l Limits) error {
	if err := l.validate(); err != nil {
		return err
	}
	budget.Lock()
	defer budget.Unlock()
	if budget.configured {
		if budget.limits == l {
			return nil
		}
		return fmt.Errorf("%w: aggregate budget cannot be changed", ErrLimitsUnavailable)
	}
	if l == (Limits{}) {
		return nil
	}
	if budget.started {
		return fmt.Errorf("%w: configure aggregate budget before starting runners", ErrLimitsUnavailable)
	}
	if err := configureBudget(l); err != nil {
		return err
	}
	budget.limits, budget.configured = l, true
	return nil
}

// Serialize tree preparation against budget installation: no runner can be
// prepared outside the budget while ConfigureBudget installs it.
func prepareTree(cmd *exec.Cmd, l Limits) (*tree, error) {
	budget.Lock()
	defer budget.Unlock()
	t, err := newTree(cmd, l)
	if err == nil {
		budget.started = true
	}
	return t, err
}
