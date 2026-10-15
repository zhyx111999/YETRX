package agentexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"slices"
	"sync"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/execution"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

const WorkerCommand = "agent-session-worker"

// Config controls process creation. Env nil inherits the environment; production
// hosts should pass an explicit environment. LocalTool, when provided, makes all
// placement decisions (nil spec means host). By default SDK DefaultTools names
// use child SDK implementations; all other assembled tools stay on the host.
type Config struct {
	// OnLocalToolStart runs synchronously immediately before each child-local
	// assembled tool Call. The original host tool (including wrappers) is passed
	// for attribution. Traverse UnwrapTool() to find metering; never Call it again.
	// A failed RPC prevents local execution. Crashes after this callback retain
	// the usage count, matching host meteredTool's pre-call semantics.
	OnLocalToolStart func(context.Context, tool.CoreTool)

	Executable string
	Args, Env  []string
	Limits     execution.Limits
	LocalTool  func(tool.CoreTool) (*LocalToolSpec, error)
}

func SDKLocalTool(t tool.CoreTool) (*LocalToolSpec, error) {
	for _, sdk := range tool.DefaultTools() {
		if sdk.Name() == t.Name() {
			data, _ := json.Marshal(t.Name())
			return &LocalToolSpec{Kind: "sdk", Data: data}, nil
		}
	}
	return nil, nil
}
func NewFactory(config Config) agent.SessionFactory {
	config.Args = slices.Clone(config.Args)
	if config.Env != nil {
		config.Env = append([]string{}, config.Env...)
	}
	return func(ctx context.Context, opts agentcore.Options) (agent.RuntimeSession, error) {
		local := config.LocalTool
		if local == nil {
			local = SDKLocalTool
		}
		init, tools, err := prepare(opts, local)
		init.ObserveLocal = config.OnLocalToolStart != nil
		if err != nil {
			return nil, err
		}
		exe := config.Executable
		if exe == "" {
			exe, err = os.Executable()
			if err != nil {
				return nil, err
			}
		}
		args := config.Args
		if args == nil {
			args = []string{WorkerCommand}
		}
		if opts.SessionID == "" {
			opts.SessionID = transcript.NewSessionID()
			var fields map[string]any
			if err = json.Unmarshal(init.Options, &fields); err != nil {
				return nil, err
			}
			fields["SessionID"] = opts.SessionID
			init.Options, err = json.Marshal(fields)
			if err != nil {
				return nil, err
			}
		}
		s := &Session{opts: opts, tools: tools, ctx: ctx, streams: map[string]*modelStream{}, callbacks: make(chan hostCallback), observer: config.OnLocalToolStart}
		s.process, err = execution.Start(ctx, exe, args, config.Env, config.Limits, s.handle)
		if err != nil {
			return nil, err
		}
		if err = s.call(ctx, "init", init, nil); err != nil {
			s.Close()
			return nil, err
		}
		return s, nil
	}
}

// Session allows concurrent Messages/Close. Prompt and Resume are exclusive.
// Any transport failure or early consumer exit ends the process; no fallback.
type Session struct {
	process    *execution.Process
	opts       agentcore.Options
	tools      map[string]tool.CoreTool
	refs       errorRefs
	op         sync.Mutex
	mu         sync.RWMutex
	ctx        context.Context
	messages   []llm.Message
	streams    map[string]*modelStream
	nextStream uint64
	callbacks  chan hostCallback
	observer   func(context.Context, tool.CoreTool)
	closing    bool
	closeOnce  sync.Once
	closeErr   error
}

func (s *Session) PID() int { return s.process.PID() }
func (s *Session) Messages() []llm.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneMessages(s.messages)
}
func cloneMessages(messages []llm.Message) []llm.Message {
	out := slices.Clone(messages)
	for i := range out {
		out[i].Content = cloneBlocks(out[i].Content)
	}
	return out
}
func cloneBlocks(blocks []llm.ContentBlock) []llm.ContentBlock {
	out := slices.Clone(blocks)
	for i := range out {
		out[i].Input = slices.Clone(out[i].Input)
		out[i].Content = cloneBlocks(out[i].Content)
	}
	return out
}
func (s *Session) snapshot(m []llm.Message) {
	s.mu.Lock()
	s.messages = cloneMessages(m)
	s.mu.Unlock()
}
func (s *Session) currentContext() context.Context { s.mu.RLock(); defer s.mu.RUnlock(); return s.ctx }
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closing = true
		streams := make([]*modelStream, 0, len(s.streams))
		for _, stream := range s.streams {
			streams = append(streams, stream)
			stream.cancel()
		}
		s.mu.Unlock()
		s.closeErr = s.process.Close()
		// A provider may ignore cancellation. Never join its coroutine on Close.
		for _, stream := range streams {
			go func() { stream.mu.Lock(); defer stream.mu.Unlock(); stream.stop() }()
		}
	})
	return s.closeErr
}
func (s *Session) Resume(id string) error {
	s.op.Lock()
	defer s.op.Unlock()
	var out eventReply
	if err := s.call(s.currentContext(), "resume", id, &out); err != nil {
		return err
	}
	s.snapshot(out.Messages)
	if out.Error == nil {
		s.mu.Lock()
		s.opts.SessionID = id
		s.mu.Unlock()
	}
	return s.refs.decode(out.Error)
}
func (s *Session) Prompt(ctx context.Context, input string) iter.Seq2[harness.Event, error] {
	s.op.Lock()
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
	unlocked := []string(nil)
	if s.opts.UnlockSet != nil {
		unlocked = s.opts.UnlockSet.List()
	}
	var accepted eventReply
	err := s.call(ctx, "prompt", struct {
		Input    string
		Unlocked []string
	}{input, unlocked}, &accepted)
	if err == nil {
		err = s.refs.decode(accepted.Error)
		s.snapshot(accepted.Messages)
	}
	// Prompt has already accepted the message when it returns, matching SDK
	// semantics required by ChatControl.OnPrompt.
	return func(yield func(harness.Event, error) bool) {
		defer s.op.Unlock()
		finished := false
		defer func() {
			if !finished {
				_ = s.Close()
			}
		}()
		if err != nil {
			yield(harness.Event{}, err)
			return
		}
		for {
			var out eventReply
			if err := s.call(ctx, "next", nil, &out); err != nil {
				yield(harness.Event{}, err)
				return
			}
			if !out.More {
				s.snapshot(out.Messages)
				finished = true
				return
			}
			ev, err := out.unpack(&s.refs)
			if ev.Terminal != nil {
				s.snapshot(ev.Terminal.Messages)
			}
			if !yield(ev, err) {
				return
			}
		}
	}
}

// The broker derives identity exclusively from the original host context.
// RPC context cancellation is merged without accepting child identity claims.
func (s *Session) callbackContext(rpc context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(s.currentContext())
	stop := context.AfterFunc(rpc, cancel)
	s.mu.RLock()
	hasTranscript, id := s.opts.Transcript != nil, s.opts.SessionID
	s.mu.RUnlock()
	if hasTranscript {
		ctx = transcript.WithSessionID(ctx, id)
	}
	return ctx, func() { stop(); cancel() }
}

var errNoStream = errors.New("agentexec: unknown model stream")

func unknown(method string) error { return fmt.Errorf("agentexec: unknown method %q", method) }
