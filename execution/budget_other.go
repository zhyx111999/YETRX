//go:build !linux && !windows

package execution

import "fmt"

func configureBudget(Limits) error {
	return fmt.Errorf("%w: aggregate budgets require Linux or Windows", ErrLimitsUnavailable)
}
