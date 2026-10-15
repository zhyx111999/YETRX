package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
)

// Handler serves one inbound call. ctx is cancelled when the remote cancels
// the call or the Peer shuts down (EOF, error, Close); handlers must honour it.
// emit sends an ordered stream event to a caller that used Stream (it is a
// no-op for a plain Call) and fails once ctx is done or the reply was sent.
// Return a *Error to control the wire error; context errors map to the
// canceled/deadline codes, anything else to "internal". A handler must reject
// methods it does not serve with MethodNotFound.
type Handler func(ctx context.Context, method string, params json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error)

// serving is one inbound call whose handler is running.
type serving struct {
	p        *Peer
	id       uint64
	ctx      context.Context
	cancel   context.CancelFunc
	stream   bool
	canceled atomic.Bool // the remote gave up: nobody waits for a reply

	mu   sync.Mutex // orders emit before the final reply
	done bool
}

func (p *Peer) onCall(f *frame) error {
	if f.ID == 0 || f.Method == "" || f.Error != nil {
		return fmt.Errorf("%w: malformed call", ErrProtocol)
	}
	cost := int64(len(f.Data)) + int64(len(f.Method)) + frameOverhead
	if !p.charge(cost) {
		p.control(&frame{Type: tReply, ID: f.ID, Error: &Error{Code: CodeBusy, Message: "inbound backlog limit exceeded"}})
		return nil
	}
	ctx, cancel := context.WithCancel(p.base)
	s := &serving{p: p, id: f.ID, ctx: ctx, cancel: cancel, stream: f.Stream}

	p.mu.Lock()
	_, dup := p.serving[f.ID]
	if !dup && !p.closed {
		p.serving[f.ID] = s
	}
	closed := p.closed
	p.mu.Unlock()
	if dup || closed {
		cancel()
		p.release(cost)
		if dup {
			return fmt.Errorf("%w: duplicate call id", ErrProtocol)
		}
		return nil
	}
	go p.serve(s, f.Method, f.Data, cost)
	return nil
}

func (p *Peer) onCancel(id uint64) {
	p.mu.Lock()
	s := p.serving[id]
	p.mu.Unlock()
	if s != nil {
		s.canceled.Store(true)
		s.cancel()
	}
}

func (p *Peer) serve(s *serving, method string, params json.RawMessage, cost int64) {
	defer p.release(cost)
	defer s.cancel()
	res, err := p.invoke(s, method, params)

	s.mu.Lock()
	s.done = true
	if !s.canceled.Load() {
		f := &frame{Type: tReply, ID: s.id, Data: res}
		if err != nil {
			f = &frame{Type: tReply, ID: s.id, Error: toError(err)}
		}
		if sendErr := p.send(p.base, f); sendErr != nil && p.base.Err() == nil {
			// The reply itself was unsendable (too large, invalid JSON): fail the
			// call instead of leaving the caller waiting.
			if fallbackErr := p.send(p.base, &frame{Type: tReply, ID: s.id, Error: toError(fmt.Errorf("execution: reply: %w", sendErr))}); fallbackErr != nil {
				p.shutdown(fallbackErr)
			}
		}
	}
	s.mu.Unlock()

	p.mu.Lock()
	delete(p.serving, s.id)
	p.mu.Unlock()
}

func (p *Peer) invoke(s *serving, method string, params json.RawMessage) (res json.RawMessage, err error) {
	defer func() {
		if r := recover(); r != nil {
			// Never print the panic value: it may embed request data.
			log.Print("execution: handler panicked")
			res, err = nil, &Error{Code: CodeInternal, Message: "handler panicked"}
		}
	}()
	if p.handler == nil {
		return nil, MethodNotFound(method)
	}
	return p.handler(s.ctx, method, params, s.emit)
}

func (s *serving) emit(ev json.RawMessage) error {
	if !s.stream {
		return nil
	}
	if len(ev) == 0 {
		ev = json.RawMessage("null")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return errors.New("execution: emit after reply")
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	return s.p.send(s.ctx, &frame{Type: tEvent, ID: s.id, Data: ev})
}
