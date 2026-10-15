package config

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

const (
	DefaultResourceWorkers  = 64
	MaxExecutionConcurrency = 1024
	ResourceWorkersSetting  = "resource_max_workers"
	ExecutionBudgetSetting  = "execution_budget"
)

// ProcessBudget applies to an execution tree, or to all worker trees together.
type ProcessBudget struct {
	MemoryMiB int     `json:"memory_mib"`
	CPUs      float64 `json:"cpus"`
	Processes int     `json:"processes"`
}

type ExecutionBudget struct {
	Agent  ProcessBudget `json:"agent"`
	Helper ProcessBudget `json:"helper"`
	Total  ProcessBudget `json:"total"`
}

func (b ExecutionBudget) Validate() error {
	for _, group := range []struct {
		name   string
		budget ProcessBudget
	}{{"agent", b.Agent}, {"helper", b.Helper}, {"total", b.Total}} {
		v := group.budget
		if v.MemoryMiB < 1 || v.MemoryMiB > 1048576 || v.Processes < 1 || v.Processes > 1048576 {
			return fmt.Errorf("%s memory_mib and processes must be integers between 1 and 1048576", group.name)
		}
		if math.IsNaN(v.CPUs) || math.IsInf(v.CPUs, 0) || v.CPUs <= 0 || v.CPUs > 1024 {
			return fmt.Errorf("%s cpus must be a positive finite core count up to 1024", group.name)
		}
	}
	return nil
}

// ExecutionSettings has no shared Agent session count cap. OS budgets remain
// mandatory when enabled; persisted settings are resolved before workers start.
type ExecutionSettings struct {
	Enabled      bool
	MaxResources int
	Budget       ExecutionBudget
}

type SettingLookup func(string) (string, bool, error)

func Execution() (ExecutionSettings, error) { return ExecutionWithSettings(nil) }

// ExecutionWithSettings gives saved UI values precedence over environment
// fallbacks. The explicit isolation opt-out remains environment-only.
func ExecutionWithSettings(lookup SettingLookup) (ExecutionSettings, error) {
	c := ExecutionSettings{
		Enabled: true, MaxResources: DefaultResourceWorkers,
		Budget: ExecutionBudget{
			Agent:  ProcessBudget{MemoryMiB: 512, CPUs: 1, Processes: 128},
			Helper: ProcessBudget{MemoryMiB: 384, CPUs: 1, Processes: 128},
			Total:  ProcessBudget{MemoryMiB: 3072, CPUs: 4, Processes: 1024},
		},
	}
	if raw, ok := os.LookupEnv("ARTEX_EXECUTION_ENABLED"); ok {
		v, err := strconv.ParseBool(strings.TrimSpace(raw))
		if err != nil {
			return c, fmt.Errorf("ARTEX_EXECUTION_ENABLED must be true or false")
		}
		c.Enabled = v
	}
	read := func(key string) (string, bool, error) {
		if lookup == nil {
			return "", false, nil
		}
		return lookup(key)
	}
	resourceRaw, resourceSaved, err := read(ResourceWorkersSetting)
	if err != nil {
		return c, err
	}
	resourceSource := ResourceWorkersSetting
	if !resourceSaved {
		resourceRaw, resourceSaved = os.LookupEnv("ARTEX_RESOURCE_MAX_WORKERS")
		resourceSource = "ARTEX_RESOURCE_MAX_WORKERS"
	}
	if resourceSaved {
		n, err := strconv.Atoi(strings.TrimSpace(resourceRaw))
		if err != nil || n < 1 || n > MaxExecutionConcurrency {
			return c, fmt.Errorf("%s must be between 1 and %d", resourceSource, MaxExecutionConcurrency)
		}
		c.MaxResources = n
	}
	budgetRaw, budgetSaved, err := read(ExecutionBudgetSetting)
	if err != nil {
		return c, err
	}
	if budgetSaved {
		var budget ExecutionBudget
		if err := json.Unmarshal([]byte(budgetRaw), &budget); err != nil {
			return c, fmt.Errorf("invalid saved execution budget: %w", err)
		}
		c.Budget = budget
	} else {
		for _, group := range []struct {
			prefix string
			budget *ProcessBudget
		}{{"ARTEX_EXECUTION", &c.Budget.Agent}, {"ARTEX_RESOURCE", &c.Budget.Helper}, {"ARTEX_EXECUTION_TOTAL", &c.Budget.Total}} {
			for _, field := range []struct {
				suffix string
				dst    *int
			}{{"MEMORY_MIB", &group.budget.MemoryMiB}, {"PROCESSES", &group.budget.Processes}} {
				key := group.prefix + "_" + field.suffix
				if raw, ok := os.LookupEnv(key); ok {
					n, err := strconv.Atoi(strings.TrimSpace(raw))
					if err != nil || n < 1 || n > 1048576 {
						return c, fmt.Errorf("%s must be between 1 and 1048576", key)
					}
					*field.dst = n
				}
			}
			key := group.prefix + "_CPUS"
			if raw, ok := os.LookupEnv(key); ok {
				n, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
				if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n <= 0 || n > 1024 {
					return c, fmt.Errorf("%s must be a positive finite core count up to 1024", key)
				}
				group.budget.CPUs = n
			}
		}
	}
	return c, c.Budget.Validate()
}
