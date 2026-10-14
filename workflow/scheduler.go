package workflow

import (
	"context"
	"fmt"
	"log"
	"time"
)

// Locking: Manager.mu guards every entry. advance, settle, interrupt and launch
// run with it held and never touch the disk or call out; persist (small atomic
// write) is the only I/O under the lock. Runners and onComplete run without it.

// launch starts a fresh execution of e (Start and Resume).
func (m *Manager) launch(e *entry) {
	e.ctx, e.cancel = context.WithCancel(m.ctx)
	e.done = make(chan struct{})
	e.launched = make(map[string]bool)
	e.stopping, e.pausing = false, false
	e.mailboxes = make(map[string]*Mailbox)
	e.threads = make(map[string]threadState)
	m.advance(e)
}

// advance blocks pending tasks whose dependencies failed, starts the ones whose
// dependencies all succeeded, and settles the run once nothing is in flight.
// Callers persist afterwards.
func (m *Manager) advance(e *entry) {
	if !e.stopping && !e.pausing && m.ctx.Err() == nil {
		e.blockFailed()
		for i := range e.run.Tasks {
			t := &e.run.Tasks[i]
			if t.Status == Pending && !e.launched[t.Label] && e.ready(t) {
				if t.Checkpoint != nil {
					t.Status = Paused
					e.run.Checkpoint = &Checkpoint{CheckpointInput: *t.Checkpoint, Label: t.Label, Status: "waiting", CreatedAt: time.Now()}
					e.run.PauseReason = "checkpoint"
					e.pausing = true
					e.cancel()
					break
				}
				e.launched[t.Label] = true
				m.wg.Add(1)
				go m.execute(e, e.ctx, e.run.OwnerSession, t.Label)
			}
		}
	}
	if len(e.launched) == 0 {
		m.settle(e)
	}
}

// settle ends an execution: it picks the run's terminal status and tells the
// owner. On server shutdown it changes nothing, so the saved file still says
// "running" and the next New reports the run as interrupted.
func (m *Manager) settle(e *entry) {
	e.cancel()
	e.cancel = nil
	close(e.done)
	if m.ctx.Err() != nil {
		return
	}
	now := time.Now()
	status := Succeeded
	for i := range e.run.Tasks {
		t := &e.run.Tasks[i]
		if e.stopping && (t.Status == Pending || t.Status == Running) {
			t.Status, t.Error, t.FinishedAt = Stopped, "stopped", now
		}
		if e.pausing && t.Status == Running {
			t.Status = Pending
		}
		if t.Status != Succeeded {
			status = Failed
		}
	}
	if e.pausing {
		status = Paused
	}
	if e.stopping {
		status = Stopped
	}
	e.run.Status, e.run.FinishedAt = status, now
	if status == Paused && e.run.PauseReason == "usage_limit" {
		m.armQuotaResume(e, true)
	} else {
		m.cancelQuotaTimer(e)
	}
	m.persist(e)
	m.notify(e.run)
}

// interrupt is the start-up fix-up for a run found "running" on disk.
func (m *Manager) interrupt(e *entry) {
	now := time.Now()
	for i := range e.run.Tasks {
		if t := &e.run.Tasks[i]; t.Status == Running {
			t.Status, t.Error, t.FinishedAt = Interrupted, "interrupted by restart", now
		}
	}
	e.run.Status, e.run.FinishedAt = Interrupted, now
	m.persist(e)
	m.notify(e.run)
}

// call contains a runner panic inside the task's failure boundary.
func (m *Manager) call(ctx context.Context, job Job) (result string, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = Permanent(fmt.Errorf("panic: %v", p))
		}
	}()
	return m.runner(ctx, job)
}

// complete records a task's outcome and moves its run on.
func (m *Manager) complete(e *entry, label, result string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(e.launched, label)
	t := &e.run.Tasks[e.idx[label]]
	now := time.Now()
	switch {
	case err == nil:
		t.Status, t.Result, t.FinishedAt = Succeeded, result, now
	case e.stopping || e.pausing || m.ctx.Err() != nil:
		// Cancelled from outside, not the task's fault: keep any partial text and
		// leave the state to settle (Stop) or to the next start-up (shutdown).
		t.Result = result
	default:
		t.Status, t.Result, t.Error, t.FinishedAt = Failed, result, err.Error(), now
		if classify(err).Code == "PROVIDER_USAGE_LIMIT" {
			t.Status, e.pausing = Paused, true
			e.run.PauseReason, e.run.ResetHint = "usage_limit", extractResetHint(err.Error())
			e.cancel()
		}
	}
	m.advance(e)
	if e.cancel != nil {
		m.persist(e)
	}
}

func (m *Manager) persist(e *entry) {
	e.run.UpdatedAt = time.Now()
	if err := save(m.dir, e.run); err != nil {
		log.Printf("workflow: save %s: %v", e.run.ID, err)
	}
}

// notify hands a snapshot of r to onComplete on its own goroutine.
func (m *Manager) notify(r Run) {
	if m.onComplete == nil {
		return
	}
	r = r.clone()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer func() {
			if p := recover(); p != nil {
				log.Printf("workflow: OnComplete for %s panicked: %v", r.ID, p)
			}
		}()
		m.onComplete(r)
	}()
}

// ready reports whether every dependency of t succeeded.
func (e *entry) ready(t *Task) bool {
	for _, d := range t.DependsOn {
		if e.run.Tasks[e.idx[d]].Status != Succeeded {
			return false
		}
	}
	return true
}

// blockFailed marks pending tasks blocked when a dependency failed or is itself
// blocked, transitively. Independent branches are left alone.
func (e *entry) blockFailed() {
	now := time.Now()
	for changed := true; changed; {
		changed = false
		for i := range e.run.Tasks {
			t := &e.run.Tasks[i]
			if t.Status != Pending || e.launched[t.Label] {
				continue
			}
			for _, d := range t.DependsOn {
				if s := e.run.Tasks[e.idx[d]].Status; s == Failed || s == Blocked {
					t.Status, t.FinishedAt = Blocked, now
					t.Error = fmt.Sprintf("dependency %q is %s", d, s)
					changed = true
					break
				}
			}
		}
	}
}
