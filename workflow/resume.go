package workflow

import (
	"encoding/json"
	"fmt"
	"reflect"
	"time"
)

// ResumeWithOptions uses task identity rather than position. An edited
// dependency invalidates all its downstream results. Thread turns always run
// again because their evolving session history cannot be replayed as a value.
func (m *Manager) ResumeWithOptions(owner, id string, options ResumeOptions) (Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.resumeLocked(owner, id, options, false)
}

func (m *Manager) resumeLocked(owner, id string, options ResumeOptions, automatic bool) (Run, error) {
	e, err := m.owned(owner, id)
	if err != nil {
		return Run{}, err
	}
	if m.ctx.Err() != nil {
		return Run{}, ErrClosed
	}
	if e.cancel != nil || e.draining != 0 {
		return Run{}, ErrRunning
	}
	before := e.run.clone()
	waiting := before.Checkpoint != nil && before.Checkpoint.Status == "waiting"
	if waiting {
		if options.Tasks != nil {
			return Run{}, fmt.Errorf("%w: answer the waiting checkpoint before editing tasks", ErrInvalid)
		}
		if options.CheckpointID != before.Checkpoint.ID || !json.Valid(options.Response) {
			return Run{}, fmt.Errorf("%w: response must answer checkpoint %q", ErrInvalid, before.Checkpoint.ID)
		}
	} else if options.CheckpointID != "" || len(options.Response) != 0 {
		return Run{}, fmt.Errorf("%w: no checkpoint is waiting", ErrInvalid)
	}
	if before.Status == Succeeded && options.Tasks == nil {
		return Run{}, ErrNothingToResume
	}
	tasks := options.Tasks
	if tasks == nil {
		tasks = before.Tasks
	}
	tasks, err = normalize(tasks)
	if err != nil {
		return Run{}, err
	}
	if err := normalizeTaskDirectories(tasks, before.Options.CWD); err != nil {
		return Run{}, err
	}
	old := make(map[string]Task, len(before.Tasks))
	for _, t := range before.Tasks {
		old[t.Label] = t
	}
	if waiting {
		c := before.Checkpoint
		t := old[c.Label]
		t.Status, t.Result, t.Error, t.FinishedAt = Succeeded, string(options.Response), "", time.Now()
		old[c.Label] = t
	}
	reusable := make(map[string]bool)
	for _, t := range tasks {
		prior, found := old[t.Label]
		reusable[t.Label] = found && prior.Status == Succeeded && t.Thread == "" && reflect.DeepEqual(taskRequest(prior), taskRequest(t))
	}
	// Iterate to a fixed point, independent of request ordering.
	for changed := true; changed; {
		changed = false
		for _, t := range tasks {
			if !reusable[t.Label] {
				continue
			}
			for _, dep := range t.DependsOn {
				if !reusable[dep] {
					reusable[t.Label] = false
					changed = true
					break
				}
			}
		}
	}
	next := before.clone()
	next.Tasks = tasks
	for i, t := range next.Tasks {
		if reusable[t.Label] {
			next.Tasks[i] = cloneTasks([]Task{old[t.Label]})[0]
		}
	}
	for _, t := range before.Tasks {
		if !reusable[t.Label] {
			next.History = append(next.History, t)
		}
	}
	if waiting {
		next.Checkpoint.Status, next.Checkpoint.Response, next.Checkpoint.ConsumedAt = "consumed", append([]byte(nil), options.Response...), time.Now()
	}
	next.Status, next.FinishedAt, next.UpdatedAt = Running, time.Time{}, time.Now()
	next.PauseReason, next.ResetHint, next.ResumeAt = "", "", time.Time{}
	if !automatic {
		next.AutoResumeAttempts = 0
	}
	next.Revision++
	if err := save(m.dir, next); err != nil {
		return Run{}, err
	}
	m.cancelQuotaTimer(e)
	e.quotaRefusals = 0
	e.run, e.idx = next, newEntry(next).idx
	m.launch(e)
	return e.run.clone(), nil
}
