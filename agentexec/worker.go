package agentexec

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"os"
	"sync"
	"time"

	"github.com/Autumn-27/artex/execution"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/compaction"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// WorkerConfig registers trusted constructors before serving any session. A
// constructor receives only the host-supplied LocalToolSpec.Data.
type WorkerConfig struct {
	LocalTools map[string]LocalToolConstructor
}

// ServeWorker serves stdio only. Call it from cmd before config/Server/PG init.
// Stdout is reserved exclusively for execution frames.
func ServeWorker(ctx context.Context, config WorkerConfig) error {
	return Serve(ctx, os.Stdin, os.Stdout, config)
}
func Serve(ctx context.Context, r io.ReadCloser, w io.WriteCloser, config WorkerConfig) error {
	base, cancel := context.WithCancel(ctx)
	defer cancel()
	worker := &worker{config: config, ready: make(chan struct{}), base: base}
	worker.peer = execution.NewPeer(r, w, worker.handle)
	close(worker.ready)
	stop := context.AfterFunc(ctx, func() { _ = worker.peer.Close() })
	defer stop()
	<-worker.peer.Done()
	cancel()
	worker.mu.Lock()
	if worker.cancel != nil {
		worker.cancel()
	}
	worker.mu.Unlock()
	// After transport loss, let the executable exit even if a local tool
	// ignores cancellation. The supervisor still reaps the entire process tree.
	cleaned := make(chan struct{})
	go func() {
		defer close(cleaned)
		worker.op.Lock()
		defer worker.op.Unlock()
		if worker.stop != nil {
			worker.stop()
		}
		if worker.session != nil {
			_ = worker.session.Close()
		}
	}()
	select {
	case <-cleaned:
	case <-time.After(100 * time.Millisecond):
	}

	if errors.Is(worker.peer.Err(), io.EOF) || errors.Is(worker.peer.Err(), execution.ErrClosed) {
		return nil
	}
	return worker.peer.Err()
}

type worker struct {
	base         context.Context
	config       WorkerConfig
	peer         *execution.Peer
	ready        chan struct{}
	op           sync.Mutex
	mu           sync.Mutex
	session      *agentcore.Session
	ctx          context.Context
	cancel       context.CancelFunc
	fatal        error
	next         func() (harness.Event, error, bool)
	stop         func()
	unlock       *tool.UnlockSet
	hostHooks    bool
	todos        bool
	observeLocal bool
}

func (w *worker) fail(err error) {
	if err == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ctx != nil && w.ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		return
	}
	if w.fatal == nil {
		w.fatal = err
	}
	if w.cancel != nil {
		w.cancel()
	}
}
func (w *worker) context() context.Context {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ctx != nil {
		return w.ctx
	}
	return w.base
}
func (w *worker) call(ctx context.Context, method string, in rpcRequest) (rpcReply, error) {
	var out rpcReply
	err := w.peer.Call(ctx, method, in, &out)
	if err == nil {
		err = (*errorRefs)(nil).decode(out.Error)
	}
	return out, err
}
func (w *worker) must(ctx context.Context, method string, in rpcRequest) rpcReply {
	out, err := w.call(ctx, method, in)
	w.fail(err)
	return out
}
func (w *worker) handle(ctx context.Context, method string, raw json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
	<-w.ready
	if method == "cancel" {
		w.mu.Lock()
		if w.cancel != nil {
			w.cancel()
		}
		w.mu.Unlock()
		return marshal(rpcReply{})
	}
	w.op.Lock()
	defer w.op.Unlock()
	switch method {
	case "init":
		if w.session != nil {
			return nil, errors.New("agentexec: already initialized")
		}
		var init initRequest
		if err := json.Unmarshal(raw, &init); err != nil {
			return nil, err
		}
		opts, err := w.options(ctx, init)
		if err != nil {
			return nil, err
		}
		w.session = agentcore.NewSession(opts)
		w.mu.Lock()
		fatal := w.fatal
		w.mu.Unlock()
		if fatal != nil {
			return nil, fatal
		}
		return marshal(rpcReply{})
	case "resume":
		if w.session == nil || w.next != nil {
			return nil, errors.New("agentexec: resume outside idle session")
		}
		var id string
		if err := json.Unmarshal(raw, &id); err != nil {
			return nil, err
		}
		err := w.session.Resume(id)
		return marshal(eventReply{Error: (*errorRefs)(nil).encode(err), Messages: w.session.Messages()})
	case "prompt":
		if w.session == nil || w.next != nil {
			return nil, errors.New("agentexec: prompt outside idle session")
		}
		var in struct {
			Input    string
			Unlocked []string
			Deadline time.Time
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			return nil, err
		}
		if w.unlock != nil {
			w.unlock.Add(in.Unlocked...)
		}
		run, cancel := context.WithCancel(w.base)
		if !in.Deadline.IsZero() {
			cancel()
			run, cancel = context.WithDeadline(w.base, in.Deadline)
		}
		w.mu.Lock()
		w.ctx, w.cancel, w.fatal = run, cancel, nil
		w.mu.Unlock()
		events := w.session.Prompt(run, in.Input)
		w.next, w.stop = iter.Pull2(events)
		return marshal(eventReply{Messages: w.session.Messages()})
	case "next":
		if w.next == nil {
			return nil, errors.New("agentexec: next without prompt")
		}
		stopCancel := context.AfterFunc(ctx, func() {
			w.mu.Lock()
			if w.cancel != nil {
				w.cancel()
			}
			w.mu.Unlock()
		})
		defer stopCancel()
		ev, err, more := w.next()
		w.mu.Lock()
		fatal := w.fatal
		w.mu.Unlock()
		if fatal != nil && more {
			err = fatal
		}
		out := packEvent(ev, err, more)
		if !more {
			out.Messages = w.session.Messages()
			w.stop()
			w.next, w.stop = nil, nil
			w.mu.Lock()
			w.cancel()
			w.mu.Unlock()
		}
		return marshal(out)
	default:
		return nil, unknown(method)
	}
}
func (w *worker) options(ctx context.Context, init initRequest) (agentcore.Options, error) {
	var opts agentcore.Options
	if err := json.Unmarshal(init.Options, &opts); err != nil {
		return opts, err
	}
	w.observeLocal = init.ObserveLocal
	opts.Provider = &brokerProvider{w: w, provider: true}
	deps := &brokerProvider{w: w}
	opts.Deps.CallModel, opts.Deps.CallModelSync = deps.Stream, deps.Complete
	opts.Tools = make([]tool.CoreTool, 0, len(init.Tools)) // preserve explicit empty
	for _, spec := range init.Tools {
		t, err := w.makeTool(w.base, spec)
		if err != nil {
			return opts, err
		}
		opts.Tools = append(opts.Tools, t)
	}
	if init.HasTranscript {
		opts.Transcript = transcript.NewStore(init.TranscriptDir)
	}
	if init.Unlock {
		w.unlock = tool.NewUnlockSet(init.Unlocked...)
		opts.UnlockSet = w.unlock
	}
	if init.CanUse {
		opts.CanUseTool = w.canUse
	}
	w.hostHooks, w.todos = init.Hooks, init.Todos
	if init.Todos {
		opts.Todos = tool.NewTodoStore()
		data, err := json.Marshal(map[string]any{"todos": init.TodoItems})
		if err != nil {
			return opts, err
		}
		if len(init.TodoItems) > 0 {
			if _, err := opts.Todos.Tool().Call(ctx, data, nil); err != nil {
				return opts, err
			}
		}
	}
	if init.Hooks || init.Todos {
		opts.Hooks = &brokerHooks{w}
		if init.HookFirer {
			opts.Hooks = &brokerLifecycleHooks{brokerHooks{w}}
		}
	}
	if init.Warn {
		opts.OnWarn = func(text string) { w.must(w.context(), "warn", rpcRequest{Text: text}) }
	}
	if init.Reminder {
		opts.SystemReminderFunc = func() string { return w.must(w.context(), "reminder", rpcRequest{}).Text }
	}
	if init.NewID {
		opts.Deps.NewID = func() string { return w.must(w.context(), "newid", rpcRequest{}).Text }
	}
	var compact harness.Compactor
	if init.Compactor {
		compact = &brokerCompactor{w}
	} else if opts.Compaction != nil {
		compact = compaction.New(*opts.Compaction, compaction.ProviderSummarizer(opts.Provider, opts.MaxTokens, opts.NonStreaming))
	}
	if init.Compactor && opts.Compaction != nil && opts.OnWarn != nil {
		opts.OnWarn("agentcore: both Compactor and Compaction are set — Compaction is IGNORED")
	}
	opts.Compaction = nil
	tracker := &snapshotCompactor{w: w, inner: compact}
	opts.Compactor = tracker
	if init.ContextView {
		opts.Compactor = &snapshotView{tracker}
	}
	return opts, nil
}
func withSession(ctx context.Context, id string) context.Context {
	return transcript.WithSessionID(ctx, id)
}
