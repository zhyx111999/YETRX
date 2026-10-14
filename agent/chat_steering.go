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

// steeredSession uses Norma's public request boundary and Prompt APIs. A queued
// instruction ends the loop before its next HTTP request, then starts the next
// Prompt on the same session. Thus tools finish normally, instructions become
// real user messages in the child transcript, and upstream SDK code is intact.
// The boundary is internal: it emits neither a failure nor a completion.
type steeredSession struct {
	session         *agentcore.Session
	pending         []string
	restarted       bool
	calls, maxCalls int
	started         time.Time
	duration        time.Duration
}

func newSteeredSession(opts *agentcore.Options, drain func() []string) *steeredSession {
	s := &steeredSession{maxCalls: opts.MaxTurns, duration: opts.MaxDuration, started: time.Now()}
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
	before := func() error {
		// Norma enforces the ordinary turn/settlement budget. Only a steering
		// restart needs this cumulative guard against resetting that budget.
		if s.restarted && (s.maxCalls > 0 && s.calls >= s.maxCalls || s.duration > 0 && time.Since(s.started) >= s.duration) {
			return ErrChatBudget
		}
		if notes := drain(); len(notes) > 0 {
			s.pending = notes
			return errSteeringBoundary
		}
		s.calls++
		return nil
	}
	opts.Deps.CallModel = func(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return func(yield func(llm.StreamEvent, error) bool) {
			if err := before(); err != nil {
				yield(llm.StreamEvent{}, err)
				return
			}
			for ev, err := range stream(ctx, req) {
				if !yield(ev, err) {
					return
				}
			}
		}
	}
	opts.Deps.CallModelSync = func(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
		if err := before(); err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		return complete(ctx, req)
	}
	s.session = agentcore.NewSession(*opts)
	return s
}
func (s *steeredSession) Messages() []llm.Message { return s.session.Messages() }
func (s *steeredSession) Prompt(ctx context.Context, input string) iter.Seq2[harness.Event, error] {
	return func(yield func(harness.Event, error) bool) {
		var previous llm.Usage
		for {
			boundary := false
			for ev, err := range s.session.Prompt(ctx, input) {
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
			}
			if !boundary || ctx.Err() != nil {
				return
			}
			s.restarted = true
			input = strings.Join(s.pending, "\n\n")
			s.pending = nil
		}
	}
}
