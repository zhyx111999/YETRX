package agent

import (
	"context"
	"errors"
	"iter"
	"strings"
	"time"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

var errSteeringBoundary = errors.New("native chat steering boundary")
var ErrChatBudget = errors.New("chat execution budget exhausted")

// steeredSession drives the ChatControl boundary callbacks (see ChatControl for
// the contract) using only Norma's public request boundary and Prompt APIs.
//
// A drained batch ends the loop before its next HTTP request, then starts the
// next Prompt on the same session: tools finish normally, the batch becomes a
// real user message after the completed tool results, and upstream SDK code is
// intact. The boundary is internal: it emits neither a failure nor a
// completion. The session never drops a batch it drained but did not deliver;
// it simply never calls OnPrompt for it, which is how the host knows.
//
// It carries no host types and depends on agentcore.Options only, so every
// agent loop built from those options can use it.
type steeredSession struct {
	session         *agentcore.Session
	control         ChatControl
	pending         []string
	restarted       bool
	calls, maxCalls int
	started         time.Time
	duration        time.Duration
	// A non-streaming response reaches the loop as one block that it replays as
	// events; tail counts the replayed events the consumer has yet to see.
	tail int
	// flush commits text the capture layer still buffers, so the response is
	// fully recorded before OnResponseEnd fires.
	flush func()
}

func newControlledSession(opts agentcore.Options, control ChatControl) (*agentcore.Session, *steeredSession) {
	s := &steeredSession{control: control, maxCalls: opts.MaxTurns, duration: opts.MaxDuration, started: time.Now()}
	if s.maxCalls > 0 && opts.Settlement != nil {
		s.maxCalls += opts.Settlement.MaxTurns
	}
	stream, complete := opts.Deps.CallModel, opts.Deps.CallModelSync
	if stream == nil {
		stream = opts.Provider.Stream
	}
	if complete == nil {
		complete = opts.Provider.Complete
	}
	opts.Deps.CallModel = func(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			if err := s.before(); err != nil {
				yield(llm.StreamEvent{}, err)
				return
			}
			tools := 0
			for ev, err := range stream(ctx, req) {
				if err == nil && ev.Type == llm.SEToolUseStart {
					tools++
				}
				if !yield(ev, err) {
					return
				}
			}
			// The provider stream is exhausted and the loop consumed every event.
			if tools > 0 {
				s.responseEnded(ctx)
			}
		}
	}
	opts.Deps.CallModelSync = func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if err := s.before(); err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		msg, stop, usage, err := complete(ctx, req)
		if err == nil && len(msg.ToolUses()) > 0 {
			s.tail = replayedEvents(msg)
		}
		return msg, stop, usage, err
	}
	s.session = agentcore.NewSession(opts)
	return s.session, s
}

// replayedEvents counts the events the loop emits when it replays a complete
// message: non-empty thinking and text blocks, and every tool call.
func replayedEvents(msg llm.Message) int {
	n := 0
	for _, b := range msg.Content {
		switch b.Type {
		case llm.BlockThinking:
			if b.Thinking != "" {
				n++
			}
		case llm.BlockText:
			if b.Text != "" {
				n++
			}
		case llm.BlockToolUse:
			n++
		}
	}
	return n
}

func (s *steeredSession) before() error {
	// Norma enforces the ordinary turn/settlement budget. Only a steering
	// restart needs this cumulative guard against resetting that budget.
	if s.restarted && (s.maxCalls > 0 && s.calls >= s.maxCalls || s.duration > 0 && time.Since(s.started) >= s.duration) {
		return ErrChatBudget
	}
	if s.control.DrainMessages != nil {
		if batch := s.control.DrainMessages(); len(batch) > 0 {
			s.pending = batch
			return errSteeringBoundary
		}
	}
	s.calls++
	return nil
}

// responseEnded reports a response that asked for tools, unless the run was
// cancelled: a cancelled response is not a boundary anything may be inserted at.
func (s *steeredSession) responseEnded(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	if s.flush != nil {
		s.flush()
	}
	if s.control.OnResponseEnd != nil {
		s.control.OnResponseEnd()
	}
}

// afterEvent runs once the consumer has handled ev. Non-streaming responses
// have no end-of-stream: their last replayed event is the end.
func (s *steeredSession) afterEvent(ctx context.Context, ev harness.Event) {
	if s.tail == 0 {
		return
	}
	switch ev.Kind {
	case harness.KindThinking, harness.KindText, harness.KindToolUse:
		if s.tail--; s.tail == 0 {
			s.responseEnded(ctx)
		}
	}
}

// setResponseFlush lets the capture layer commit buffered text before the host
// is told that a response ended.
func (s *steeredSession) setResponseFlush(flush func()) { s.flush = flush }

func (s *steeredSession) Messages() []llm.Message { return s.session.Messages() }
func (s *steeredSession) Prompt(ctx context.Context, input string) iter.Seq2[harness.Event, error] {
	return func(yield func(harness.Event, error) bool) {
		var previous llm.Usage
		for {
			// Session.Prompt appends and records the user message when called.
			events := s.session.Prompt(ctx, input)
			if s.control.OnPrompt != nil {
				s.control.OnPrompt()
			}
			s.tail = 0
			boundary := false
			for ev, err := range events {
				if errors.Is(err, errSteeringBoundary) {
					boundary = true
					continue
				}
				if ev.Kind == harness.KindResult && ev.Terminal != nil {
					if errors.Is(ev.Terminal.Err, errSteeringBoundary) {
						previous.Add(ev.Terminal.Usage)
						boundary = true
						continue
					}
					terminal := *ev.Terminal
					terminal.Usage.Add(previous)
					ev.Terminal = &terminal
				}
				if ev.Kind == harness.KindUsage && ev.Usage != nil {
					u := *ev.Usage
					u.Add(previous)
					ev.Usage = &u
				}
				if !yield(ev, err) {
					return
				}
				s.afterEvent(ctx, ev)
			}
			batch := s.pending
			s.pending = nil
			if !boundary || ctx.Err() != nil {
				return // A drained batch that never reached OnPrompt stays with the host.
			}
			s.restarted = true
			input = strings.Join(batch, "\n\n")
		}
	}
}
