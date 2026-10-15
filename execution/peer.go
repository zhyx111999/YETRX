// Package execution moves an Agent session loop out of the web process: a
// supervisor starts a child process under OS resource limits (Process) and
// talks to it over stdin/stdout with a symmetric JSON RPC (Peer).
//
// stdout/stdin of the child carry protocol frames only; diagnostics go to
// stderr, whose bounded tail the supervisor keeps. Neither layer logs payloads.
package execution

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

// Memory bounds per Peer.
const (
	// MaxFrameBytes caps one frame body in either direction.
	MaxFrameBytes = 16 << 20
	// MaxBacklogBytes caps what a Peer buffers for its consumers: parameters of
	// running inbound calls, pending outbound calls, replies and queued events. Every
	// item also counts frameOverhead so empty messages are bounded too.
	MaxBacklogBytes = 64 << 20

	frameOverhead = 1 << 10
)

type peerLimits struct{ frame, backlog int64 }

// Peer is one end of a bidirectional RPC session. Both ends may Call each
// other concurrently. The read loop never blocks on user code: handlers run in
// their own goroutines and stream events are queued per call, so a handler or
// onEvent may issue nested Calls without deadlock.
type Peer struct {
	r       io.ReadCloser
	w       io.WriteCloser
	handler Handler
	lim     peerLimits

	controls chan *frame
	wlock    chan struct{} // write lock; a channel so waiters honour ctx
	backlog  atomic.Int64

	base       context.Context // parent of every handler ctx; cancelled on shutdown
	cancelBase context.CancelFunc

	mu      sync.Mutex
	nextID  uint64
	calls   map[uint64]*call    // outbound, awaiting reply
	serving map[uint64]*serving // inbound, handler running
	closed  bool
	err     error // shutdown cause, valid once closed

	ioOnce sync.Once
	ioErr  error
	done   chan struct{}
}

// NewPeer starts the read loop on r and returns immediately. A nil handler
// rejects every inbound call as unknown. Close (or EOF/protocol error) closes
// both r and w.
func NewPeer(r io.ReadCloser, w io.WriteCloser, handler Handler) *Peer {
	return newPeer(r, w, handler, peerLimits{frame: MaxFrameBytes, backlog: MaxBacklogBytes})
}

func newPeer(r io.ReadCloser, w io.WriteCloser, handler Handler, lim peerLimits) *Peer {
	base, cancel := context.WithCancel(context.Background())
	p := &Peer{
		r: r, w: w, handler: handler, lim: lim,
		wlock:      make(chan struct{}, 1),
		controls:   make(chan *frame, 64),
		base:       base,
		cancelBase: cancel,
		calls:      map[uint64]*call{},
		serving:    map[uint64]*serving{},
		done:       make(chan struct{}),
	}
	go p.controlLoop()
	go p.readLoop()
	return p
}

// Done is closed once the Peer has shut down: every pending call has failed,
// every handler context is cancelled and r/w are closed. It does not wait for
// handlers to return.
func (p *Peer) Done() <-chan struct{} { return p.done }

// Err is nil while the Peer is running. Afterwards it is the cause: io.EOF for
// a clean remote close, ErrClosed for a local Close, otherwise the I/O or
// protocol error.
func (p *Peer) Err() error {
	select {
	case <-p.done:
		return p.err
	default:
		return nil
	}
}

// Close shuts the Peer down and closes r and w. Idempotent.
func (p *Peer) Close() error {
	p.shutdown(ErrClosed)
	<-p.done
	return p.ioErr
}

func (p *Peer) shutdown(cause error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.err = cause
	calls := p.calls
	p.calls = nil
	p.mu.Unlock()

	p.cancelBase()
	err := p.closedErr()
	for _, c := range calls {
		c.finish(nil, err, 0)
	}
	p.ioOnce.Do(func() {
		p.ioErr = errors.Join(ignoreClosed(p.w.Close()), ignoreClosed(p.r.Close()))
	})
	close(p.done)
}

func ignoreClosed(err error) error {
	if errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return nil
	}
	return err
}

// closedErr is the error pending and later calls fail with.
func (p *Peer) closedErr() error {
	p.mu.Lock()
	cause := p.err
	p.mu.Unlock()
	if cause == nil || cause == ErrClosed {
		return ErrClosed
	}
	return fmt.Errorf("%w: %w", ErrClosed, cause)
}

func (p *Peer) readLoop() {
	for {
		f, err := readFrame(p.r, p.lim.frame)
		if err == nil {
			err = p.dispatch(f)
		}
		if err != nil {
			p.shutdown(err)
			return
		}
	}
}

func (p *Peer) dispatch(f *frame) error {
	if f.ID == 0 {
		return fmt.Errorf("%w: zero call id", ErrProtocol)
	}
	switch f.Type {
	case tCall:
		return p.onCall(f)
	case tCancel:
		if len(f.Data) != 0 || f.Error != nil || f.Method != "" {
			return fmt.Errorf("%w: malformed cancel", ErrProtocol)
		}
		p.onCancel(f.ID)
	case tEvent:
		if f.Error != nil || f.Method != "" {
			return fmt.Errorf("%w: malformed event", ErrProtocol)
		}
		p.onEvent(f)
	case tReply:
		p.onReply(f)
	default:
		return fmt.Errorf("%w: unknown frame type", ErrProtocol)
	}
	return nil
}

// control keeps the reader independent of a blocked writer without spawning
// unbounded goroutines. Exhausting this bounded queue fails the connection.
func (p *Peer) control(f *frame) {
	select {
	case <-p.base.Done():
	case p.controls <- f:
	default:
		p.shutdown(ErrBacklog)
	}
}

func (p *Peer) controlLoop() {
	for {
		select {
		case <-p.base.Done():
			return
		case f := <-p.controls:
			if err := p.send(p.base, f); err != nil {
				p.shutdown(err)
				return
			}
		}
	}
}

// send serializes encoding and writing. Cancelling an in-progress write closes
// the transport: a partially written frame cannot safely be reused. r/w must
// support Close interrupting their I/O (os pipes and net.Conn do).
func (p *Peer) send(ctx context.Context, f *frame) error {
	cost := frameDataSize(f)
	if cost > p.lim.frame {
		return ErrFrameTooLarge
	}
	// Outbound call parameters are already reserved by prepare. Other frames
	// reserve payload while waiting for the serialized encoder/writer.
	if f.Type != tCall {
		cost += frameOverhead
		if !p.charge(cost) {
			return ErrBacklog
		}
		defer p.release(cost)
	}
	select {
	case p.wlock <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.base.Done():
		return p.closedErr()
	}
	defer func() { <-p.wlock }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.base.Err() != nil {
		return p.closedErr()
	}
	b, err := encodeFrame(f, p.lim.frame)
	if err != nil {
		return err
	}
	interrupted := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		p.shutdown(ctx.Err())
		close(interrupted)
	})
	n, err := p.w.Write(b)
	if !stop() {
		<-interrupted
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	if err != nil {
		p.shutdown(err)
		return p.closedErr()
	}
	return nil
}

// charge reserves n bytes of backlog; false if the budget would be exceeded.
func (p *Peer) charge(n int64) bool {
	if p.backlog.Add(n) > p.lim.backlog {
		p.backlog.Add(-n)
		return false
	}
	return true
}

func (p *Peer) release(n int64) { p.backlog.Add(-n) }
