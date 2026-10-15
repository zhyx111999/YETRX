// Package agentexec runs Norma sessions in supervised child processes. The host
// retains providers, credentials, policies and application tools.
package agentexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

type wireError struct {
	Ref           uint64
	Message, Code string
}
type remoteError struct{ wireError }

func (e *remoteError) Error() string { return e.Message }
func (e *remoteError) Is(target error) bool {
	return e.Code == "canceled" && target == context.Canceled || e.Code == "deadline" && target == context.DeadlineExceeded
}

type errorRefs struct {
	mu   sync.Mutex
	next uint64
	refs map[uint64]error
}

func (r *errorRefs) encode(err error) *wireError {
	if err == nil {
		return nil
	}
	var remote *remoteError
	if errors.As(err, &remote) {
		return &remote.wireError
	}
	w := &wireError{Message: err.Error()}
	if errors.Is(err, context.Canceled) {
		w.Code = "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		w.Code = "deadline"
	}
	if r != nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.next++
		w.Ref = r.next
		if r.refs == nil {
			r.refs = map[uint64]error{}
		}
		r.refs[w.Ref] = err
	}
	return w
}
func (r *errorRefs) decode(w *wireError) error {
	if w == nil {
		return nil
	}
	if r != nil && w.Ref != 0 {
		r.mu.Lock()
		err := r.refs[w.Ref]
		r.mu.Unlock()
		if err != nil {
			return err
		}
		return fmt.Errorf("agentexec: unknown error reference %d", w.Ref)
	}
	return &remoteError{*w}
}

// Errors never enter JSON's error interface (which would encode as {}).
type eventReply struct {
	Event                harness.Event
	Error, TerminalError *wireError
	More                 bool
	Messages             []llm.Message
}

func packEvent(ev harness.Event, err error, more bool) eventReply {
	out := eventReply{Event: ev, Error: (*errorRefs)(nil).encode(err), More: more}
	if ev.Terminal != nil {
		t := *ev.Terminal
		out.TerminalError = (*errorRefs)(nil).encode(t.Err)
		t.Err = nil
		out.Event.Terminal = &t
	}
	return out
}
func (r *eventReply) unpack(refs *errorRefs) (harness.Event, error) {
	if r.Event.Terminal != nil {
		r.Event.Terminal.Err = refs.decode(r.TerminalError)
	}
	return r.Event, refs.decode(r.Error)
}

type rpcRequest struct {
	Deadline       time.Time
	Name, Text, ID string
	Input, Result  json.RawMessage
	Permission     permission.Context
	ToolUseID      string
	Request        llm.CompletionRequest
	Messages       []llm.Message
	Tokens         int
	IsError        bool
	Error          *wireError
	Provider       bool
}
type rpcReply struct {
	Error          *wireError
	Text, ID, Stop string
	Bool, More     bool
	Updated        []byte
	Strings        []string
	Decision       permission.Decision
	Result         tool.Result
	Message        llm.Message
	Messages       []llm.Message
	Usage          llm.Usage
	Event          llm.StreamEvent
}

func marshal(v any) (json.RawMessage, error) { return json.Marshal(v) }
