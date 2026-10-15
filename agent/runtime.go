package agent

import (
	"context"
	"errors"
	"iter"
	"sync"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
)

// RuntimeSession is the host-facing session contract. Prompt accepts the user
// message when called; the returned iterator drives the loop.
type RuntimeSession interface {
	Messages() []llm.Message
	Prompt(context.Context, string) iter.Seq2[harness.Event, error]
	Resume(string) error
	Close() error
}
type SessionFactory func(context.Context, agentcore.Options) (RuntimeSession, error)

var runtimeFactory struct {
	sync.RWMutex
	factory SessionFactory
}

// SetSessionFactory replaces the factory atomically. nil explicitly selects
// the original in-process SDK. Factory failures never fall back to in-process.
func SetSessionFactory(factory SessionFactory) {
	runtimeFactory.Lock()
	defer runtimeFactory.Unlock()
	runtimeFactory.factory = factory
}
func NewRuntimeSession(ctx context.Context, opts agentcore.Options) (RuntimeSession, error) {
	runtimeFactory.RLock()
	factory := runtimeFactory.factory
	runtimeFactory.RUnlock()
	if factory == nil {
		return agentcore.NewSession(opts), nil
	}
	s, err := factory(ctx, opts)
	if err == nil && s == nil {
		err = errors.New("agent: session factory returned nil")
	}
	return s, err
}

// acceptedPrompt observes the synchronous acceptance part of Prompt. A remote
// transport failure before acceptance must not acknowledge a queued message.
func acceptedPrompt(s RuntimeSession, ctx context.Context, input string, onPrompt func()) iter.Seq2[harness.Event, error] {
	if onPrompt == nil {
		return s.Prompt(ctx, input)
	}
	before := len(s.Messages())
	events := s.Prompt(ctx, input)
	if len(s.Messages()) > before {
		onPrompt()
	}
	return events
}
