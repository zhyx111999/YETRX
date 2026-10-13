// Package workflow keeps the state of native background sub-task runs: it
// validates a batch of tasks (independent ones run in parallel, dependent ones
// wait for their dependencies), schedules them under one process-wide
// concurrency limit, persists every state change to <dir>/<runID>.json and tells
// the owner when a run reaches a terminal state.
//
// It does not execute anything itself. The Runner supplied to New runs one task
// (the server wires it to a native ChatAgent), so there is exactly one agent
// framework in the process.
//
// Lifecycle: runs live as long as the ctx passed to New (the server's), never
// as long as the user turn that started them. Cancelling that ctx is a process
// shutdown: nothing is rewritten, so the next New finds the in-flight work still
// marked running and flips it to interrupted. Interrupted work is never re-run
// automatically (tasks may have side effects); the owner calls Resume.
package workflow

import (
	"context"
	"errors"
	"time"
)

const (
	MaxTasks           = 32   // tasks per run
	MaxDepth           = 3    // nesting depth: a caller at depth >= MaxDepth cannot Start
	DefaultConcurrency = 4    // default global limit of simultaneously running tasks
	MaxReadChars       = 8000 // Read never returns more than this many characters
)

// Status is the state of a Task or a Run. Runs only use running, succeeded,
// failed, stopped and interrupted.
type Status string

const (
	Pending     Status = "pending"
	Running     Status = "running"
	Succeeded   Status = "succeeded"
	Failed      Status = "failed"
	Blocked     Status = "blocked"     // a dependency failed or was blocked, so it never ran
	Stopped     Status = "stopped"     // cancelled by Stop
	Interrupted Status = "interrupted" // was running when the process went away
)

var (
	ErrInvalid         = errors.New("workflow: invalid request")
	ErrDepth           = errors.New("workflow: nesting too deep")
	ErrNotFound        = errors.New("workflow: run not found") // also for runs owned by someone else
	ErrNoTask          = errors.New("workflow: task not found")
	ErrNoResult        = errors.New("workflow: task has no result")
	ErrRunning         = errors.New("workflow: run is still running")
	ErrNotRunning      = errors.New("workflow: run is not running")
	ErrNothingToResume = errors.New("workflow: every task already succeeded")
	ErrClosed          = errors.New("workflow: manager is shut down")
)

// Task is one unit of work. The first five fields are the request; the rest is
// state maintained by the Manager (ignored by Start).
type Task struct {
	Label        string   `json:"label"` // unique inside the run
	Task         string   `json:"task"`  // the prompt
	AgentKey     string   `json:"agent_key,omitempty"`
	LLMProfileID *int64   `json:"llm_profile_id,omitempty"` // nil = follow the owner / global profile
	DependsOn    []string `json:"depends_on,omitempty"`     // labels of tasks that must succeed first

	Status     Status    `json:"status"`
	Result     string    `json:"result,omitempty"` // full text; Read returns its tail
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

// Run is a batch of tasks started by one owner. Values handed out by the Manager
// are private copies.
type Run struct {
	ID           string `json:"id"`
	OwnerSession string `json:"owner_session"`
	OwnerAgent   string `json:"owner_agent"`
	Depth        int    `json:"depth"` // nesting depth of the owner; its tasks run at Depth+1
	Status       Status `json:"status"`
	Tasks        []Task `json:"tasks"`

	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

// Job is what a Runner gets for one task.
type Job struct {
	RunID        string
	OwnerSession string
	OwnerAgent   string
	Depth        int  // depth of the agent that runs this task; pass it to Start if that agent starts runs of its own
	Task         Task // Label, Task, AgentKey, LLMProfileID, DependsOn
	// Results maps each label in Task.DependsOn to that task's full result.
	Results map[string]string
}

// Runner executes one task and returns its final text. It must return promptly
// once ctx is cancelled (Stop, shutdown). A non-nil error marks the task failed;
// any text returned alongside it is kept as a partial result.
type Runner func(ctx context.Context, job Job) (string, error)

func (r Run) clone() Run {
	r.Tasks = cloneTasks(r.Tasks)
	return r
}

func cloneTasks(in []Task) []Task {
	out := make([]Task, len(in))
	for i, t := range in {
		t.DependsOn = append([]string(nil), t.DependsOn...)
		if t.LLMProfileID != nil {
			id := *t.LLMProfileID
			t.LLMProfileID = &id
		}
		out[i] = t
	}
	return out
}
