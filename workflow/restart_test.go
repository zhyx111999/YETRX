package workflow

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func TestShutdownThenRestartMarksInterruptedAndNeverAutoRuns(t *testing.T) {
	dir := t.TempDir()
	p1 := newProbe()
	e1 := open(t, dir, 4, p1.run(func(ctx context.Context, job Job) (string, error) {
		if job.Task.Label == "a" {
			return "result-a", nil
		}
		<-ctx.Done() // "b" is in flight when the server goes away
		return "partial-b", ctx.Err()
	}))
	id := e1.start("conv-1", task("a"), task("b", "a"), task("c", "b"))
	for range 2 {
		p1.waitStarted(t) // a, then b
	}

	e1.close()
	e1.quiet(50 * time.Millisecond) // shutdown is not a completion

	// ---- the next process ----
	p2 := newProbe()
	e2 := open(t, dir, 4, p2.run(func(ctx context.Context, job Job) (string, error) {
		return "again-" + job.Task.Label, nil
	}))
	n := e2.next()
	if n.ID != id {
		t.Fatalf("notified about %s, want %s", n.ID, id)
	}
	wantStatus(t, n, Interrupted, map[string]Status{"a": Succeeded, "b": Interrupted, "c": Pending})
	if got := byLabel(t, n, "a").Result; got != "result-a" {
		t.Errorf("finished result lost: %q", got)
	}

	time.Sleep(100 * time.Millisecond)
	if len(p2.started) != 0 {
		t.Fatal("restart re-executed work on its own")
	}
	e2.quiet(0)
	r := e2.status("conv-1", id)
	wantStatus(t, r, Interrupted, map[string]Status{"a": Succeeded, "b": Interrupted, "c": Pending})
	var onDisk Run
	if data := mustRead(t, filepath.Join(dir, id+".json")); json.Unmarshal(data, &onDisk) != nil || onDisk.Status != Interrupted {
		t.Errorf("interrupted state was not saved: %s", data)
	}

	// Resume: a stays as it was, b and c run now.
	if _, err := e2.m.Resume("conv-1", id); err != nil {
		t.Fatal(err)
	}
	final := e2.next()
	wantStatus(t, final, Succeeded, map[string]Status{"a": Succeeded, "b": Succeeded, "c": Succeeded})
	if byLabel(t, final, "a").Result != "result-a" || p2.called("a") != 0 {
		t.Errorf("a was re-run (calls %d) or its result replaced: %q", p2.called("a"), byLabel(t, final, "a").Result)
	}
	if byLabel(t, final, "b").Result != "again-b" {
		t.Errorf("b result = %q", byLabel(t, final, "b").Result)
	}

	// A third start finds only terminal runs: nothing to report, nothing to run.
	e2.close()
	e3 := open(t, dir, 4, func(ctx context.Context, job Job) (string, error) {
		t.Error("runner called after a clean restart")
		return "", nil
	})
	e3.quiet(100 * time.Millisecond)
	if got := e3.status("conv-1", id); got.Status != Succeeded {
		t.Errorf("status after third start = %s", got.Status)
	}
}

func TestCrashLeftRunningRunIsInterrupted(t *testing.T) {
	dir := t.TempDir()
	r := sampleRun("wf-00000000000000aa")
	r.Status = Running
	r.FinishedAt = time.Time{}
	r.Tasks = []Task{
		{Label: "a", Task: "x", Status: Succeeded, Result: "kept"},
		{Label: "b", Task: "y", Status: Running, DependsOn: []string{"a"}},
		{Label: "c", Task: "z", Status: Pending, DependsOn: []string{"b"}},
	}
	other := sampleRun("wf-00000000000000bb") // already terminal: left alone, not re-announced
	for _, x := range []Run{r, other} {
		if err := save(dir, x); err != nil {
			t.Fatal(err)
		}
	}

	e := open(t, dir, 2, func(ctx context.Context, job Job) (string, error) {
		t.Errorf("runner called for %s", job.Task.Label)
		return "", nil
	})
	n := e.next()
	wantStatus(t, n, Interrupted, map[string]Status{"a": Succeeded, "b": Interrupted, "c": Pending})
	if n.FinishedAt.IsZero() {
		t.Error("interrupted run has no FinishedAt")
	}
	e.quiet(100 * time.Millisecond)
	if got := e.status("conv-1", other.ID); got.Status != other.Status {
		t.Errorf("terminal run changed: %s", got.Status)
	}
	if len(e.m.List("conv-1")) != 2 {
		t.Error("restart lost a run")
	}
}

func TestStateIsSavedAtEveryStep(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	p := newProbe()
	e := open(t, dir, 4, p.run(func(ctx context.Context, job Job) (string, error) {
		if job.Task.Label == "b" {
			return block(release)(ctx, job)
		}
		return "out-a", nil
	}))
	id := e.start("conv-1", task("a"), task("b", "a"))
	p.waitStarted(t)
	p.waitStarted(t)

	disk := func() Run {
		var r Run
		if err := json.Unmarshal(mustRead(t, filepath.Join(dir, id+".json")), &r); err != nil {
			t.Fatalf("file is not valid JSON mid-run: %v", err)
		}
		return r
	}
	// b is running: a's result and b's running state are already on disk.
	eventually(t, "a's result saved", func() bool { return byLabel(t, disk(), "a").Result == "out-a" })
	if got := disk(); got.Status != Running || byLabel(t, got, "b").Status != Running {
		t.Errorf("mid-run file = %s / b %s", got.Status, byLabel(t, got, "b").Status)
	}

	close(release)
	e.next()
	want, _ := json.Marshal(e.status("conv-1", id))
	got, _ := json.Marshal(disk())
	if string(want) != string(got) {
		t.Errorf("final file differs from the in-memory run:\nfile %s\nmem  %s", got, want)
	}
}
