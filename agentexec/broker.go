package agentexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strconv"
	"strings"
	"sync"

	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

type modelStream struct {
	mu     sync.Mutex
	next   func() (llm.StreamEvent, error, bool)
	stop   func()
	cancel context.CancelFunc
}

func (s *Session) handle(rpc context.Context, method string, raw json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
	if strings.HasPrefix(method, "model.") || strings.HasPrefix(method, "compact.") || method == "hook.stop" || method == "hook.start" || method == "hook.prompt" || method == "reminder" || method == "newid" || method == "warn" {
		return s.onHostThread(rpc, func() (json.RawMessage, error) { return s.dispatch(rpc, method, raw, emit) })
	}
	return s.dispatch(rpc, method, raw, emit)
}
func (s *Session) dispatch(rpc context.Context, method string, raw json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
	var in rpcRequest
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	ctx, done := s.callbackContext(rpc)
	defer done()
	if !in.Deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, in.Deadline)
		defer cancel()
	}
	out := rpcReply{}
	var callErr error
	switch method {
	case "local.start":
		t := s.tools[in.Name]
		if t == nil {
			return nil, fmt.Errorf("agentexec: unknown local tool %q", in.Name)
		}
		if s.observer != nil {
			s.observer(ctx, t)
		}
	case "snapshot":
		s.snapshot(in.Messages)
	case "model.open":
		// Stream lifetime spans next calls, so it must not inherit this RPC's
		// short-lived handler context. Cancellation still follows host Prompt.
		streamCtx, cancel := context.WithCancel(s.currentContext())
		s.mu.RLock()
		hasTx, id := s.opts.Transcript != nil, s.opts.SessionID
		s.mu.RUnlock()
		if hasTx {
			streamCtx = withSession(streamCtx, id)
		}
		stream := s.opts.Deps.CallModel
		if in.Provider || stream == nil {
			stream = s.opts.Provider.Stream
		}
		next, stop := iter.Pull2(stream(streamCtx, in.Request))
		m := &modelStream{next: next, stop: stop, cancel: cancel}
		s.mu.Lock()
		if s.closing {
			s.mu.Unlock()
			cancel()
			stop()
			return nil, errors.New("agentexec: session closed")
		}
		s.nextStream++
		out.ID = strconv.FormatUint(s.nextStream, 10)
		s.streams[out.ID] = m
		s.mu.Unlock()
	case "model.next", "model.close":
		s.mu.RLock()
		m := s.streams[in.ID]
		s.mu.RUnlock()
		if m == nil {
			callErr = errNoStream
			break
		}
		m.mu.Lock()
		if method == "model.next" {
			stop := context.AfterFunc(rpc, m.cancel)
			out.Event, callErr, out.More = m.next()
			stop()
		}
		if method == "model.close" || !out.More {
			m.stop()
			m.cancel()
			s.mu.Lock()
			delete(s.streams, in.ID)
			s.mu.Unlock()
		}
		m.mu.Unlock()
	case "model.complete":
		complete := s.opts.Deps.CallModelSync
		if in.Provider || complete == nil {
			complete = s.opts.Provider.Complete
		}
		out.Message, out.Stop, out.Usage, callErr = complete(ctx, in.Request)
	case "tool.readonly", "tool.concurrent", "tool.permission", "tool.call":
		t := s.tools[in.Name]
		if t == nil {
			return nil, fmt.Errorf("agentexec: unknown assembled tool %q", in.Name)
		}
		switch method {
		case "tool.readonly":
			out.Bool = t.IsReadOnly(in.Input)
		case "tool.concurrent":
			out.Bool = t.IsConcurrencySafe(in.Input)
		case "tool.permission":
			out.Decision = t.CheckPermissions(ctx, in.Input, s.permissionContext())
		case "tool.call":
			tc := &tool.ToolContext{WorkingDir: s.opts.WorkingDir, ToolUseID: in.ToolUseID, MaxOutputChars: s.opts.MaxToolOutputChars, OutputDir: s.opts.ToolOutputDir, Env: append([]string(nil), s.opts.BashEnv...)}
			tc.Emit = func(p tool.ProgressInfo) {
				b, err := json.Marshal(p)
				if err == nil {
					_ = emit(b)
				}
			}
			out.Result, callErr = t.Call(ctx, in.Input, tc)

			if s.opts.UnlockSet != nil {
				out.Strings = s.opts.UnlockSet.List()
			}
		}
	case "todo.write":
		out.Result, callErr = s.opts.Todos.Tool().Call(ctx, in.Input, nil)
	case "permission":
		out.Decision = s.opts.CanUseTool(ctx, in.Name, in.Input, s.permissionContext())
	case "hook.pre":
		out.Bool, out.Text, out.Updated = s.opts.Hooks.PreToolUse(ctx, in.Name, in.Input)
	case "hook.post":
		s.opts.Hooks.PostToolUse(ctx, in.Name, in.Input, in.Result, in.IsError)
	case "hook.stop":
		out.Bool, out.Strings, out.Text = s.opts.Hooks.Stop(ctx, in.Messages)
	case "hook.start":
		s.opts.Hooks.(hookFirer).FireSessionStart(ctx)
	case "hook.prompt":
		out.Text = s.opts.Hooks.(hookFirer).FireUserPromptSubmit(ctx, in.Text)
	case "compact.pre":
		out.Messages = s.opts.Compactor.Pre(ctx, in.Messages, in.Tokens)
	case "compact.reactive":
		out.Messages, out.Bool = s.opts.Compactor.Reactive(ctx, in.Messages)
	case "compact.overflow":
		out.Bool = s.opts.Compactor.IsOverflow(s.refs.decode(in.Error))
	case "compact.view":
		out.Messages = s.opts.Compactor.(harness.ContextView).View(ctx, in.Messages)
	case "reminder":
		out.Text = s.opts.SystemReminderFunc()
	case "newid":
		out.Text = s.opts.Deps.NewID()
	case "warn":
		s.opts.OnWarn(in.Text)
	default:
		return nil, unknown(method)
	}
	// RawMessage(nil) becomes JSON null, which decodes as four input bytes.
	// Carry rewritten permission input separately to preserve nil vs literal null.
	if method == "permission" || method == "tool.permission" {
		out.Updated = out.Decision.UpdatedInput
		out.Decision.UpdatedInput = nil
	}
	out.Error = s.refs.encode(callErr)
	return marshal(out)
}
func (s *Session) permissionContext() permission.Context {
	mode := s.opts.PermissionMode
	if mode == "" {
		mode = permission.ModeDefault
	}
	return permission.Context{Mode: mode, WorkingDir: s.opts.WorkingDir, Allowed: s.opts.AllowedTools, Disallowed: s.opts.DisallowedTools}
}
