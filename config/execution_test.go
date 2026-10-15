package config

import (
	"os"
	"testing"
)

func TestExecutionSettingsRequireExplicitValidLimits(t *testing.T) {
	keys := []string{"ARTEX_EXECUTION_ENABLED", "ARTEX_EXECUTION_MAX_SESSIONS", "ARTEX_EXECUTION_MEMORY_MIB", "ARTEX_EXECUTION_CPUS", "ARTEX_EXECUTION_PROCESSES", "ARTEX_RESOURCE_MAX_WORKERS", "ARTEX_RESOURCE_MEMORY_MIB", "ARTEX_RESOURCE_CPUS", "ARTEX_RESOURCE_PROCESSES", "ARTEX_EXECUTION_TOTAL_MEMORY_MIB", "ARTEX_EXECUTION_TOTAL_CPUS", "ARTEX_EXECUTION_TOTAL_PROCESSES"}
	for _, key := range keys {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}
	defaults, err := Execution()
	if err != nil || defaults != (ExecutionSettings{
		Enabled: true, MaxResources: 64,
		Budget: ExecutionBudget{
			Agent:  ProcessBudget{MemoryMiB: 512, CPUs: 1, Processes: 128},
			Helper: ProcessBudget{MemoryMiB: 384, CPUs: 1, Processes: 128},
			Total:  ProcessBudget{MemoryMiB: 3072, CPUs: 4, Processes: 1024},
		},
	}) {
		t.Fatalf("default limits: %+v %v", defaults, err)
	}
	for _, item := range []struct{ key, value string }{
		{"ARTEX_EXECUTION_ENABLED", "maybe"},
		{"ARTEX_EXECUTION_MEMORY_MIB", "-1"}, {"ARTEX_EXECUTION_CPUS", "NaN"}, {"ARTEX_RESOURCE_CPUS", "+Inf"},
		{"ARTEX_EXECUTION_PROCESSES", "0"}, {"ARTEX_RESOURCE_MAX_WORKERS", "10000000"},
	} {
		t.Run(item.key, func(t *testing.T) {
			t.Setenv(item.key, item.value)
			if _, err := Execution(); err == nil {
				t.Fatal("invalid setting accepted")
			}
		})
	}
	// The retired global Agent count cap must not be restored by legacy env.
	t.Setenv("ARTEX_EXECUTION_MAX_SESSIONS", "not-a-limit")
	t.Setenv("ARTEX_RESOURCE_MAX_WORKERS", "12")
	overridden, err := Execution()
	if err != nil || overridden.MaxResources != 12 || overridden.Budget != defaults.Budget {
		t.Fatalf("explicit capacity overrides: %+v %v", overridden, err)
	}
	t.Setenv("ARTEX_EXECUTION_ENABLED", "false")
	c, err := Execution()
	if err != nil || c.Enabled {
		t.Fatalf("explicit opt-out: %+v %v", c, err)
	}
}
