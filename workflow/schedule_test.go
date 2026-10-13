package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIndependentTasksRunInParallelThenDependentsFollow(t *testing.T) {
	release := make(chan struct{})
	p := newProbe()
	e := open(t, t.TempDir(), 4, p.run(func(ctx context.Context, job Job) (string, error) {
		if job.Task.Label != "c" { // a and b wait for each other's start via the test
			return block(release)(ctx, job)
		}
		return "c saw " + job.Results["a"] + "+" + job.Results["b"], nil
	}))

	id := e.start("conv-1", task("a"), task("b"), task("c", "a", "b"))
	got := map[string]bool{p.waitStarted(t): true, p.waitStarted(t): true}
	if !got["a"] || !got["b"] {
		t.Fatalf("a and b must start together, started %v", got)
	}
	if p.called("c") != 0 {
		t.Fatal("c started before its dependencies finished")
	}
	if r := e.status("conv-1", id); r.Status != Running || byLabel(t, r, "c").Status != Pending {
		t.Fatalf("mid-flight snapshot: %+v", r)
	}

	close(release)
	r := e.next()
	wantStatus(t, r, Succeeded, map[string]Status{"a": Succeeded, "b": Succeeded, "c": Succeeded})
	if res := byLabel(t, r, "c").Result; res != "c saw out-a+out-b" {
		t.Errorf("dependency results not passed: %q", res)
	}
	if p.index("start:c") < max(p.index("end:a"), p.index("end:b")) {
		t.Errorf("c started before a and b ended: %v", p.events)
	}
}

func TestJobCarriesOwnerAndDepth(t *testing.T) {
	jobs := make(chan Job, 1)
	profile := int64(5)
	e := open(t, t.TempDir(), 1, func(ctx context.Context, job Job) (string, error) {
		jobs <- job
		return "ok", nil
	})
	id, err := e.m.Start("conv-9", "main", 1, []Task{{Label: "a", Task: "go", AgentKey: "k", LLMProfileID: &profile}})
	if err != nil {
		t.Fatal(err)
	}
	job := <-jobs
	e.next()
	if job.RunID != id || job.OwnerSession != "conv-9" || job.OwnerAgent != "main" || job.Depth != 2 ||
		job.Task.Label != "a" || job.Task.AgentKey != "k" || *job.Task.LLMProfileID != 5 {
		t.Errorf("job = %+v", job)
	}
}

func TestGlobalConcurrencyLimit(t *testing.T) {
	const limit = 3
	release := make(chan struct{})
	p := newProbe()
	e := open(t, t.TempDir(), limit, p.run(block(release)))

	// Two owners, five independent tasks each: ten runnable at once, three allowed.
	e.start("conv-1", independent(5)...)
	e.start("conv-2", independent(5)...)
	for range limit {
		p.waitStarted(t)
	}
	time.Sleep(150 * time.Millisecond) // time for an over-eager scheduler to overshoot
	if n := p.running(); n != limit {
		t.Fatalf("%d tasks executing, want exactly %d", n, limit)
	}
	if len(p.started) != 0 {
		t.Fatal("a task beyond the limit started")
	}

	close(release)
	e.next()
	e.next()
	if p.peak() != limit {
		t.Errorf("peak concurrency = %d, want %d", p.peak(), limit)
	}
	total := 0
	for i := range 5 {
		total += p.called(fmt.Sprintf("t%d", i))
	}
	if total != 10 { // the same five labels in both runs
		t.Errorf("tasks executed = %d, want 10", total)
	}
}

func TestFailureBlocksDependentsOnly(t *testing.T) {
	p := newProbe()
	e := open(t, t.TempDir(), 4, p.run(func(ctx context.Context, job Job) (string, error) {
		if job.Task.Label == "bad" {
			return "partial output", errors.New("boom")
		}
		return "ok", nil
	}))
	id := e.start("conv-1",
		task("bad"), task("mid", "bad"), task("leaf", "mid"), // blocked chain
		task("good"), task("after-good", "good"), // independent branch
		task("joined", "good", "bad"), // one good, one bad dependency
	)
	r := e.next()
	wantStatus(t, r, Failed, map[string]Status{
		"bad": Failed, "mid": Blocked, "leaf": Blocked, "good": Succeeded, "after-good": Succeeded, "joined": Blocked,
	})
	for _, l := range []string{"mid", "leaf", "joined"} {
		if p.called(l) != 0 {
			t.Errorf("blocked task %s was executed", l)
		}
	}
	if bad := byLabel(t, r, "bad"); bad.Error != "boom" || bad.Result != "partial output" {
		t.Errorf("failed task = %+v", bad)
	}
	if msg := byLabel(t, r, "leaf").Error; !strings.Contains(msg, "mid") {
		t.Errorf("blocked reason = %q", msg)
	}
	got, err := e.m.Read("conv-1", id, "bad", 0)
	if err != nil || got != "partial output" {
		t.Errorf("Read of failed task = %q, %v", got, err)
	}
}

func TestRunnerPanicFailsTaskAndReleasesSlot(t *testing.T) {
	e := open(t, t.TempDir(), 1, func(ctx context.Context, job Job) (string, error) {
		if job.Task.Label == "boom" {
			panic("kaput")
		}
		return "fine", nil
	})
	e.start("conv-1", task("boom"))
	r := e.next()
	if tk := byLabel(t, r, "boom"); r.Status != Failed || !strings.Contains(tk.Error, "kaput") {
		t.Errorf("panic run = %s / %+v", r.Status, tk)
	}
	e.start("conv-1", task("next")) // concurrency is 1: only works if the slot was released
	wantStatus(t, e.next(), Succeeded, map[string]Status{"next": Succeeded})
}

func TestStop(t *testing.T) {
	var cancelled atomic.Int32
	p := newProbe()
	e := open(t, t.TempDir(), 4, p.run(func(ctx context.Context, job Job) (string, error) {
		if job.Task.Label != "slow" {
			return "done-" + job.Task.Label, nil
		}
		<-ctx.Done()
		cancelled.Add(1)
		return "partial", ctx.Err()
	}))
	id := e.start("conv-1", task("quick"), task("slow"), task("after", "slow"), task("after-quick", "quick"))
	eventually(t, "quick tasks done", func() bool {
		r := e.status("conv-1", id)
		return byLabel(t, r, "quick").Status == Succeeded && byLabel(t, r, "after-quick").Status == Succeeded
	})

	r, err := e.m.Stop("conv-1", id)
	if err != nil {
		t.Fatal(err)
	}
	wantStatus(t, r, Stopped, map[string]Status{
		"quick": Succeeded, "after-quick": Succeeded, "slow": Stopped, "after": Stopped,
	})
	if cancelled.Load() != 1 {
		t.Error("the running task's ctx was not cancelled")
	}
	if byLabel(t, r, "quick").Result != "done-quick" || byLabel(t, r, "slow").Result != "partial" {
		t.Errorf("results after stop: %+v", r.Tasks)
	}
	if p.called("after") != 0 {
		t.Error("a dependent of the stopped task ran")
	}

	if n := e.next(); n.Status != Stopped || n.ID != id {
		t.Errorf("notification = %s %s", n.ID, n.Status)
	}
	e.quiet(100 * time.Millisecond) // once, not twice
	if _, err := e.m.Stop("conv-1", id); !errors.Is(err, ErrNotRunning) {
		t.Errorf("second Stop = %v, want ErrNotRunning", err)
	}
}

func TestStopWhileWaitingForSlot(t *testing.T) {
	p := newProbe()
	e := open(t, t.TempDir(), 1, p.run(block(nil)))
	id := e.start("conv-1", independent(4)...)
	p.waitStarted(t) // one running, three queued behind the single slot
	r, err := e.m.Stop("conv-1", id)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != Stopped {
		t.Fatalf("status = %s", r.Status)
	}
	for _, tk := range r.Tasks {
		if tk.Status != Stopped {
			t.Errorf("task %s = %s, want stopped", tk.Label, tk.Status)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if len(p.started) != 0 {
		t.Error("a queued task started after Stop")
	}
}

func TestResumeKeepsSuccessesAndRerunsTheRest(t *testing.T) {
	p := newProbe()
	e := open(t, t.TempDir(), 4, p.run(func(ctx context.Context, job Job) (string, error) {
		if job.Task.Label == "slow" && p.called("slow") == 1 {
			<-ctx.Done()
			return "", ctx.Err()
		}
		return "done-" + job.Task.Label, nil
	}))
	id := e.start("conv-1", task("quick"), task("slow"), task("after", "slow", "quick"))
	eventually(t, "quick done", func() bool {
		return byLabel(t, e.status("conv-1", id), "quick").Status == Succeeded
	})
	if _, err := e.m.Resume("conv-1", id); !errors.Is(err, ErrRunning) {
		t.Fatalf("Resume of a running run = %v, want ErrRunning", err)
	}
	if _, err := e.m.Stop("conv-1", id); err != nil {
		t.Fatal(err)
	}
	if n := e.next(); n.Status != Stopped {
		t.Fatalf("first notification = %s", n.Status)
	}

	r, err := e.m.Resume("conv-1", id)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != Running {
		t.Errorf("resumed status = %s", r.Status)
	}
	final := e.next()
	wantStatus(t, final, Succeeded, map[string]Status{"quick": Succeeded, "slow": Succeeded, "after": Succeeded})
	if p.called("quick") != 1 || p.called("slow") != 2 || p.called("after") != 1 {
		t.Errorf("calls quick/slow/after = %d/%d/%d, want 1/2/1", p.called("quick"), p.called("slow"), p.called("after"))
	}
	if res := byLabel(t, final, "quick").Result; res != "done-quick" {
		t.Errorf("kept result = %q", res)
	}
	if _, err := e.m.Resume("conv-1", id); !errors.Is(err, ErrNothingToResume) {
		t.Errorf("Resume of a finished run = %v, want ErrNothingToResume", err)
	}
	e.quiet(50 * time.Millisecond)
}

func TestResumeRetriesFailedAndBlocked(t *testing.T) {
	p := newProbe()
	e := open(t, t.TempDir(), 4, p.run(func(ctx context.Context, job Job) (string, error) {
		if job.Task.Label == "flaky" && p.called("flaky") == 1 {
			return "half", errors.New("first attempt")
		}
		return "done-" + job.Task.Label, nil
	}))
	id := e.start("conv-1", task("flaky"), task("dep", "flaky"), task("tail", "dep"), task("solo"))
	wantStatus(t, e.next(), Failed, map[string]Status{"flaky": Failed, "dep": Blocked, "tail": Blocked, "solo": Succeeded})

	if _, err := e.m.Resume("conv-1", id); err != nil {
		t.Fatal(err)
	}
	final := e.next()
	wantStatus(t, final, Succeeded, map[string]Status{"flaky": Succeeded, "dep": Succeeded, "tail": Succeeded, "solo": Succeeded})
	if byLabel(t, final, "flaky").Error != "" {
		t.Error("old error survived the retry")
	}
	if p.called("solo") != 1 || p.called("flaky") != 2 {
		t.Errorf("solo/flaky calls = %d/%d, want 1/2", p.called("solo"), p.called("flaky"))
	}
}

func TestStopWithRunnerThatIgnoresCancel(t *testing.T) {
	old := stopGrace
	stopGrace = 50 * time.Millisecond
	defer func() { stopGrace = old }()

	release := make(chan struct{})
	p := newProbe()
	e := open(t, t.TempDir(), 1, p.run(func(ctx context.Context, job Job) (string, error) {
		<-release // deaf to ctx
		return "late", ctx.Err()
	}))
	id := e.start("conv-1", task("deaf"))
	p.waitStarted(t)

	r, err := e.m.Stop("conv-1", id)
	if err != nil || r.Status != Running {
		t.Fatalf("Stop on a deaf runner = %s, %v; want it to return while still running", r.Status, err)
	}
	if _, err := e.m.Resume("conv-1", id); !errors.Is(err, ErrRunning) {
		t.Errorf("Resume while the old task is still alive = %v, want ErrRunning (no duplicate execution)", err)
	}
	close(release)
	if n := e.next(); n.Status != Stopped {
		t.Errorf("final status = %s, want stopped", n.Status)
	}
	if p.called("deaf") != 1 {
		t.Errorf("deaf ran %d times", p.called("deaf"))
	}
}

func TestCallbackRunsWithoutLocks(t *testing.T) {
	e := open(t, t.TempDir(), 4, func(ctx context.Context, job Job) (string, error) { return "ok", nil })
	var chained atomic.Bool
	e.hook = func(r Run) {
		// Every one of these takes Manager.mu; they would deadlock if it were held.
		if _, err := e.m.Status(r.OwnerSession, r.ID); err != nil {
			t.Errorf("Status in callback: %v", err)
		}
		e.m.List(r.OwnerSession)
		if chained.CompareAndSwap(false, true) {
			if _, err := e.m.Start(r.OwnerSession, "main", 0, independent(1)); err != nil {
				t.Errorf("Start in callback: %v", err)
			}
		}
	}
	e.start("conv-1", independent(2)...)
	e.next()
	e.next()
	e.quiet(50 * time.Millisecond)
}

func TestSnapshotsAreCopies(t *testing.T) {
	e := open(t, t.TempDir(), 1, func(ctx context.Context, job Job) (string, error) { return "orig", nil })
	id := e.start("conv-1", task("a"), task("b", "a"))
	n := e.next()
	n.Tasks[0].Result = "tampered"
	n.Tasks[1].DependsOn[0] = "tampered"
	r := e.status("conv-1", id)
	r.Tasks[0].Status = Failed
	again := e.status("conv-1", id)
	if again.Tasks[0].Result != "orig" || again.Tasks[1].DependsOn[0] != "a" || again.Tasks[0].Status != Succeeded {
		t.Errorf("caller mutation leaked into the manager: %+v", again.Tasks)
	}
}
