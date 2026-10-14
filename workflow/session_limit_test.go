package workflow

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// The default is deliberately small; these tests use the larger figure a
// deployment may configure, to show the limit is per session and not baked in.
const sessionLimit = 16

func TestEachSessionReachesTheConfiguredLimit(t *testing.T) {
	const queued = 4
	release := make(chan struct{})
	p := newProbe()
	e := open(t, t.TempDir(), sessionLimit, p.run(block(release)))

	sessions := []string{"conv-1", "conv-2"}
	for _, s := range sessions {
		e.start(s, independent(sessionLimit+queued)...)
	}
	p.expectRunning(t, len(sessions)*sessionLimit)
	for _, s := range sessions {
		if n := p.runningFor(s); n != sessionLimit {
			t.Errorf("%s has %d tasks executing, want %d", s, n, sessionLimit)
		}
	}

	close(release)
	e.next()
	e.next()
	for _, s := range sessions {
		if peak := p.peakFor(s); peak != sessionLimit {
			t.Errorf("%s peak = %d, want %d", s, peak, sessionLimit)
		}
	}
	for i := range sessionLimit + queued {
		if n := p.called(fmt.Sprintf("t%d", i)); n != len(sessions) {
			t.Errorf("t%d ran %d times, want %d", i, n, len(sessions))
		}
	}
	eventually(t, "every session slot released", func() bool { return e.m.limiter.sessions() == 0 })
}

func TestRunsOfOneSessionShareTheLimit(t *testing.T) {
	release := make(chan struct{})
	p := newProbe()
	e := open(t, t.TempDir(), sessionLimit, p.run(block(release)))

	first := e.start("conv-1", independent(12)...)
	second := e.start("conv-1", independent(12)...)
	p.expectRunning(t, sessionLimit)

	var running, pending int
	for _, id := range []string{first, second} {
		for _, tk := range e.status("conv-1", id).Tasks {
			switch tk.Status {
			case Running:
				running++
			case Pending:
				pending++
			}
		}
	}
	if running != sessionLimit || pending != 24-sessionLimit {
		t.Errorf("running/pending = %d/%d across both runs, want %d/%d", running, pending, sessionLimit, 24-sessionLimit)
	}

	close(release)
	for range 2 {
		wantStatus(t, e.next(), Succeeded, nil)
	}
	if peak := p.peakFor("conv-1"); peak != sessionLimit {
		t.Errorf("peak = %d, want %d", peak, sessionLimit)
	}
	eventually(t, "every session slot released", func() bool { return e.m.limiter.sessions() == 0 })
}

// A timed-out runner that ignores cancellation keeps its session's slot until it
// really returns, and that must hold up nobody else.
func TestDrainingRunnerOnlyHoldsItsOwnSession(t *testing.T) {
	old := drainGrace
	drainGrace = 20 * time.Millisecond
	defer func() { drainGrace = old }()

	release := make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	p := newProbe()
	e := open(t, t.TempDir(), 1, p.run(func(ctx context.Context, job Job) (string, error) {
		if job.Task.Label == "deaf" {
			<-release // deaf to ctx
		}
		return "ok", nil
	}))

	if _, err := e.m.StartWithOptions("conv-1", "agent", 0, []Task{task("deaf")}, Options{AgentTimeoutMS: 10}); err != nil {
		t.Fatal(err)
	}
	if r := e.next(); r.Status != Failed || !r.Tasks[0].Draining {
		t.Fatalf("deaf run = %s, draining=%t", r.Status, r.Tasks[0].Draining)
	}
	sameSession := e.start("conv-1", task("behind"))
	otherSession := e.start("conv-2", task("free"))

	if r := e.next(); r.ID != otherSession || r.Status != Succeeded {
		t.Fatalf("first completion = %s %s, want conv-2's run to finish while conv-1 drains", r.ID, r.Status)
	}
	e.quiet(100 * time.Millisecond)
	if p.called("behind") != 0 {
		t.Fatal("the slot of a runner that had not returned was handed to its own session's next task")
	}
	if n := e.m.limiter.users("conv-1"); n != 2 {
		t.Errorf("conv-1 users = %d, want the draining task and the queued one", n)
	}

	unblock.Do(func() { close(release) })
	if r := e.next(); r.ID != sameSession || r.Status != Succeeded {
		t.Fatalf("after the drain: %s %s", r.ID, r.Status)
	}
	eventually(t, "every session slot released", func() bool { return e.m.limiter.sessions() == 0 })
}

// Stop on a session whose runner ignores cancellation still waits for the real
// exit, drops that session's queue at once, and never delays another session.
func TestStoppingADeafSessionLeavesOthersRunning(t *testing.T) {
	old := stopGrace
	stopGrace = 50 * time.Millisecond
	defer func() { stopGrace = old }()

	release := make(chan struct{})
	var unblock sync.Once
	defer unblock.Do(func() { close(release) })
	p := newProbe()
	e := open(t, t.TempDir(), 1, p.run(func(ctx context.Context, job Job) (string, error) {
		if job.OwnerSession == "conv-1" {
			<-release // deaf to ctx
		}
		return "ok", nil
	}))

	// Whichever of s0..s2 wins the only slot is deaf; the other two queue behind it.
	stuck := e.start("conv-1", task("s0"), task("s1"), task("s2"))
	p.waitStarted(t)
	eventually(t, "conv-1's queue", func() bool { return e.m.limiter.users("conv-1") == 3 })

	r, err := e.m.Stop("conv-1", stuck)
	if err != nil || r.Status != Running {
		t.Fatalf("Stop = %s, %v; want it to return while the deaf task is still alive", r.Status, err)
	}
	if n := e.m.limiter.users("conv-1"); n != 1 {
		t.Errorf("conv-1 users after Stop = %d, want only the live runner", n)
	}

	other := e.start("conv-2", independent(3)...)
	if r := e.next(); r.ID != other || r.Status != Succeeded {
		t.Fatalf("conv-2 = %s %s while conv-1 was being stopped", r.ID, r.Status)
	}

	unblock.Do(func() { close(release) })
	if r := e.next(); r.ID != stuck || r.Status != Stopped {
		t.Fatalf("stopped run = %s %s", r.ID, r.Status)
	}
	if n := p.called("s0") + p.called("s1") + p.called("s2"); n != 1 {
		t.Errorf("conv-1 ran %d tasks, want only the one that held the slot", n)
	}
	eventually(t, "every session slot released", func() bool { return e.m.limiter.sessions() == 0 })
}

func TestStoppingQueuedRunReleasesItsWaitingTasks(t *testing.T) {
	release := make(chan struct{})
	p := newProbe()
	e := open(t, t.TempDir(), 2, p.run(block(release)))

	holder := e.start("conv-1", task("hold-a"), task("hold-b"))
	p.expectRunning(t, 2)
	queued := e.start("conv-1", task("q0"), task("q1"), task("q2"))
	eventually(t, "three tasks waiting behind two", func() bool { return e.m.limiter.users("conv-1") == 5 })

	r, err := e.m.Stop("conv-1", queued)
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, r, Stopped, map[string]Status{"q0": Stopped, "q1": Stopped, "q2": Stopped})
	if n := e.m.limiter.users("conv-1"); n != 2 {
		t.Errorf("users after cancelling the queued run = %d, want the 2 running tasks", n)
	}
	if n := e.next(); n.ID != queued || n.Status != Stopped {
		t.Fatalf("notification = %s %s", n.ID, n.Status)
	}

	close(release)
	if n := e.next(); n.ID != holder || n.Status != Succeeded {
		t.Fatalf("notification = %s %s", n.ID, n.Status)
	}
	for _, label := range []string{"q0", "q1", "q2"} {
		if p.called(label) != 0 {
			t.Errorf("cancelled task %s ran", label)
		}
	}
	eventually(t, "every session slot released", func() bool { return e.m.limiter.sessions() == 0 })
}

func TestShutdownLeavesNoQueuedWaiters(t *testing.T) {
	p := newProbe()
	e := open(t, t.TempDir(), 1, p.run(block(nil)))
	for _, s := range []string{"conv-1", "conv-2"} {
		e.start(s, independent(4)...)
	}
	p.expectRunning(t, 2)
	for _, s := range []string{"conv-1", "conv-2"} {
		eventually(t, s+" queued", func() bool { return e.m.limiter.users(s) == 4 })
	}

	e.close()
	if n := e.m.limiter.sessions(); n != 0 {
		t.Errorf("%d sessions still tracked after shutdown", n)
	}
	if n := p.running(); n != 0 {
		t.Errorf("%d tasks still executing after shutdown", n)
	}
	var started int
	for i := range 4 {
		started += p.called(fmt.Sprintf("t%d", i))
	}
	if started != 2 { // the one task per session that held its slot
		t.Errorf("%d tasks started, want 2: queued tasks must not start during shutdown", started)
	}
}
