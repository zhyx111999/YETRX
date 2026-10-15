// Package workflow keeps the state of native background sub-task runs: it
// validates a batch of tasks (independent ones run in parallel, dependent ones
// wait for their dependencies), schedules them under a concurrency limit that
// applies separately to each owner session, persists every state change to
// <dir>/<runID>.json and tells the owner when a run reaches a terminal state.
// All runs of one conversation share its limit; conversations do not share one.
// Slots are tied to live runners: a cancelled or timed-out runner that has not
// returned still holds its session's slot, and only that session notices.
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
	MaxTasks           = 32 // tasks per run
	MaxDepth           = 1  // children cannot dispatch or control workflows
	MaxAgentRetries    = 3
	DefaultConcurrency = 16   // default limit of simultaneously running tasks per owner session
	MaxReadChars       = 8000 // Read never returns more than this many characters
)

// Status is the state of a Task or a Run. Paused runs wait for a checkpoint
// response or a provider usage-limit recovery.
type Status string

const (
	Pending     Status = "pending"
	Paused      Status = "paused"
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

// Task carries request fields followed by Manager-maintained execution state.
// Start and edited resume discard caller-supplied execution state.
type Task struct {
	Label        string           `json:"label"` // unique inside the run
	Task         string           `json:"task"`  // the prompt
	AgentKey     string           `json:"agent_key,omitempty"`
	DependsOn    []string         `json:"depends_on,omitempty"` // labels of tasks that must succeed first
	CWD          string           `json:"cwd,omitempty"`
	Isolation    string           `json:"isolation,omitempty"`
	KeepWorktree *bool            `json:"keep_worktree,omitempty"`
	Thread       string           `json:"thread,omitempty"`
	Checkpoint   *CheckpointInput `json:"checkpoint,omitempty"`

	Attempts       int           `json:"attempts,omitempty"`
	SessionID      string        `json:"session_id,omitempty"`
	Workspace      *Workspace    `json:"workspace,omitempty"`
	AttemptHistory []Attempt     `json:"attempt_history,omitempty"`
	Messages       []Instruction `json:"messages,omitempty"`
	Draining       bool          `json:"draining,omitempty"`

	Status     Status    `json:"status"`
	Result     string    `json:"result,omitempty"` // full text; Read returns its tail
	Error      string    `json:"error,omitempty"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

// Run is a batch of tasks started by one owner. Values handed out by the Manager
// are private copies.
type Run struct {
	ID                 string      `json:"id"`
	OwnerSession       string      `json:"owner_session"`
	OwnerAgent         string      `json:"owner_agent"`
	Depth              int         `json:"depth"` // nesting depth of the owner; its tasks run at Depth+1
	Status             Status      `json:"status"`
	Tasks              []Task      `json:"tasks"`
	Options            Options     `json:"options,omitempty"`
	Revision           int         `json:"revision,omitempty"`
	History            []Task      `json:"history,omitempty"`
	Checkpoint         *Checkpoint `json:"checkpoint,omitempty"`
	PauseReason        string      `json:"pause_reason,omitempty"`
	ResetHint          string      `json:"reset_hint,omitempty"`
	AutoResumeAttempts int         `json:"auto_resume_attempts,omitempty"`
	ResumeAt           time.Time   `json:"resume_at,omitzero"`

	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
}

// Job is what a Runner gets for one task.
type Job struct {
	RunID        string
	OwnerSession string
	OwnerAgent   string
	Depth        int  // child depth; any further dispatch is denied
	Task         Task // Label, Task, AgentKey, DependsOn
	// Results maps each label in Task.DependsOn to that task's full result.
	Results   map[string]string
	CWD       string
	SessionID string
	Messages  *Mailbox
}

// Runner executes one task and returns its final text. It must return promptly
// once ctx is cancelled (Stop, shutdown). A non-nil error marks the task failed;
// any text returned alongside it is kept as a partial result.
type Runner func(ctx context.Context, job Job) (string, error)

func (r Run) clone() Run {
	r.Tasks = cloneTasks(r.Tasks)
	r.History = cloneTasks(r.History)
	if r.Checkpoint != nil {
		c := *r.Checkpoint
		c.Payload = append([]byte(nil), c.Payload...)
		c.Response = append([]byte(nil), c.Response...)
		r.Checkpoint = &c
	}
	return r
}

func cloneTasks(in []Task) []Task {
	out := make([]Task, len(in))
	for i, t := range in {
		t.DependsOn = append([]string(nil), t.DependsOn...)
		t.Messages = append([]Instruction(nil), t.Messages...)
		t.AttemptHistory = append([]Attempt(nil), t.AttemptHistory...)
		if t.KeepWorktree != nil {
			v := *t.KeepWorktree
			t.KeepWorktree = &v
		}
		if t.Workspace != nil {
			v := *t.Workspace
			t.Workspace = &v
		}
		if t.Checkpoint != nil {
			v := *t.Checkpoint
			v.Payload = append([]byte(nil), v.Payload...)
			t.Checkpoint = &v
		}
		out[i] = t
	}
	return out
}
