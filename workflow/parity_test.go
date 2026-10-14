package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMessageAttemptBoundaryAndOwnership(t *testing.T) {
	ready, release := make(chan Job, 1), make(chan struct{})
	e := open(t, t.TempDir(), 1, func(ctx context.Context, job Job) (string, error) {
		ready <- job
		select {
		case <-release:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		messages := job.Messages.Drain()
		if len(job.Messages.Drain()) != 0 {
			return "", errors.New("duplicate delivery")
		}
		return strings.Join(messages, "|"), nil
	})
	id := e.start("owner", task("live"), task("pending", "live"))
	job := <-ready
	if err := e.m.Message("other", id, "live", "bad"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := e.m.Message("owner", id, "pending", "bad"); !errors.Is(err, ErrNotRunning) {
		t.Fatal(err)
	}
	for _, text := range []string{"  first\n", "second"} {
		if err := e.m.Message("owner", id, "live", text); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	<-ready
	result := e.next()
	if got := byLabel(t, result, "live").Result; got != "  first\n|second" {
		t.Fatalf("messages changed: %q", got)
	}
	if err := e.m.Message("owner", id, "live", "late"); !errors.Is(err, ErrNotRunning) {
		t.Fatal(err)
	}
	if job.Messages.enqueue("late") {
		t.Fatal("closed mailbox accepted instruction")
	}
}

func TestEditedResumeReusesReorderedTasksAndInvalidatesDependents(t *testing.T) {
	p := newProbe()
	e := open(t, t.TempDir(), 4, p.run(func(_ context.Context, job Job) (string, error) { return job.Task.Task, nil }))
	a, b, c := task("a"), task("b"), task("c", "a")
	id := e.start("owner", a, b, c)
	e.next()
	a.Task = "changed input"
	r, err := e.m.ResumeWithOptions("owner", id, ResumeOptions{Tasks: []Task{c, b, a}})
	if err != nil {
		t.Fatal(err)
	}
	if byLabel(t, r, "b").Status != Succeeded {
		t.Fatal("reordered success lost")
	}
	r = e.next()
	if r.Status != Succeeded || p.called("a") != 2 || p.called("b") != 1 || p.called("c") != 2 {
		t.Fatalf("reuse mismatch: %+v %v", r, p.calls)
	}
	if len(r.History) != 2 {
		t.Fatalf("superseded results lost: %+v", r.History)
	}
	if _, err := e.m.ResumeWithOptions("owner", id, ResumeOptions{Tasks: []Task{b, a, c}}); err != nil {
		t.Fatal(err)
	}
	e.next()
	if p.called("a") != 2 || p.called("b") != 1 || p.called("c") != 2 {
		t.Fatal("pure reorder reran completed tasks")
	}
}

func TestCheckpointSurvivesRestartAndConsumesExactResponse(t *testing.T) {
	dir := t.TempDir()
	e := open(t, dir, 1, func(_ context.Context, job Job) (string, error) { return job.Results["gate"], nil })
	id := e.start("owner", Task{Label: "gate", Checkpoint: &CheckpointInput{ID: "approve:1", Kind: "review", Payload: json.RawMessage(`{"diff":"ready"}`)}}, task("after", "gate"))
	if r := e.next(); r.Status != Paused || r.Checkpoint == nil {
		t.Fatalf("not waiting: %+v", r)
	}
	e.close()
	next := open(t, dir, 1, func(_ context.Context, job Job) (string, error) { return job.Results["gate"], nil })
	if r := next.status("owner", id); r.Checkpoint == nil || r.Checkpoint.Status != "waiting" {
		t.Fatalf("checkpoint not durable: %+v", r)
	}
	if _, err := next.m.Resume("owner", id); !errors.Is(err, ErrInvalid) {
		t.Fatal("missing answer accepted", err)
	}
	if _, err := next.m.ResumeWithOptions("owner", id, ResumeOptions{CheckpointID: "wrong", Response: json.RawMessage(`null`)}); !errors.Is(err, ErrInvalid) {
		t.Fatal("wrong answer accepted", err)
	}
	if _, err := next.m.ResumeWithOptions("owner", id, ResumeOptions{CheckpointID: "approve:1", Response: json.RawMessage(`null`)}); err != nil {
		t.Fatal(err)
	}
	r := next.next()
	if r.Status != Succeeded || byLabel(t, r, "after").Result != "null" || r.Checkpoint.Status != "consumed" {
		t.Fatalf("response not consumed: %+v", r)
	}
}

func TestRetriesClassifyFailuresAndUseFreshSessions(t *testing.T) {
	for _, tc := range []struct {
		name          string
		err           error
		retries, want int
		status        Status
	}{
		{"default", errors.New("temporary transport failure"), 0, 1, Failed},
		{"recoverable", errors.New("temporary transport failure"), 2, 3, Failed},
		{"permanent", Permanent(errors.New("invalid setup")), 3, 1, Failed},
		{"quota", errors.New("HTTP 429 quota exceeded"), 3, 1, Paused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions := map[string]bool{}
			e := open(t, t.TempDir(), 1, func(_ context.Context, job Job) (string, error) {
				if sessions[job.SessionID] {
					return "", Permanent(errors.New("session reused"))
				}
				sessions[job.SessionID] = true
				return "partial", tc.err
			})
			_, err := e.m.StartWithOptions("owner", "agent", 0, []Task{task("a")}, Options{AgentRetries: tc.retries})
			if err != nil {
				t.Fatal(err)
			}
			r := e.next()
			if r.Status != tc.status || r.Tasks[0].Attempts != tc.want || len(sessions) != tc.want {
				t.Fatalf("%+v sessions=%v", r, sessions)
			}
		})
	}
}

func TestTimedOutRunnerCannotOverlapRetryOrResume(t *testing.T) {
	old := drainGrace
	drainGrace = 20 * time.Millisecond
	defer func() { drainGrace = old }()
	release := make(chan struct{})
	var calls atomic.Int32
	e := open(t, t.TempDir(), 1, func(_ context.Context, _ Job) (string, error) { calls.Add(1); <-release; return "late", nil })
	// Ensure cleanup cannot deadlock if a later assertion fails.
	defer close(release)
	id, err := e.m.StartWithOptions("owner", "agent", 0, []Task{task("a")}, Options{AgentRetries: 3, AgentTimeoutMS: 10})
	if err != nil {
		t.Fatal(err)
	}
	r := e.next()
	if !r.Tasks[0].Draining || r.Tasks[0].Attempts != 1 || r.Status != Failed {
		t.Fatalf("abandoned runner not fenced: %+v", r)
	}
	if _, err := e.m.Resume("owner", id); !errors.Is(err, ErrRunning) {
		t.Fatal("resume overlapped live runner", err)
	}
	if calls.Load() != 1 {
		t.Fatal("duplicate live attempt")
	}
}

func TestRetryWaitsForTimedOutAttemptToReturn(t *testing.T) {
	var active, peak atomic.Int32
	e := open(t, t.TempDir(), 1, func(ctx context.Context, job Job) (string, error) {
		current := active.Add(1)
		if current > peak.Load() {
			peak.Store(current)
		}
		defer active.Add(-1)
		if job.Task.Attempts == 1 {
			<-ctx.Done()
			time.Sleep(15 * time.Millisecond)
			return "", ctx.Err()
		}
		return "recovered", nil
	})
	_, err := e.m.StartWithOptions("owner", "agent", 0, []Task{task("a")}, Options{AgentRetries: 1, AgentTimeoutMS: 30})
	if err != nil {
		t.Fatal(err)
	}
	r := e.next()
	if r.Status != Succeeded || r.Tasks[0].Attempts != 2 || peak.Load() != 1 {
		t.Fatalf("retry order: %+v peak=%d", r, peak.Load())
	}
}

func TestThreadsKeepSessionAndCannotReplay(t *testing.T) {
	a, b := task("a"), task("b", "a")
	a.Thread, b.Thread = "named", "named"
	sessions := make(chan string, 4)
	e := open(t, t.TempDir(), 2, func(_ context.Context, job Job) (string, error) { sessions <- job.SessionID; return "ok", nil })
	id := e.start("owner", a, b)
	e.next()
	first, second := <-sessions, <-sessions
	if first != second {
		t.Fatal("thread did not retain session")
	}
	if _, err := e.m.ResumeWithOptions("owner", id, ResumeOptions{Tasks: []Task{a, b}}); err != nil {
		t.Fatal(err)
	}
	e.next()
	third, fourth := <-sessions, <-sessions
	if third != fourth || third == first {
		t.Fatal("resumed invocation reused thread history")
	}
}

func TestWorkspaceRetentionCleanupAndRetryReuse(t *testing.T) {
	repo := t.TempDir()
	gitTest := func(args ...string) string {
		t.Helper()
		out, err := gitCommand(context.Background(), 30*time.Second, append([]string{"-C", repo}, args...)...)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	gitTest("init")
	if err := os.WriteFile(filepath.Join(repo, "source.txt"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	gitTest("add", "source.txt")
	gitTest("-c", "user.name=YETRX test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "fixture")
	head := gitTest("rev-parse", "HEAD")
	for _, keep := range []bool{true, false} {
		t.Run(map[bool]string{true: "keep", false: "remove"}[keep], func(t *testing.T) {
			var paths []string
			e := open(t, t.TempDir(), 1, func(_ context.Context, job Job) (string, error) {
				paths = append(paths, job.CWD)
				if err := os.WriteFile(filepath.Join(job.CWD, "source.txt"), []byte("child"), 0600); err != nil {
					return "", err
				}
				if job.Task.Attempts == 1 {
					return "", errors.New("recoverable")
				}
				return "done", nil
			})
			_, err := e.m.StartWithOptions("owner", "agent", 0, []Task{{Label: "editor", Task: "edit", Isolation: "worktree", KeepWorktree: &keep}}, Options{CWD: repo, AgentRetries: 1})
			if err != nil {
				t.Fatal(err)
			}
			r := e.next()
			w := r.Tasks[0].Workspace
			if r.Status != Succeeded || w == nil || len(paths) != 2 || paths[0] != paths[1] || w.Removed == keep {
				t.Fatalf("workspace lifecycle: %+v %v", r, paths)
			}
			content, _ := os.ReadFile(filepath.Join(repo, "source.txt"))
			if string(content) != "original" || gitTest("rev-parse", "HEAD") != head {
				t.Fatal("parent checkout changed")
			}
			if keep {
				if _, err := os.Stat(w.CWD); err != nil {
					t.Fatal(err)
				}
				removeWorkspace(w)
			} else {
				if _, err := os.Stat(w.CWD); !os.IsNotExist(err) {
					t.Fatal("worktree still exists", err)
				}
			}
			if got := gitTest("for-each-ref", "--format=%(refname)", "refs/heads/"+w.Branch); got != "" {
				t.Fatal("owned branch not cleaned", got)
			}
		})
	}
}

func TestRequestedIsolationNeverFallsBackToSharedDirectory(t *testing.T) {
	var calls atomic.Int32
	e := open(t, t.TempDir(), 1, func(context.Context, Job) (string, error) { calls.Add(1); return "bad", nil })
	_, err := e.m.StartWithOptions("owner", "agent", 0, []Task{{Label: "a", Task: "edit", Isolation: "worktree"}}, Options{CWD: t.TempDir(), AgentRetries: 3})
	if err != nil {
		t.Fatal(err)
	}
	r := e.next()
	if r.Status != Failed || calls.Load() != 0 || !strings.Contains(r.Tasks[0].Error, "isolation failed") {
		t.Fatalf("isolation fell back: %+v", r)
	}
}
