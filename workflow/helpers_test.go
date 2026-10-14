package workflow

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Durable Windows writes can stall under the filesystem scanner. This only
// bounds test polling; production attempt and stop deadlines are unchanged.
const wait = 20 * time.Second

// env is a Manager under test plus the completions it reported.
type env struct {
	t      *testing.T
	dir    string
	m      *Manager
	cancel context.CancelFunc
	done   chan Run
	hook   func(Run) // optional; set before the first Start
}

func open(t *testing.T, dir string, concurrency int, runner Runner) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e := &env{t: t, dir: dir, cancel: cancel, done: make(chan Run, 256)}
	m, err := New(ctx, dir, concurrency, runner, func(r Run) {
		if e.hook != nil {
			e.hook(r)
		}
		e.done <- r
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	e.m = m
	t.Cleanup(e.close)
	return e
}

// close simulates the server going away.
func (e *env) close() {
	e.cancel()
	e.m.Wait()
}

func (e *env) start(owner string, tasks ...Task) string {
	e.t.Helper()
	id, err := e.m.Start(owner, "main", 0, tasks)
	if err != nil {
		e.t.Fatalf("Start: %v", err)
	}
	return id
}

// next returns the next completion notification.
func (e *env) next() Run {
	e.t.Helper()
	select {
	case r := <-e.done:
		return r
	case <-time.After(wait):
		e.m.mu.Lock()
		for _, entry := range e.m.runs {
			e.t.Logf("stalled run %s: status=%s launched=%v stopping=%t tasks=%+v", entry.run.ID, entry.run.Status, entry.launched, entry.stopping, entry.run.Tasks)
		}
		e.m.mu.Unlock()
		e.t.Fatal("no completion notification")
		return Run{}
	}
}

// quiet fails if a completion notification arrives within d.
func (e *env) quiet(d time.Duration) {
	e.t.Helper()
	select {
	case r := <-e.done:
		e.t.Fatalf("unexpected notification: run %s is %s", r.ID, r.Status)
	case <-time.After(d):
	}
}

func (e *env) status(owner, id string) Run {
	e.t.Helper()
	r, err := e.m.Status(owner, id)
	if err != nil {
		e.t.Fatalf("Status: %v", err)
	}
	return r
}

// eventually polls cond until it holds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(wait)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func task(label string, deps ...string) Task {
	return Task{Label: label, Task: "do " + label, DependsOn: deps}
}

func independent(n int) []Task {
	out := make([]Task, n)
	for i := range out {
		out[i] = task(fmt.Sprintf("t%d", i))
	}
	return out
}

func byLabel(t *testing.T, r Run, label string) Task {
	t.Helper()
	for _, tk := range r.Tasks {
		if tk.Label == label {
			return tk
		}
	}
	t.Fatalf("run has no task %q", label)
	return Task{}
}

func wantStatus(t *testing.T, r Run, run Status, tasks map[string]Status) {
	t.Helper()
	if r.Status != run {
		t.Errorf("run status = %s, want %s", r.Status, run)
	}
	for label, want := range tasks {
		if got := byLabel(t, r, label).Status; got != want {
			t.Errorf("task %s = %s, want %s", label, got, want)
		}
	}
}

// probe records what a Runner saw: call counts, event order, and the real
// number of tasks executing at once, overall and per owner session.
type probe struct {
	mu       sync.Mutex
	calls    map[string]int
	events   []string // "start:x" / "end:x"
	cur      int
	max      int
	owners   map[string]int // executing now, by owner session
	ownerMax map[string]int
	started  chan string
}

func newProbe() *probe {
	return &probe{calls: map[string]int{}, owners: map[string]int{}, ownerMax: map[string]int{}, started: make(chan string, 256)}
}

// run wraps body with bookkeeping.
func (p *probe) run(body Runner) Runner {
	return func(ctx context.Context, job Job) (string, error) {
		label, owner := job.Task.Label, job.OwnerSession
		p.mu.Lock()
		p.calls[label]++
		p.cur++
		p.max = max(p.max, p.cur)
		p.owners[owner]++
		p.ownerMax[owner] = max(p.ownerMax[owner], p.owners[owner])
		p.events = append(p.events, "start:"+label)
		p.mu.Unlock()
		p.started <- label
		defer func() {
			p.mu.Lock()
			p.cur--
			p.owners[owner]--
			p.events = append(p.events, "end:"+label)
			p.mu.Unlock()
		}()
		return body(ctx, job)
	}
}

func (p *probe) called(label string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[label]
}

func (p *probe) peak() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.max
}

func (p *probe) running() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cur
}

// runningFor and peakFor are running and peak for one owner session.
func (p *probe) runningFor(owner string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.owners[owner]
}

func (p *probe) peakFor(owner string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ownerMax[owner]
}

// expectRunning waits for n tasks to be executing, then gives an over-eager
// scheduler time to start more and fails if it did.
func (p *probe) expectRunning(t *testing.T, n int) {
	t.Helper()
	eventually(t, fmt.Sprintf("%d tasks executing", n), func() bool { return p.running() >= n })
	time.Sleep(150 * time.Millisecond)
	if got := p.running(); got != n {
		t.Fatalf("%d tasks executing, want exactly %d", got, n)
	}
}

func (p *probe) index(event string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, ev := range p.events {
		if ev == event {
			return i
		}
	}
	return -1
}

// waitStarted returns the label of the next task that began executing.
func (p *probe) waitStarted(t *testing.T) string {
	t.Helper()
	select {
	case l := <-p.started:
		return l
	case <-time.After(wait):
		t.Fatal("no task started")
		return ""
	}
}

// block waits for release or cancellation.
func block(release <-chan struct{}) Runner {
	return func(ctx context.Context, job Job) (string, error) {
		select {
		case <-release:
			return "out-" + job.Task.Label, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}
