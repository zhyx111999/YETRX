package workflow

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"
)

// A timed-out runner gets the same ten-second drain grace as pi. It cannot
// overlap a retry. An uncooperative runner keeps its slot and blocks resume
// until it really returns, even after the task has reported failure.
var drainGrace = 10 * time.Second

type threadState struct {
	cwd, session string
	busy         bool
}
type outcome struct {
	text string
	err  error
}

func (m *Manager) execute(e *entry, ctx context.Context, label string) {
	defer m.wg.Done()
	select {
	case m.sem <- struct{}{}:
		defer func() { <-m.sem }()
	case <-ctx.Done():
		m.complete(e, label, "", ctx.Err())
		return
	}
	if ctx.Err() != nil {
		m.complete(e, label, "", ctx.Err())
		return
	}
	m.mu.Lock()
	task := cloneTasks([]Task{e.run.Tasks[e.idx[label]]})[0]
	base := e.run.Options.CWD
	m.mu.Unlock()
	workspace, err := createWorkspace(ctx, base, task)
	if err != nil {
		m.complete(e, label, "", err)
		return
	}
	m.mu.Lock()
	e.run.Tasks[e.idx[label]].Workspace = workspace
	m.mu.Unlock()
	text, err, draining := m.attempts(e, ctx, label, workspace.CWD)
	cleanup := func() {
		if task.KeepWorktree != nil && !*task.KeepWorktree {
			copy := *workspace
			removeWorkspace(&copy)
			m.mu.Lock()
			*workspace = copy
			m.mu.Unlock()
		}
	}
	if draining != nil {
		m.mu.Lock()
		e.draining++
		e.run.Tasks[e.idx[label]].Draining = true
		m.mu.Unlock()
		m.complete(e, label, text, err)
		<-draining
		cleanup()
		m.mu.Lock()
		e.draining--
		e.run.Tasks[e.idx[label]].Draining = false
		m.persist(e)
		m.mu.Unlock()
		return
	}
	cleanup()
	m.complete(e, label, text, err)
}

func (m *Manager) attempts(e *entry, ctx context.Context, label, cwd string) (string, error, <-chan outcome) {
	m.mu.Lock()
	options := e.run.Options
	m.mu.Unlock()
	var text string
	var err error
	for number := 1; number <= options.AgentRetries+1; number++ {
		if ctx.Err() != nil {
			return text, ctx.Err(), nil
		}
		job, beginErr := m.beginAttempt(e, ctx, label, cwd)
		if beginErr != nil {
			return text, beginErr, nil
		}
		attemptCtx, cancel := context.WithCancel(ctx)
		if options.AgentTimeoutMS > 0 {
			cancel()
			attemptCtx, cancel = context.WithTimeout(ctx, time.Duration(options.AgentTimeoutMS)*time.Millisecond)
		}
		result, draining := m.runAttempt(attemptCtx, job)
		cancel()
		text, err = result.text, result.err
		if err == nil && strings.TrimSpace(text) == "" {
			err = &Failure{Code: "AGENT_EMPTY_OUTPUT", Recoverable: true, Cause: fmt.Errorf("subagent completed without assistant text")}
		}
		job.Messages.close()
		m.mu.Lock()
		delete(e.mailboxes, label)
		t := &e.run.Tasks[e.idx[label]]
		a := &t.AttemptHistory[len(t.AttemptHistory)-1]
		a.FinishedAt = time.Now()
		failure := classify(err)
		if failure != nil {
			a.Error, a.Code, a.Recoverable = failure.Error(), failure.Code, failure.Recoverable
		}
		if t.Thread != "" && draining == nil {
			state := e.threads[t.Thread]
			state.busy = false
			e.threads[t.Thread] = state
		}
		finished := err == nil || draining != nil || ctx.Err() != nil || !failure.Recoverable || number > options.AgentRetries
		if !finished {
			m.persist(e)
		}
		m.mu.Unlock()
		if finished {
			return text, err, draining
		}
		timer := time.NewTimer(min(250*time.Millisecond*time.Duration(1<<(number-1)), 2*time.Second))
		select {
		case <-ctx.Done():
			timer.Stop()
			return text, ctx.Err(), nil
		case <-timer.C:
		}
	}
	return text, err, nil
}

func (m *Manager) beginAttempt(e *entry, ctx context.Context, label, cwd string) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ctx.Err() != nil {
		return Job{}, ctx.Err()
	}
	t := &e.run.Tasks[e.idx[label]]
	sessionKey := fmt.Sprintf("%s/%d/%s/%d", e.run.ID, e.run.Revision, label, t.Attempts+1)
	if t.Thread != "" {
		sessionKey = fmt.Sprintf("%s/%d/thread/%s", e.run.ID, e.run.Revision, t.Thread)
	}
	hash := sha256.Sum256([]byte(sessionKey))
	session := fmt.Sprintf("workflow-%s-%x", e.run.ID, hash[:8])
	if t.Thread != "" {
		state, exists := e.threads[t.Thread]
		if exists && (state.busy || state.cwd != cwd) {
			return Job{}, Permanent(fmt.Errorf("thread %q is busy or has a different cwd", t.Thread))
		}
		e.threads[t.Thread] = threadState{cwd: cwd, session: session, busy: true}
	}
	t.Attempts++
	t.Status, t.SessionID = Running, session
	if t.StartedAt.IsZero() {
		t.StartedAt = time.Now()
	}
	t.AttemptHistory = append(t.AttemptHistory, Attempt{Number: t.Attempts, SessionID: session, StartedAt: time.Now()})
	box := &Mailbox{}
	e.mailboxes[label] = box
	m.persist(e)
	job := Job{RunID: e.run.ID, OwnerSession: e.run.OwnerSession, OwnerAgent: e.run.OwnerAgent,
		Depth: e.run.Depth + 1, Task: cloneTasks([]Task{*t})[0], CWD: cwd, SessionID: session, Messages: box,
		Results: make(map[string]string, len(t.DependsOn))}
	for _, d := range t.DependsOn {
		job.Results[d] = e.run.Tasks[e.idx[d]].Result
	}
	return job, nil
}

func (m *Manager) runAttempt(ctx context.Context, job Job) (outcome, <-chan outcome) {
	done := make(chan outcome, 1)
	go func() { text, err := m.call(ctx, job); done <- outcome{text, err} }()
	select {
	case result := <-done:
		if ctx.Err() != nil {
			result.err = ctx.Err()
		}
		return result, nil
	case <-ctx.Done():
		timer := time.NewTimer(drainGrace)
		defer timer.Stop()
		select {
		case result := <-done:
			result.err = ctx.Err()
			return result, nil
		case <-timer.C:
			return outcome{err: ctx.Err()}, done
		}
	}
}
