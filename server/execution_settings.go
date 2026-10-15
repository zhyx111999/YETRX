package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/workflow"
)

const settingSubagentConcurrency = "subagent_concurrency"

// NewManagerWithExecution resolves saved settings before any service can launch
// workers. Invalid budgets fail startup instead of enabling unisolated execution.
func NewManagerWithExecution(ctx context.Context, dir, proxyAddr string) (*Manager, error) {
	return newManager(dir, proxyAddr, func(pg *db.DB) error {
		c, err := config.ExecutionWithSettings(pg.GetSetting)
		if err != nil {
			return fmt.Errorf("execution configuration: %w", err)
		}
		if _, err := subagentConcurrency(pg); err != nil {
			return err
		}
		return ConfigureExecution(ctx, c)
	})
}

func subagentConcurrency(pg *db.DB) (int, error) {
	if pg == nil {
		return workflow.DefaultConcurrency, nil
	}
	raw, ok, err := pg.GetSetting(settingSubagentConcurrency)
	if err != nil {
		return 0, err
	}
	if !ok {
		return workflow.DefaultConcurrency, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 1 || n > config.MaxExecutionConcurrency {
		return 0, fmt.Errorf("invalid saved %s", settingSubagentConcurrency)
	}
	return n, nil
}

type executionSettingsPatch struct {
	ResourceMaxWorkers  *int                    `json:"resource_max_workers"`
	SubagentConcurrency *int                    `json:"subagent_concurrency"`
	ExecutionBudget     *config.ExecutionBudget `json:"execution_budget"`
}

func (p executionSettingsPatch) validate() error {
	for _, item := range []struct {
		name  string
		value *int
	}{
		{"资源 helper 并发", p.ResourceMaxWorkers}, {"每父对话子代理并发", p.SubagentConcurrency},
	} {
		if item.value != nil && (*item.value < 1 || *item.value > config.MaxExecutionConcurrency) {
			return fmt.Errorf("%s必须是 1 到 %d 的整数", item.name, config.MaxExecutionConcurrency)
		}
	}
	if p.ExecutionBudget != nil {
		return p.ExecutionBudget.Validate()
	}
	return nil
}

func (s *Server) executionSettingsPayload() (map[string]any, error) {
	s.executionSettingsMu.Lock()
	defer s.executionSettingsMu.Unlock()
	var lookup config.SettingLookup
	if s.m.pg != nil {
		lookup = s.m.pg.GetSetting
	}
	c, err := config.ExecutionWithSettings(lookup)
	if err != nil {
		return nil, err
	}
	concurrency, err := subagentConcurrency(s.m.pg)
	if err != nil {
		return nil, err
	}
	var activeBudget *config.ExecutionBudget
	pending := false
	if active := executionState.Load(); active != nil && active.Enabled {
		budget := active.Budget
		activeBudget = &budget
		pending = budget != c.Budget
	}
	return map[string]any{
		"resource_max_workers":              c.MaxResources,
		"subagent_concurrency":              concurrency,
		"resource_execution_enabled":        resourceRuntime.Load() != nil,
		"execution_budget":                  c.Budget,
		"execution_budget_active":           activeBudget,
		"execution_budget_restart_required": pending,
	}, nil
}

// Persist the execution fields atomically, then update the existing admission
// objects in place. Never replace a runtime and lose its live reservations.
func (s *Server) setExecutionSettings(ctx context.Context, p executionSettingsPatch) error {
	if err := p.validate(); err != nil {
		return err
	}
	type setting struct{ key, value string }
	var changes []setting
	if p.ResourceMaxWorkers != nil {
		changes = append(changes, setting{config.ResourceWorkersSetting, strconv.Itoa(*p.ResourceMaxWorkers)})
	}
	if p.SubagentConcurrency != nil {
		changes = append(changes, setting{settingSubagentConcurrency, strconv.Itoa(*p.SubagentConcurrency)})
	}
	if p.ExecutionBudget != nil {
		raw, err := json.Marshal(p.ExecutionBudget)
		if err != nil {
			return err
		}
		changes = append(changes, setting{config.ExecutionBudgetSetting, string(raw)})
	}
	if len(changes) == 0 {
		return nil
	}
	s.executionSettingsMu.Lock()
	defer s.executionSettingsMu.Unlock()
	tx, err := s.m.pg.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, item := range changes {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings (key, value) VALUES ($1, $2)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, item.key, item.value); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if p.ResourceMaxWorkers != nil {
		if runtime := resourceRuntime.Load(); runtime != nil {
			if err := runtime.SetMaxWorkers(*p.ResourceMaxWorkers); err != nil {
				return err
			}
		}
	}
	if p.SubagentConcurrency != nil && s.workflows != nil {
		if err := s.workflows.SetConcurrency(*p.SubagentConcurrency); err != nil {
			return err
		}
	}
	// OS budgets stay installed for this process lifetime; restart applies the
	// saved complete snapshot before creating the first supervised process.
	return nil
}
