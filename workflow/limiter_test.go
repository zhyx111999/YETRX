package workflow

import (
	"context"
	"errors"
	"testing"
	"time"
)

// users is the number of tasks running or queued for owner; sessions is how many
// owners the limiter still keeps bookkeeping for.
func (l *sessionLimiter) users(owner string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	if g := l.gates[owner]; g != nil {
		return g.users
	}
	return 0
}

func (l *sessionLimiter) sessions() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.gates)
}

func mustAcquire(t *testing.T, l *sessionLimiter, owner string) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	release, err := l.acquire(ctx, owner)
	if err != nil {
		t.Fatalf("acquire(%s): %v", owner, err)
	}
	return release
}

func TestLimiterCapsEachOwnerSeparately(t *testing.T) {
	l := newSessionLimiter(2)
	a1, a2 := mustAcquire(t, l, "a"), mustAcquire(t, l, "a")
	b1, b2 := mustAcquire(t, l, "b"), mustAcquire(t, l, "b") // a being full does not touch b

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := l.acquire(ctx, "a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("third slot of a = %v, want it to wait until the deadline", err)
	}
	if n := l.users("a"); n != 2 {
		t.Fatalf("an abandoned wait left a user behind: %d", n)
	}

	a1() // a freed slot is reusable at once
	a3 := mustAcquire(t, l, "a")
	for _, release := range []func(){a2, a3, b1, b2} {
		release()
	}
	if n := l.sessions(); n != 0 {
		t.Fatalf("%d owners still tracked after every release", n)
	}
}

func TestLimiterWaiterTakesTheReleasedSlot(t *testing.T) {
	l := newSessionLimiter(1)
	first := mustAcquire(t, l, "a")
	granted := make(chan func(), 1)
	go func() {
		release, err := l.acquire(context.Background(), "a")
		if err != nil {
			t.Error(err)
			return
		}
		granted <- release
	}()
	eventually(t, "the second task to queue", func() bool { return l.users("a") == 2 })
	select {
	case <-granted:
		t.Fatal("a queued task got a slot that was still held")
	case <-time.After(50 * time.Millisecond):
	}

	first()
	select {
	case release := <-granted:
		release()
	case <-time.After(wait):
		t.Fatal("the queued task never got the released slot")
	}
	if n := l.sessions(); n != 0 {
		t.Fatalf("%d owners still tracked", n)
	}
}

// With a free slot and a cancelled context both ready, cancellation must win,
// otherwise a stopped run could start work.
func TestLimiterNeverGrantsASlotToACancelledContext(t *testing.T) {
	l := newSessionLimiter(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 200 {
		if release, err := l.acquire(ctx, "a"); err == nil {
			release()
			t.Fatal("slot granted to a cancelled context")
		} else if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	}
	if n := l.sessions(); n != 0 {
		t.Fatalf("%d owners still tracked", n)
	}
	release := mustAcquire(t, l, "a") // and the slot itself was not consumed
	release()
}
