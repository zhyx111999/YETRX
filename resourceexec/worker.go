package resourceexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/Autumn-27/artex/execution"
	"github.com/Autumn-27/norma/tool"
)

// ServeResourceWorker must be called by cmd before configuration/DB/server init.
// Stdout belongs exclusively to execution.Peer. One helper owns one connection.
func ServeResourceWorker(ctx context.Context) error { return Serve(ctx, os.Stdin, os.Stdout) }
func Serve(ctx context.Context, r io.ReadCloser, w io.WriteCloser) error {
	base, cancel := context.WithCancel(ctx)
	defer cancel()
	worker := &resourceWorker{ctx: base}
	peer := execution.NewPeer(r, w, worker.handle)
	stop := context.AfterFunc(ctx, func() { _ = peer.Close() })
	defer stop()
	<-peer.Done()
	cancel()
	worker.mu.Lock()
	cl := worker.client
	worker.closed = true
	worker.mu.Unlock()
	if cl != nil {
		_ = cl.Close()
	}
	if errors.Is(peer.Err(), io.EOF) || errors.Is(peer.Err(), execution.ErrClosed) {
		return nil
	}
	return peer.Err()
}

type resourceWorker struct {
	ctx                 context.Context
	mu                  sync.RWMutex
	initialized, closed bool
	client              Client
	tools               map[string]tool.CoreTool
	order               []metadata
}

func (w *resourceWorker) handle(ctx context.Context, method string, raw json.RawMessage, emit func(json.RawMessage) error) (json.RawMessage, error) {
	if method == "init" {
		return w.init(ctx, raw)
	}
	w.mu.RLock()
	ready := w.initialized && w.tools != nil
	w.mu.RUnlock()
	if !ready {
		return nil, errors.New("resourceexec: worker not initialized")
	}
	if method == "tools" {
		w.mu.RLock()
		defer w.mu.RUnlock()
		return json.Marshal(w.order)
	}
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if method == "batch" {
		w.mu.RLock()
		cl := w.client
		w.mu.RUnlock()
		if cl == nil {
			return nil, errors.New("resourceexec: batch requires MCP")
		}
		out, err := cl.Call(ctx, req.Name, req.Input)
		if err != nil {
			return nil, err
		}
		return json.Marshal(out)
	}
	w.mu.RLock()
	t := w.tools[req.Name]
	w.mu.RUnlock()
	if t == nil {
		return nil, fmt.Errorf("resourceexec: unknown tool %q", req.Name)
	}
	switch method {
	case "readonly":
		return json.Marshal(t.IsReadOnly(req.Input))
	case "concurrent":
		return json.Marshal(t.IsConcurrencySafe(req.Input))
	case "permission":
		return json.Marshal(t.CheckPermissions(ctx, req.Input, req.Permission))
	case "call":
		tc := req.Context.unpack()
		tc.Emit = func(p tool.ProgressInfo) { b, _ := json.Marshal(p); _ = emit(b) }
		res, err := t.Call(ctx, req.Input, tc)
		if err != nil {
			return nil, err
		}
		return json.Marshal(res)
	default:
		return nil, execution.MethodNotFound(method)
	}
}
func (w *resourceWorker) init(ctx context.Context, raw json.RawMessage) (json.RawMessage, error) {
	w.mu.Lock()
	if w.initialized || w.closed {
		w.mu.Unlock()
		return nil, errors.New("resourceexec: duplicate/closed init")
	}
	w.initialized = true
	w.mu.Unlock()
	var req initRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if (req.MCP == nil) == (req.Custom == nil) {
		return nil, errors.New("resourceexec: choose exactly one resource")
	}
	var ts []tool.CoreTool
	var cl Client
	if req.Custom != nil {
		ts = []tool.CoreTool{req.Custom.Tool()}
	} else {
		var err error
		cl, err = ConnectDirect(ctx, w.ctx, *req.MCP)
		if err != nil {
			return nil, mcpUnavailable(ctx, err)
		}
		ts, err = cl.Tools(ctx)
		if err != nil {
			_ = cl.Close()
			return nil, mcpUnavailable(ctx, err)
		}
	}
	tools := map[string]tool.CoreTool{}
	var order []metadata
	for _, t := range ts {
		if _, ok := tools[t.Name()]; ok {
			if cl != nil {
				_ = cl.Close()
			}
			return nil, fmt.Errorf("resourceexec: duplicate tool %q", t.Name())
		}
		tools[t.Name()] = t
		order = append(order, metadata{t.Name(), t.Description(), t.Prompt(), t.InputSchema()})
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		if cl != nil {
			_ = cl.Close()
		}
		return nil, errors.New("resourceexec: worker closed")
	}
	w.client, w.tools, w.order = cl, tools, order
	return json.RawMessage("null"), nil
}
