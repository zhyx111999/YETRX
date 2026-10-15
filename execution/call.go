package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// call is the state of one outbound call: ordered stream events, then a final outcome.
type call struct {
	stream bool
	wake   chan struct{}

	mu      sync.Mutex
	events  []json.RawMessage
	fin     bool
	res     json.RawMessage
	resCost int64
	err     error
}

func newCall(stream bool) *call {
	return &call{stream: stream, wake: make(chan struct{}, 1)}
}

func (c *call) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// push queues an event; false if the call is already finished.
func (c *call) push(ev json.RawMessage) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fin {
		return false
	}
	c.events = append(c.events, ev)
	c.signal()
	return true
}

// finish records the outcome once; queued events are still delivered first.
func (c *call) finish(res json.RawMessage, err error, cost int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fin {
		return false
	}
	c.fin, c.res, c.err, c.resCost = true, res, err, cost
	c.signal()
	return true
}

// abort finishes with err and discards queued events, returning their backlog cost.
func (c *call) abort(err error) (cost int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ev := range c.events {
		cost += eventCost(ev)
	}
	c.events = nil
	cost += c.resCost
	c.resCost = 0
	if !c.fin {
		c.fin, c.err = true, err
	}
	c.signal()
	return cost
}

func (c *call) pending() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.fin
}

func eventCost(ev json.RawMessage) int64 { return int64(len(ev)) + frameOverhead }

// next pops the oldest event; done reports that the queue is empty and the call finished.
func (c *call) next() (ev json.RawMessage, ok, done bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.events) > 0 {
		ev = c.events[0]
		c.events[0] = nil
		c.events = c.events[1:]
		return ev, true, false
	}
	return nil, false, c.fin
}

// Call invokes method on the remote and decodes its result into result (nil to
// discard). If ctx ends first, Call returns ctx.Err() and the remote handler's
// context is cancelled. Safe for concurrent use, including from inside a
// Handler or an onEvent callback.
func (p *Peer) Call(ctx context.Context, method string, params any, result any) error {
	return p.do(ctx, method, params, false, nil, result)
}

// Stream is Call plus ordered events: onEvent receives the remote's emitted
// events one at a time, in order, on the calling goroutine, before the final
// result is returned. An onEvent error aborts the call (remote is cancelled)
// and is returned. A consumer that falls more than MaxBacklogBytes behind gets
// ErrBacklog.
func (p *Peer) Stream(ctx context.Context, method string, params any, onEvent func(json.RawMessage) error, result any) error {
	return p.do(ctx, method, params, true, onEvent, result)
}

func (p *Peer) do(ctx context.Context, method string, params any, stream bool, onEvent func(json.RawMessage) error, result any) error {
	if method == "" {
		return errors.New("execution: empty method")
	}
	if !p.charge(frameOverhead) {
		return ErrBacklog
	}
	defer p.release(frameOverhead)
	data, cost, err := p.prepare(ctx, method, params)
	if err != nil {
		return err
	}
	defer p.release(cost)
	if err := ctx.Err(); err != nil {
		return err
	}
	c := newCall(stream)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return p.closedErr()
	}
	p.nextID++
	id := p.nextID
	p.calls[id] = c
	p.mu.Unlock()
	defer func() {
		p.forget(id)
		p.release(c.abort(context.Canceled))
	}()

	if err := p.send(ctx, &frame{Type: tCall, ID: id, Method: method, Stream: stream, Data: data}); err != nil {
		p.forget(id)
		return err
	}
	return p.await(ctx, id, c, onEvent, result)
}

// prepare serializes parameter encoding with frame encoding and reserves the
// resulting bytes before another caller can encode. Caller-owned values and
// user MarshalJSON allocations are outside the transport's backlog budget.
func (p *Peer) prepare(ctx context.Context, method string, params any) (json.RawMessage, int64, error) {
	select {
	case p.wlock <- struct{}{}:
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	case <-p.base.Done():
		return nil, 0, p.closedErr()
	}
	defer func() { <-p.wlock }()
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if int64(len(method)) > p.lim.frame {
		return nil, 0, ErrFrameTooLarge
	}
	data, err := encodeValue(params)
	if err != nil {
		return nil, 0, fmt.Errorf("execution: encode params: %w", err)
	}
	cost := int64(len(data)) + int64(len(method))
	if cost > p.lim.frame {
		return nil, 0, ErrFrameTooLarge
	}
	if !p.charge(cost) {
		return nil, 0, ErrBacklog
	}
	return data, cost, nil
}

func (p *Peer) await(ctx context.Context, id uint64, c *call, onEvent func(json.RawMessage) error, result any) error {
	for {
		if err := ctx.Err(); err != nil {
			p.abandon(id, c)
			return err
		}
		ev, ok, done := c.next()
		switch {
		case ok:
			p.release(eventCost(ev))
			if onEvent != nil {
				if err := onEvent(ev); err != nil {
					p.abandon(id, c)
					return err
				}
			}
		case done:
			if c.err != nil {
				return c.err
			}
			if result == nil || len(c.res) == 0 {
				return nil
			}
			if err := json.Unmarshal(c.res, result); err != nil {
				return fmt.Errorf("execution: decode result: %w", err)
			}
			return nil
		default:
			select {
			case <-c.wake:
			case <-ctx.Done():
			}
		}
	}
}

// forget removes a call from the table, reporting whether it was still pending.
func (p *Peer) forget(id uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.calls[id]
	delete(p.calls, id)
	return ok
}

// abandon is the caller giving up: drop local state and tell the remote to cancel.
func (p *Peer) abandon(id uint64, c *call) {
	p.release(c.abort(context.Canceled))
	if p.forget(id) {
		p.control(&frame{Type: tCancel, ID: id})
	}
}

func (p *Peer) onReply(f *frame) {
	p.mu.Lock()
	c := p.calls[f.ID]
	p.mu.Unlock()
	if c == nil || !c.pending() {
		return
	}
	cost := eventCost(f.Data)
	if f.Error != nil {
		cost += int64(len(f.Error.Code) + len(f.Error.Message) + len(f.Error.Data))
	}
	if !p.charge(cost) {
		p.release(c.abort(ErrBacklog))
		return
	}
	var err error
	if f.Error != nil {
		err = f.Error
	}
	if !c.finish(f.Data, err, cost) {
		p.release(cost)
	}
}

func (p *Peer) onEvent(f *frame) {
	p.mu.Lock()
	c := p.calls[f.ID]
	p.mu.Unlock()
	if c == nil || !c.stream || !c.pending() {
		return
	}
	if len(f.Data) == 0 {
		f.Data = json.RawMessage("null")
	}
	cost := eventCost(f.Data)
	if !p.charge(cost) {
		p.release(c.abort(ErrBacklog))
		if p.forget(f.ID) {
			p.control(&frame{Type: tCancel, ID: f.ID})
		}
		return
	}
	if !c.push(f.Data) {
		p.release(cost)
	}
}
