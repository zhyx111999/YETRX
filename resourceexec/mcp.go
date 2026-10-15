package resourceexec

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

// This independent adapter adds progress/cancellation without modifying norma.
// One connection owns initialization, listing and every subsequent call.
type mcpFrame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}
type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *mcpError) Error() string { return fmt.Sprintf("MCP error %d: %s", e.Code, e.Message) }

type mcpConnection struct {
	config    MCPConfig
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	next      int64
	pending   map[int64]chan mcpFrame
	send      func(context.Context, mcpFrame) error
	shutdown  func() error
	closeOnce sync.Once
}

// ConnectDirect is ONLY for the resource worker or explicitly disabled isolation.
// initCtx bounds handshake; lifetime owns the connection after initialization.
func ConnectDirect(initCtx, lifetime context.Context, cfg MCPConfig) (Client, error) {
	ctx, cancel := context.WithCancel(lifetime)
	c := &mcpConnection{config: cfg, ctx: ctx, cancel: cancel, pending: map[int64]chan mcpFrame{}}
	stop := context.AfterFunc(initCtx, cancel)
	defer stop()
	var err error
	switch cfg.Transport {
	case "stdio":
		err = c.openStdio()
	case "http", "sse":
		err = c.openHTTP()
	default:
		err = fmt.Errorf("resourceexec: unknown MCP transport %q", cfg.Transport)
	}
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	version := "2025-06-18"
	if cfg.Transport == "sse" {
		version = "2024-11-05"
	}
	_, err = c.call(initCtx, "initialize", map[string]any{"protocolVersion": version, "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "artex-resourceexec", "version": "1"}}, nil)
	if err == nil {
		err = c.notify(initCtx, "notifications/initialized", map[string]any{})
	}
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	go func() { <-ctx.Done(); _ = c.Close() }()
	return c, nil
}
func (c *mcpConnection) Close() error {
	c.closeOnce.Do(func() {
		c.cancel()
		if c.shutdown != nil {
			_ = c.shutdown()
		}
	})
	return nil
}
func (c *mcpConnection) openStdio() error {
	if c.config.Command == "" {
		return errors.New("resourceexec: stdio command missing")
	}
	cmd := exec.CommandContext(c.ctx, c.config.Command, c.config.Args...)
	cmd.Env = os.Environ()
	for k, v := range c.config.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	// Never forward uncontrolled MCP stderr (which can contain environment secrets).
	cmd.Stderr = io.Discard
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		return err
	}
	if err = cmd.Start(); err != nil {
		in.Close()
		out.Close()
		return err
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); c.cancel(); close(done) }()
	var writeMu sync.Mutex
	c.send = func(ctx context.Context, f mcpFrame) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		return json.NewEncoder(in).Encode(f)
	}
	c.shutdown = func() error { in.Close(); _ = cmd.Process.Kill(); out.Close(); <-done; return nil }
	go func() {
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 64<<10), 8<<20)
		for sc.Scan() {
			var f mcpFrame
			if json.Unmarshal(sc.Bytes(), &f) == nil {
				c.deliver(f)
			}
		}
		c.cancel()
	}()
	return nil
}
func (c *mcpConnection) deliver(f mcpFrame) {
	id := f.ID
	if f.Method == "notifications/progress" {
		var p struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		}
		if json.Unmarshal(f.Params, &p) != nil {
			return
		}
		if json.Unmarshal(p.ProgressToken, &id) != nil {
			var s string
			_ = json.Unmarshal(p.ProgressToken, &s)
			id, _ = strconv.ParseInt(s, 10, 64)
		}
	}
	if id == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if ch := c.pending[id]; ch != nil {
		select {
		case ch <- f:
		default:
			c.cancel()
		}
	}
}
func (c *mcpConnection) notify(ctx context.Context, method string, params any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.send(ctx, mcpFrame{JSONRPC: "2.0", Method: method, Params: raw})
}
func (c *mcpConnection) call(ctx context.Context, method string, params map[string]any, progress func(tool.ProgressInfo)) (json.RawMessage, error) {
	if c.config.Transport == "http" {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 120*time.Second)
		defer cancel()
	}

	c.mu.Lock()
	c.next++
	id := c.next
	ch := make(chan mcpFrame, 64)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	if method == "tools/call" {
		params["_meta"] = map[string]any{"progressToken": id}
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	requestCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	sent := make(chan error, 1)
	go func() { sent <- c.send(requestCtx, mcpFrame{JSONRPC: "2.0", ID: id, Method: method, Params: raw}) }()
	for {
		select {
		case err := <-sent:
			if err != nil {
				return nil, err
			}
		case <-ctx.Done():
			// The host also closes the entire worker on cancellation, bounding servers
			// that ignore this protocol notification.
			go func() {
				nctx, ncancel := context.WithTimeout(c.ctx, time.Second)
				defer ncancel()
				_ = c.notify(nctx, "notifications/cancelled", map[string]any{"requestId": id, "reason": "caller canceled"})
			}()
			return nil, ctx.Err()
		case <-c.ctx.Done():
			return nil, errors.New("resourceexec: MCP connection closed")
		case f := <-ch:
			if f.Method == "notifications/progress" {
				if progress != nil {
					var p struct {
						Message  string   `json:"message"`
						Progress float64  `json:"progress"`
						Total    *float64 `json:"total"`
					}
					if json.Unmarshal(f.Params, &p) == nil {
						msg := p.Message
						if msg == "" {
							msg = fmt.Sprintf("progress %g", p.Progress)
							if p.Total != nil {
								msg += fmt.Sprintf("/%g", *p.Total)
							}
						}
						progress(tool.ProgressInfo{Message: msg})
					}
				}
				continue
			}
			if f.Error != nil {
				return nil, f.Error
			}
			return f.Result, nil
		}
	}
}

type remoteTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func (c *mcpConnection) Tools(ctx context.Context) ([]tool.CoreTool, error) {
	var out []tool.CoreTool
	seen := map[string]bool{}
	cursors := map[string]bool{}
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(ctx, "tools/list", params, nil)
		if err != nil {
			return nil, err
		}
		var res struct {
			Tools []remoteTool `json:"tools"`
			Next  string       `json:"nextCursor"`
		}
		if err = json.Unmarshal(raw, &res); err != nil {
			return nil, err
		}
		for _, rt := range res.Tools {
			if seen[rt.Name] {
				return nil, fmt.Errorf("resourceexec: duplicate MCP tool %q", rt.Name)
			}
			seen[rt.Name] = true
			out = append(out, c.wrap(rt))
		}
		if res.Next == "" {
			return out, nil
		}
		if cursors[res.Next] {
			return nil, errors.New("resourceexec: repeated tools/list cursor")
		}
		cursors[res.Next] = true
		cursor = res.Next
	}
}
func (c *mcpConnection) wrap(rt remoteTool) tool.CoreTool {
	schema := rt.InputSchema
	if schema == nil {
		schema = map[string]any{"type": "object"}
	}
	name := "mcp__" + c.config.Name + "__" + rt.Name
	return tool.Build(tool.Spec{Name: name, Description: rt.Description, Schema: schema,
		// Preserve SDK fail-closed scheduling and approval behavior: remote MCP
		// annotations are advisory, not authority to change host permissions.
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.AskUser("call MCP tool " + name + "?")
		},
		Run: func(ctx context.Context, in json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
			var args any
			if len(in) > 0 {
				if err := json.Unmarshal(in, &args); err != nil {
					return tool.Result{}, err
				}
			}
			var emit func(tool.ProgressInfo)
			if tc != nil {
				emit = tc.Emit
			}
			raw, err := c.call(ctx, "tools/call", map[string]any{"name": rt.Name, "arguments": args}, emit)
			if err != nil {
				return tool.Errorf("Error: " + err.Error()), nil
			}
			var res struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			}
			if err = json.Unmarshal(raw, &res); err != nil {
				return tool.Errorf("Error: bad MCP response: " + err.Error()), nil
			}
			text := ""
			for _, b := range res.Content {
				text += b.Text
			}
			r := tool.Text(tool.Capture(tc, text))
			r.IsError = res.IsError
			return r, nil
		}})
}

func (c *mcpConnection) Call(ctx context.Context, name string, args any) (string, error) {
	raw, err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, nil)
	if err != nil {
		return "", err
	}
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err = json.Unmarshal(raw, &res); err != nil {
		return "", err
	}
	text := ""
	for _, b := range res.Content {
		text += b.Text
	}
	if res.IsError {
		return text, fmt.Errorf("MCP tool %q returned an error: %s", name, text)
	}
	return text, nil
}
