package config

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// ExecutionSettings controls supervised sessions and their resource helpers.
// OS limits are mandatory when enabled. Environment settings are intentionally
// resolved before opening stores or starting any background execution.
type ExecutionSettings struct {
	Enabled           bool
	MaxSessions       int
	MemoryMiB         int
	CPUs              float64
	Processes         int
	MaxResources      int
	ResourceMemoryMiB int
	ResourceCPUs      float64
	ResourceProcesses int
	TotalMemoryMiB    int
	TotalCPUs         float64
	TotalProcesses    int
}

func Execution() (ExecutionSettings, error) {
	c := ExecutionSettings{
		Enabled: true, MaxSessions: 8, MemoryMiB: 512, CPUs: 1, Processes: 128,
		MaxResources: 16, ResourceMemoryMiB: 384, ResourceCPUs: 1, ResourceProcesses: 128,
		TotalMemoryMiB: 3072, TotalCPUs: 4, TotalProcesses: 1024,
	}
	if raw, ok := os.LookupEnv("ARTEX_EXECUTION_ENABLED"); ok {
		v, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return c, fmt.Errorf("ARTEX_EXECUTION_ENABLED must be true or false")
		}
		c.Enabled = v
	}
	for _, field := range []struct {
		name string
		dst  *int
		max  int
	}{
		{"ARTEX_EXECUTION_MAX_SESSIONS", &c.MaxSessions, 1024},
		{"ARTEX_EXECUTION_MEMORY_MIB", &c.MemoryMiB, 1048576},
		{"ARTEX_EXECUTION_PROCESSES", &c.Processes, 1048576},
		{"ARTEX_RESOURCE_MAX_WORKERS", &c.MaxResources, 1024},
		{"ARTEX_RESOURCE_MEMORY_MIB", &c.ResourceMemoryMiB, 1048576},
		{"ARTEX_RESOURCE_PROCESSES", &c.ResourceProcesses, 1048576},
		{"ARTEX_EXECUTION_TOTAL_MEMORY_MIB", &c.TotalMemoryMiB, 1048576},
		{"ARTEX_EXECUTION_TOTAL_PROCESSES", &c.TotalProcesses, 1048576},
	} {
		if raw, ok := os.LookupEnv(field.name); ok {
			v, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil || v <= 0 || v > field.max {
				return c, fmt.Errorf("%s must be between 1 and %d", field.name, field.max)
			}
			*field.dst = v
		}
	}
	for _, field := range []struct {
		name string
		dst  *float64
	}{
		{"ARTEX_EXECUTION_CPUS", &c.CPUs}, {"ARTEX_RESOURCE_CPUS", &c.ResourceCPUs}, {"ARTEX_EXECUTION_TOTAL_CPUS", &c.TotalCPUs},
	} {
		if raw, ok := os.LookupEnv(field.name); ok {
			v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
			if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 || v > 1024 {
				return c, fmt.Errorf("%s must be a positive finite core count up to 1024", field.name)
			}
			*field.dst = v
		}
	}
	return c, nil
}
