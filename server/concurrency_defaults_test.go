package server

import "testing"

func TestExplorationWorkerDefault(t *testing.T) {
	if defaultWorkers != 8 {
		t.Fatalf("default Worker count = %d, want 8", defaultWorkers)
	}
	if defaultConcurrencyLimit != 5 {
		t.Fatalf("unrelated root-task concurrency limit changed: %d", defaultConcurrencyLimit)
	}
}
