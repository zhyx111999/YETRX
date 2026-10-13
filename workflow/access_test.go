package workflow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestOwnerIsolation(t *testing.T) {
	release := make(chan struct{})
	p := newProbe()
	e := open(t, t.TempDir(), 4, p.run(block(release)))
	id := e.start("conv-1", task("a"))
	p.waitStarted(t)
	other := e.start("conv-2", task("a"))
	p.waitStarted(t)

	for _, intruder := range []string{"conv-2", "conv-3", "", "conv-1 ", "CONV-1"} {
		check := func(what string, err error) {
			t.Helper()
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("%s as %q = %v, want ErrNotFound", what, intruder, err)
			}
		}
		_, err := e.m.Status(intruder, id)
		check("Status", err)
		_, err = e.m.Read(intruder, id, "a", 0)
		check("Read", err)
		_, err = e.m.Stop(intruder, id)
		check("Stop", err)
		_, err = e.m.Resume(intruder, id)
		check("Resume", err)
	}
	if got := e.m.List(""); len(got) != 0 {
		t.Errorf("List(\"\") leaked %d runs", len(got))
	}
	if got := e.m.List("conv-1"); len(got) != 1 || got[0].ID != id {
		t.Errorf("List(conv-1) = %+v", got)
	}
	if got := e.m.List("conv-2"); len(got) != 1 || got[0].ID != other {
		t.Errorf("List(conv-2) = %+v", got)
	}
	// The failed attempts must not have touched the run.
	if r := e.status("conv-1", id); r.Status != Running {
		t.Errorf("foreign Stop affected the run: %s", r.Status)
	}
	if _, err := e.m.Status("conv-1", "wf-doesnotexist000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id = %v", err)
	}
	close(release)
	e.next()
	e.next()
}

func TestListIsNewestFirst(t *testing.T) {
	e := open(t, t.TempDir(), 4, func(ctx context.Context, job Job) (string, error) { return "ok", nil })
	var ids []string
	for range 3 {
		ids = append(ids, e.start("conv-1", task("a")))
		time.Sleep(2 * time.Millisecond)
	}
	for range 3 {
		e.next()
	}
	got := e.m.List("conv-1")
	if len(got) != 3 || got[0].ID != ids[2] || got[1].ID != ids[1] || got[2].ID != ids[0] {
		t.Errorf("order = %v, started %v", got, ids)
	}
}

func TestReadReturnsTheLastCharacters(t *testing.T) {
	long := strings.Repeat("字", 9000) + "尾"  // 9001 characters, 27003 bytes
	emoji := strings.Repeat("a", 7999) + "😀" // multi-byte character exactly at the cut
	e := open(t, t.TempDir(), 4, func(ctx context.Context, job Job) (string, error) {
		switch job.Task.Label {
		case "long":
			return long, nil
		case "emoji":
			return emoji, nil
		case "short":
			return "hello", nil
		}
		return "", nil
	})
	id := e.start("conv-1", task("long"), task("emoji"), task("short"), task("empty"))
	e.next()

	read := func(label string, n int) string {
		t.Helper()
		got, err := e.m.Read("conv-1", id, label, n)
		if err != nil {
			t.Fatalf("Read(%s, %d): %v", label, n, err)
		}
		if !utf8.ValidString(got) {
			t.Fatalf("Read(%s, %d) cut a character in half", label, n)
		}
		return got
	}
	for _, n := range []int{0, -1, MaxReadChars, MaxReadChars + 1, 1 << 30} { // default and cap
		got := read("long", n)
		if utf8.RuneCountInString(got) != MaxReadChars || !strings.HasSuffix(got, "字尾") {
			t.Errorf("Read(long, %d) = %d chars", n, utf8.RuneCountInString(got))
		}
	}
	if got := read("long", 3); got != "字字尾" {
		t.Errorf("Read(long, 3) = %q", got)
	}
	if got := read("emoji", 0); utf8.RuneCountInString(got) != MaxReadChars || !strings.HasSuffix(got, "a😀") {
		t.Errorf("emoji tail wrong: %d chars", utf8.RuneCountInString(got))
	}
	if got := read("short", 100); got != "hello" {
		t.Errorf("short result = %q", got)
	}
	if got := read(" short ", 100); got != "hello" {
		t.Errorf("label not trimmed: %q", got)
	}
	// The stored result is complete; only Read trims.
	if full := byLabel(t, e.status("conv-1", id), "long").Result; full != long {
		t.Errorf("stored result truncated: %d bytes", len(full))
	}

	if _, err := e.m.Read("conv-1", id, "nope", 0); !errors.Is(err, ErrNoTask) {
		t.Errorf("unknown label = %v", err)
	}
	if _, err := e.m.Read("conv-1", id, "empty", 0); !errors.Is(err, ErrNoResult) {
		t.Errorf("empty result = %v", err)
	}
}

func TestReadBeforeFinish(t *testing.T) {
	p := newProbe()
	e := open(t, t.TempDir(), 4, p.run(block(nil)))
	id := e.start("conv-1", task("a"), task("b", "a"))
	p.waitStarted(t)
	for _, l := range []string{"a", "b"} {
		if _, err := e.m.Read("conv-1", id, l, 0); !errors.Is(err, ErrNoResult) {
			t.Errorf("Read(%s) of an unfinished task = %v, want ErrNoResult", l, err)
		}
	}
}

// A task's own agent starts runs with Job.OwnerSession and Job.Depth: the owner
// stays the root conversation, the depth grows, and it stops at MaxDepth.
func TestNestedRunsKeepRootOwnerAndStopAtMaxDepth(t *testing.T) {
	type seen struct {
		depth   int
		session string
	}
	var e *env
	jobs := make(chan seen, 10)
	startErr := make(chan error, 1)
	e = open(t, t.TempDir(), 4, func(ctx context.Context, job Job) (string, error) {
		jobs <- seen{job.Depth, job.OwnerSession}
		_, err := e.m.Start(job.OwnerSession, job.Task.AgentKey, job.Depth, []Task{{Label: "child", Task: "go deeper", AgentKey: "sub"}})
		if err != nil {
			startErr <- err
		}
		return "ok", nil
	})
	e.start("conv-7", Task{Label: "root", Task: "go", AgentKey: "main"})
	for range MaxDepth {
		e.next()
	}
	e.quiet(50 * time.Millisecond)

	if err := <-startErr; !errors.Is(err, ErrDepth) {
		t.Errorf("deepest Start = %v, want ErrDepth", err)
	}
	for want := 1; want <= MaxDepth; want++ {
		if s := <-jobs; s.depth != want || s.session != "conv-7" {
			t.Errorf("job at level %d = %+v", want, s)
		}
	}
	runs := e.m.List("conv-7")
	if len(runs) != MaxDepth {
		t.Fatalf("root conversation sees %d runs, want %d", len(runs), MaxDepth)
	}
	for _, r := range runs {
		if r.OwnerSession != "conv-7" || r.Depth >= MaxDepth {
			t.Errorf("run %s: owner %s depth %d", r.ID, r.OwnerSession, r.Depth)
		}
	}
}

// Meant for `go test -race`: many owners start, inspect, stop and resume at once.
func TestConcurrentUse(t *testing.T) {
	const owners, perOwner, limit = 6, 4, 4
	p := newProbe()
	e := open(t, t.TempDir(), limit, p.run(func(ctx context.Context, job Job) (string, error) {
		select {
		case <-time.After(3 * time.Millisecond):
			return "ok", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}))

	var resumed atomic.Int32
	var wg sync.WaitGroup
	for o := range owners {
		wg.Go(func() {
			session := fmt.Sprintf("conv-%d", o)
			for i := range perOwner {
				id, err := e.m.Start(session, "main", 0,
					[]Task{task("a"), task("b"), task("c", "a", "b"), task("d", "c")})
				if err != nil {
					t.Errorf("Start: %v", err)
					return
				}
				e.m.List(session)
				if _, err := e.m.Status(session, id); err != nil {
					t.Errorf("Status: %v", err)
				}
				if i%2 == 0 { // half of them get stopped mid-flight, then resumed
					if _, err := e.m.Stop(session, id); err == nil {
						if _, err := e.m.Resume(session, id); err == nil {
							resumed.Add(1)
						}
					} else if !errors.Is(err, ErrNotRunning) {
						t.Errorf("Stop: %v", err)
					}
				}
			}
		})
	}
	wg.Wait()

	// One notification per terminal transition: each run finishes once, plus
	// once more for every successful Resume.
	for range owners*perOwner + int(resumed.Load()) {
		e.next()
	}
	e.quiet(100 * time.Millisecond)

	for o := range owners {
		for _, r := range e.m.List(fmt.Sprintf("conv-%d", o)) {
			if r.Status != Succeeded {
				t.Errorf("run %s ended %s", r.ID, r.Status)
			}
		}
	}
	if p.peak() > limit {
		t.Errorf("peak concurrency %d exceeded %d", p.peak(), limit)
	}
}
