//go:build windows

package execution

import "golang.org/x/sys/windows"

// Retained for the supervisor lifetime, never closed by a runner. The OS
// closes this non-inheritable handle on exit; KILL_ON_JOB_CLOSE kills children.
var budgetJob *tree

func configureBudget(l Limits) error {
	job, err := newJob(l, float64(windows.GetActiveProcessorCount(windows.ALL_PROCESSOR_GROUPS)))
	if err != nil {
		return err
	}
	budgetJob = job
	return nil
}
