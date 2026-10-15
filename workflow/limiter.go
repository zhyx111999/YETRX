package workflow

import (
	"context"
	"sync"
)

// sessionLimiter keeps independent, live-adjustable limits per owner. Running
// tasks retain their slots until the runner exits, even after a limit decrease.
// Its private lock is never held together with Manager.mu.
type sessionLimiter struct {
	mu    sync.Mutex
	limit int
	gates map[string]*gate
}

type gate struct {
	active  int
	users   int // Running and queued tasks, guarded by sessionLimiter.mu.
	changed chan struct{}
}

func newSessionLimiter(limit int) *sessionLimiter {
	return &sessionLimiter{limit: limit, gates: make(map[string]*gate)}
}

func (l *sessionLimiter) setLimit(limit int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.limit == limit {
		return
	}
	l.limit = limit
	for _, g := range l.gates {
		g.notify()
	}
}

func (l *sessionLimiter) currentLimit() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limit
}

func (g *gate) notify() {
	close(g.changed)
	g.changed = make(chan struct{})
}

// acquire waits for capacity or cancellation. A resize wakes queued tasks
// without replacing their gate or losing reservations held by live runners.
func (l *sessionLimiter) acquire(ctx context.Context, owner string) (func(), error) {
	l.mu.Lock()
	g := l.gates[owner]
	if g == nil {
		g = &gate{changed: make(chan struct{})}
		l.gates[owner] = g
	}
	g.users++
	for {
		if err := ctx.Err(); err != nil {
			l.leave(owner, g)
			l.mu.Unlock()
			return nil, err
		}
		if g.active < l.limit {
			g.active++
			l.mu.Unlock()
			var once sync.Once
			release := func() {
				once.Do(func() {
					l.mu.Lock()
					g.active--
					l.leave(owner, g)
					g.notify()
					l.mu.Unlock()
				})
			}
			if err := ctx.Err(); err != nil {
				release()
				return nil, err
			}
			return release, nil
		}
		changed := g.changed
		l.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
		}
		l.mu.Lock()
	}
}

// leave runs with l.mu held. Empty gates retain no owner references.
func (l *sessionLimiter) leave(owner string, g *gate) {
	g.users--
	if g.users == 0 {
		delete(l.gates, owner)
	}
}
