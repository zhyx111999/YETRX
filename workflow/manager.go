package workflow

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// stopGrace bounds how long Stop waits for a Runner that ignores cancellation.
var stopGrace = 5 * time.Second

// Manager owns all runs. Every method is safe for concurrent use, and every
// method that takes an ownerSession only ever sees that session's runs.
type Manager struct {
	ctx        context.Context
	dir        string
	runner     Runner
	onComplete func(Run)
	limiter    *sessionLimiter // running-task slots, separate for every owner session
	wg         sync.WaitGroup

	mu   sync.Mutex // guards runs and every entry
	runs map[string]*entry
}

// entry is the live bookkeeping of one run.
type entry struct {
	run           Run
	idx           map[string]int  // label -> index into run.Tasks
	ctx           context.Context // current execution; cancel != nil while tasks are in flight
	cancel        context.CancelFunc
	done          chan struct{}   // closed when the current execution has drained
	launched      map[string]bool // tasks with a goroutine in flight (waiting for a session slot or running)
	stopping      bool
	pausing       bool
	draining      int
	mailboxes     map[string]*Mailbox
	threads       map[string]threadState
	quotaTimer    *time.Timer
	quotaRefusals int
}

// New loads the runs saved in dir and returns a Manager whose work lives as long
// as ctx. Runs found still "running" (the process died or shut down under them)
// are marked interrupted, saved, and reported through onComplete once; nothing is
// re-run. concurrency is the number of tasks one owner session may have running
// at once, shared by all of that session's runs; other sessions have their own.
// concurrency <= 0 means DefaultConcurrency. onComplete may be nil.
//
// onComplete receives a snapshot of a run each time it reaches a terminal state
// (succeeded, failed, stopped, interrupted or paused). It is called from its own goroutine
// without any Manager lock held, so it may call back into the Manager.
func New(ctx context.Context, dir string, concurrency int, runner Runner, onComplete func(Run)) (*Manager, error) {
	if runner == nil {
		return nil, errors.New("workflow: nil Runner")
	}
	if concurrency <= 0 {
		concurrency = DefaultConcurrency
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	saved, err := loadAll(dir)
	if err != nil {
		return nil, err
	}
	m := &Manager{
		ctx: ctx, dir: dir, runner: runner, onComplete: onComplete,
		limiter: newSessionLimiter(concurrency), runs: make(map[string]*entry, len(saved)),
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range saved {
		r.Revision = max(1, r.Revision)
		r.Options.AgentRetries = min(MaxAgentRetries, max(0, r.Options.AgentRetries))
		r.AutoResumeAttempts = min(quotaMaxAttempts+1, max(0, r.AutoResumeAttempts))
		for i := range r.Tasks {
			r.Tasks[i].Draining = false
		}
		e := newEntry(r)
		m.runs[r.ID] = e
		if r.Status == Running {
			m.interrupt(e)
		} else if r.Status == Paused && r.PauseReason == "usage_limit" {
			m.armQuotaResume(e, false)
		}
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		<-ctx.Done()
		m.mu.Lock()
		defer m.mu.Unlock()
		for _, e := range m.runs {
			m.cancelQuotaTimer(e)
		}
	}()
	return m, nil
}

// SetConcurrency changes every owner's admission limit without stopping live
// runners. Queued tasks are woken immediately when capacity increases.
func (m *Manager) SetConcurrency(limit int) error {
	if limit <= 0 {
		return fmt.Errorf("%w: concurrency must be positive", ErrInvalid)
	}
	m.limiter.setLimit(limit)
	return nil
}

func (m *Manager) Concurrency() int { return m.limiter.currentLimit() }

// Start validates tasks and begins running them in the background; it returns
// the run id at once. depth is the caller's nesting depth (0 for the top-level
// assistant; use Job.Depth inside a Runner). Tasks without DependsOn start
// immediately, in parallel, subject to the owner session's concurrency limit.
func (m *Manager) Start(ownerSession, ownerAgent string, depth int, tasks []Task) (string, error) {
	return m.StartWithOptions(ownerSession, ownerAgent, depth, tasks, Options{})
}

func (m *Manager) StartWithOptions(ownerSession, ownerAgent string, depth int, tasks []Task, options Options) (string, error) {
	switch {
	case ownerSession == "" || depth < 0:
		return "", fmt.Errorf("%w: owner session required, depth >= 0", ErrInvalid)
	case depth >= MaxDepth:
		return "", fmt.Errorf("%w: depth %d, limit %d", ErrDepth, depth, MaxDepth)
	}
	tasks, err := normalize(tasks)
	if err != nil {
		return "", err
	}
	options, err = normalizeOptions(options)
	if err != nil {
		return "", err
	}
	if err := normalizeTaskDirectories(tasks, options.CWD); err != nil {
		return "", err
	}
	now := time.Now()
	e := newEntry(Run{
		ID: newID(), OwnerSession: ownerSession, OwnerAgent: ownerAgent, Depth: depth,
		Status: Running, Tasks: tasks, Options: options, Revision: 1, CreatedAt: now, UpdatedAt: now,
	})

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx.Err() != nil {
		return "", ErrClosed
	}
	if err := save(m.dir, e.run); err != nil {
		return "", err
	}
	m.runs[e.run.ID] = e
	m.launch(e)
	return e.run.ID, nil
}

// List returns the owner's runs, newest first.
func (m *Manager) List(ownerSession string) []Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Run
	for _, e := range m.runs {
		if ownerSession != "" && e.run.OwnerSession == ownerSession {
			out = append(out, e.run.clone())
		}
	}
	slices.SortFunc(out, func(a, b Run) int {
		return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out
}

// Status returns a snapshot of one run.
func (m *Manager) Status(ownerSession, id string) (Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, err := m.owned(ownerSession, id)
	if err != nil {
		return Run{}, err
	}
	return e.run.clone(), nil
}

// Read returns the last maxChars characters of a task's result (default and
// maximum MaxReadChars). A failed or stopped task may still have a partial result.
func (m *Manager) Read(ownerSession, id, label string, maxChars int) (string, error) {
	if maxChars <= 0 || maxChars > MaxReadChars {
		maxChars = MaxReadChars
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, err := m.owned(ownerSession, id)
	if err != nil {
		return "", err
	}
	i, ok := e.idx[strings.TrimSpace(label)]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrNoTask, label)
	}
	t := e.run.Tasks[i]
	if t.Result == "" {
		reason := string(t.Status)
		if t.Error != "" {
			reason += ": " + t.Error
		}
		return "", fmt.Errorf("%w: %q is %s", ErrNoResult, t.Label, reason)
	}
	return tail(t.Result, maxChars), nil
}

// Stop cancels a running run: tasks in flight get a cancelled ctx, tasks not yet
// started are dropped, tasks that already succeeded keep their results. It waits
// for the Runners to return (up to stopGrace) and returns the resulting snapshot,
// normally already stopped. The owner is notified through onComplete as usual.
func (m *Manager) Stop(ownerSession, id string) (Run, error) {
	m.mu.Lock()
	e, err := m.owned(ownerSession, id)
	if err != nil {
		m.mu.Unlock()
		return Run{}, err
	}
	if e.cancel == nil {
		if e.run.Status == Paused {
			m.cancelQuotaTimer(e)
			e.run.Status, e.run.FinishedAt, e.run.ResumeAt = Stopped, time.Now(), time.Time{}
			m.persist(e)
			m.notify(e.run)
			run := e.run.clone()
			m.mu.Unlock()
			return run, nil
		}
		m.mu.Unlock()
		return Run{}, fmt.Errorf("%w: it is %s", ErrNotRunning, e.run.Status)
	}
	e.stopping = true
	e.cancel()
	done := e.done
	m.mu.Unlock()

	select {
	case <-done:
	case <-time.After(stopGrace):
	}
	return m.Status(ownerSession, id)
}

// Resume preserves reusable successful calls and reruns unfinished work.
func (m *Manager) Resume(ownerSession, id string) (Run, error) {
	return m.ResumeWithOptions(ownerSession, id, ResumeOptions{})
}

// Wait blocks until the ctx given to New is cancelled and every task and
// onComplete goroutine has returned. Call it on shutdown, or in tests before
// reopening the same directory.
func (m *Manager) Wait() {
	<-m.ctx.Done()
	m.mu.Lock() // Start and Resume add to wg under mu after checking ctx
	m.mu.Unlock()
	m.wg.Wait()
}

func (m *Manager) owned(ownerSession, id string) (*entry, error) {
	e := m.runs[id]
	if ownerSession == "" || e == nil || e.run.OwnerSession != ownerSession {
		return nil, ErrNotFound
	}
	return e, nil
}

func newEntry(r Run) *entry {
	e := &entry{run: r, idx: make(map[string]int, len(r.Tasks))}
	for i, t := range r.Tasks {
		e.idx[t.Label] = i
	}
	return e
}

// tail returns the last n characters of s.
func tail(s string, n int) string {
	if len(s) <= n { // at most len(s) characters
		return s
	}
	i := len(s)
	for ; n > 0 && i > 0; n-- {
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
	}
	return s[i:]
}
