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
	if err != nil || !defaults.Enabled || defaults.MemoryMiB <= 0 || defaults.ResourceMemoryMiB <= 0 {
		t.Fatalf("default limits: %+v %v", defaults, err)
	}
	for _, item := range []struct{ key, value string }{
		{"ARTEX_EXECUTION_ENABLED", "maybe"}, {"ARTEX_EXECUTION_MAX_SESSIONS", "0"},
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
	t.Setenv("ARTEX_EXECUTION_ENABLED", "false")
	c, err := Execution()
	if err != nil || c.Enabled {
		t.Fatalf("explicit opt-out: %+v %v", c, err)
	}
}
