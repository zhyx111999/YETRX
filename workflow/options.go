package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Options apply to a workflow invocation. Retries count additional attempts;
// zero is deliberately the default, matching pi-workflows-lean.
type Options struct {
	CWD            string `json:"cwd,omitempty"`
	AgentRetries   int    `json:"agent_retries,omitempty"`
	AgentTimeoutMS int64  `json:"agent_timeout_ms,omitempty"`
}

type Attempt struct {
	Number      int       `json:"number"`
	SessionID   string    `json:"session_id"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
	Error       string    `json:"error,omitempty"`
	Code        string    `json:"code,omitempty"`
	Recoverable bool      `json:"recoverable,omitempty"`
}

type CheckpointInput struct {
	ID      string          `json:"checkpoint_id"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

type Checkpoint struct {
	CheckpointInput
	Label      string          `json:"label"`
	Status     string          `json:"status"`
	Response   json.RawMessage `json:"response,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
	ConsumedAt time.Time       `json:"consumed_at,omitzero"`
}

type ResumeOptions struct {
	Tasks        []Task          `json:"tasks,omitempty"`
	CheckpointID string          `json:"checkpoint_id,omitempty"`
	Response     json.RawMessage `json:"response,omitempty"`
}

// Failure separates execution failures from deterministic setup failures and
// provider quotas. Only the former consume the configured retry budget.
type Failure struct {
	Code        string
	Recoverable bool
	Cause       error
}

func (e *Failure) Error() string { return e.Cause.Error() }
func (e *Failure) Unwrap() error { return e.Cause }
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &Failure{Code: "SCRIPT_VALIDATION_ERROR", Cause: err}
}

var providerLimit = regexp.MustCompile(`(?i)usage limit|limit reached|insufficient[_\s]?quota|quota exceeded|out of budget|available balance|\bquota\b|rate.?limit|too many requests|\b429\b|\bbilling\b`)

func classify(err error) *Failure {
	if err == nil {
		return nil
	}
	var f *Failure
	if errors.As(err, &f) {
		return f
	}
	if errors.Is(err, context.Canceled) {
		return &Failure{Code: "WORKFLOW_ABORTED", Cause: err}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &Failure{Code: "AGENT_TIMEOUT", Recoverable: true, Cause: err}
	}
	if providerLimit.MatchString(err.Error()) {
		return &Failure{Code: "PROVIDER_USAGE_LIMIT", Cause: err}
	}
	return &Failure{Code: "AGENT_EXECUTION_ERROR", Recoverable: true, Cause: err}
}

func normalizeOptions(o Options) (Options, error) {
	o.AgentRetries = min(MaxAgentRetries, max(0, o.AgentRetries))
	if o.AgentTimeoutMS < 0 || o.AgentTimeoutMS > 2147483647 {
		return o, fmt.Errorf("%w: agent_timeout_ms must be 0..2147483647", ErrInvalid)
	}
	if o.CWD != "" {
		cwd, err := canonicalDirectory(o.CWD)
		if err != nil {
			return o, fmt.Errorf("%w: cwd: %v", ErrInvalid, err)
		}
		o.CWD = cwd
	}
	return o, nil
}

var checkpointIdentifier = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,200}$`)

func validateCheckpoint(c *CheckpointInput) error {
	if c == nil {
		return nil
	}
	if !checkpointIdentifier.MatchString(c.ID) || !checkpointIdentifier.MatchString(c.Kind) || !json.Valid(c.Payload) {
		return fmt.Errorf("%w: checkpoint needs valid checkpoint_id, kind and a JSON payload", ErrInvalid)
	}
	return nil
}

func taskRequest(t Task) Task {
	return Task{Label: strings.TrimSpace(t.Label), Task: t.Task, AgentKey: strings.TrimSpace(t.AgentKey), DependsOn: t.DependsOn,
		CWD: t.CWD, Isolation: t.Isolation, KeepWorktree: t.KeepWorktree, Thread: strings.TrimSpace(t.Thread), Checkpoint: t.Checkpoint, Status: Pending}
}
