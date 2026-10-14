package workflow

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestQuotaResetHintsAndBackoff(t *testing.T) {
	now := time.Date(2026, 10, 14, 14, 0, 0, 0, time.FixedZone("test", 8*3600))
	for _, tc := range []struct {
		hint string
		want time.Duration
		ok   bool
	}{
		{"Resets in ~3h", 3 * time.Hour, true}, {"Try again in ~299 min", 299 * time.Minute, true},
		{"1h30m", 90 * time.Minute, true}, {"resets at 2026-10-14 15:20:00 +0800 CST", 80 * time.Minute, true},
		{"resets at 2026-10-14T07:20:00Z", 80 * time.Minute, true}, {"Try again at 3:20 PM", 80 * time.Minute, true},
		{"reset at 25:89 PM", 0, false}, {"not specified", 0, false}, {"3models", 0, false},
	} {
		got, ok := parseResetHint(tc.hint, now)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%q=%v,%t want %v,%t", tc.hint, got, ok, tc.want, tc.ok)
		}
	}
	random := func() float64 { return .5 }
	if got := quotaDelay("", 3, now, now, random); got != 20*time.Minute {
		t.Fatal("fallback backoff", got)
	}
	if got := quotaDelay("Resets in 1h", 1, now, now.Add(20*time.Minute), random); got != 40*time.Minute {
		t.Fatal("cold start elapsed", got)
	}
	if got := quotaDelay("Resets in 3d", 5, now, now, random); got > 6*time.Hour || got < 5*time.Hour {
		t.Fatal("ceiling jitter", got)
	}
	if got := quotaDelay("Resets in 1s", 1, now, now, random); got != time.Minute {
		t.Fatal("minimum", got)
	}
}

func TestQuotaResumeIsBoundedAndManualResumeResetsCycle(t *testing.T) {
	e := open(t, t.TempDir(), 1, func(context.Context, Job) (string, error) { return "", errors.New("429 usage limit. Resets in 5m") })
	id := e.start("owner", task("a"))
	for attempt := 1; attempt <= quotaMaxAttempts+1; attempt++ {
		r := e.next()
		if r.Status != Paused || r.PauseReason != "usage_limit" || r.AutoResumeAttempts != attempt {
			t.Fatalf("quota pause %d: %+v", attempt, r)
		}
		e.m.mu.Lock()
		entry := e.m.runs[id]
		armed := entry.quotaTimer != nil
		if attempt <= quotaMaxAttempts {
			// Exercise the actual timer callback without a minutes-long test.
			e.m.cancelQuotaTimer(entry)
			e.m.setQuotaTimer(entry, time.Millisecond)
		}
		e.m.mu.Unlock()
		if armed != (attempt <= quotaMaxAttempts) {
			t.Fatalf("incorrect timer at attempt %d", attempt)
		}
	}
	if _, err := e.m.Resume("owner", id); err != nil {
		t.Fatal(err)
	}
	if r := e.next(); r.AutoResumeAttempts != 1 {
		t.Fatalf("manual resume did not reset cycle: %+v", r)
	}
	if _, err := e.m.Stop("owner", id); err != nil {
		t.Fatal(err)
	}
	if r := e.next(); r.Status != Stopped || !r.ResumeAt.IsZero() {
		t.Fatalf("stop did not disarm: %+v", r)
	}
}

func TestQuotaColdStartDoesNotSpendAnotherAttempt(t *testing.T) {
	dir := t.TempDir()
	runner := func(context.Context, Job) (string, error) { return "", errors.New("rate limit") }
	e := open(t, dir, 1, runner)
	id := e.start("owner", task("a"))
	e.next()
	e.close()
	reopened := open(t, dir, 1, runner)
	r := reopened.status("owner", id)
	if r.AutoResumeAttempts != 1 || r.ResumeAt.IsZero() {
		t.Fatalf("cold start lost armed attempt: %+v", r)
	}
}
