package workflow

import (
	"context"
	"sync"
)

// sessionLimiter caps the tasks running at once per owner session: every run of
// one conversation shares the same limit, and conversations never share it with
// each other, so a busy, cancelled or draining conversation cannot starve another.
//
// A slot is held from acquire until the returned release runs. execute releases
// only after its runner has really returned, so cancellation, timeouts and
// shutdown never free a slot that a live runner still occupies.
//
// Bookkeeping exists only while a conversation has a task running or queued:
// the last release or abandoned wait removes its entry. The limiter's lock is
// private, held for map updates only, and never taken together with Manager.mu.
type sessionLimiter struct {
	limit int

	mu    sync.Mutex
	gates map[string]*gate
}

// gate is one conversation's slots.
type gate struct {
	slots chan struct{} // one token per running task
	users int           // tasks running or queued; guarded by sessionLimiter.mu
}

func newSessionLimiter(limit int) *sessionLimiter {
	return &sessionLimiter{limit: limit, gates: make(map[string]*gate)}
}

// acquire waits until owner has a free slot and returns the function that gives
// it back; the caller must call it exactly once. If ctx ends first, including
// when a slot and cancellation are ready together, no slot is taken and ctx's
// error comes back.
func (l *sessionLimiter) acquire(ctx context.Context, owner string) (release func(), err error) {
	g := l.join(owner)
	select {
	case g.slots <- struct{}{}:
	case <-ctx.Done():
		l.leave(owner, g)
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		<-g.slots
		l.leave(owner, g)
		return nil, err
	}
	return func() {
		<-g.slots
		l.leave(owner, g)
	}, nil
}

func (l *sessionLimiter) join(owner string) *gate {
	l.mu.Lock()
	defer l.mu.Unlock()
	g := l.gates[owner]
	if g == nil {
		g = &gate{slots: make(chan struct{}, l.limit)}
		l.gates[owner] = g
	}
	g.users++
	return g
}

func (l *sessionLimiter) leave(owner string, g *gate) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if g.users--; g.users == 0 {
		delete(l.gates, owner)
	}
}
