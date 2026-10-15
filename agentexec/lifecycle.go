package agentexec

import (
	"context"
	"time"

	"github.com/Autumn-27/artex/execution"
)

const cleanupTimeout = 2 * time.Second

// Only transport survives business cancellation. The same iterator is pulled
// through its terminal event so Norma owns history and transcript persistence.
type promptRun struct {
	ctx                     context.Context
	cancel                  context.CancelFunc
	transport               context.Context
	stopTransport           context.CancelFunc
	accepted, done, watched chan struct{}
}

func newPromptRun(ctx context.Context) *promptRun {
	business, cancel := context.WithCancel(ctx)
	transport, stop := context.WithCancel(context.Background())
	return &promptRun{ctx: business, cancel: cancel, transport: transport, stopTransport: stop,
		accepted: make(chan struct{}), done: make(chan struct{}), watched: make(chan struct{})}
}

func (r *promptRun) watch(peer *execution.Peer) {
	defer close(r.watched)
	select {
	case <-r.done:
		return
	case <-r.ctx.Done():
	}
	cleanup, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
	stop := context.AfterFunc(cleanup, r.stopTransport)
	defer stop()
	select {
	case <-r.accepted:
		// This method must bypass the worker's iterator mutex: next may be in a tool.
		_ = peer.Call(cleanup, "cancel", nil, nil)
	case <-r.done:
		return
	case <-cleanup.Done():
	}
	select {
	case <-r.done:
	case <-cleanup.Done():
		r.stopTransport()
	}
}

func (r *promptRun) finish() {
	close(r.done)
	<-r.watched // no late cancel may reach a subsequent Prompt
	r.cancel()
	r.stopTransport()
}

func (r *promptRun) failure(err error) error {
	if r.ctx.Err() != nil {
		return r.ctx.Err()
	}
	return err
}

func (s *Session) drain(r *promptRun) bool {
	for {
		var out eventReply
		if err := s.call(r.transport, "next", nil, &out); err != nil {
			return false
		}
		if !out.More {
			s.snapshot(out.Messages)
			return true
		}
		if out.Event.Terminal != nil {
			s.snapshot(out.Event.Terminal.Messages)
		}
	}
}

func deadline(ctx context.Context) time.Time { d, _ := ctx.Deadline(); return d }
