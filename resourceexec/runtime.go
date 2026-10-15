// Package resourceexec runs resource connections in bounded helper processes.
package resourceexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"time"

	"github.com/Autumn-27/artex/execution"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

const WorkerCommand = "resource-worker"

var ErrBusy = errors.New("resourceexec: worker capacity busy")

// StartFunc allows the host to inject admission. Implementations must enforce
// the supplied OS limits before returning a Process; never execute in-process.
type StartFunc func(context.Context, string, []string, []string, execution.Limits, execution.Handler) (*execution.Process, error)
type Config struct {
	Executable string
	Args, Env  []string
	Limits     execution.Limits
	MCPLimits  execution.Limits
	MaxWorkers int
	Start      StartFunc
}

// Runtime is an immutable configuration snapshot with independent, nonblocking
// admission. Capacity is released only after the entire process tree is reaped.
type Runtime struct {
	config Config
	slots  chan struct{}
}

func New(c Config) (*Runtime, error) {
	if c.Limits.MemoryBytes == 0 {
		c.Limits.MemoryBytes = 512 << 20
	}
	if c.Limits.CPUs == 0 {
		c.Limits.CPUs = 1
	}
	if c.Limits.Processes == 0 {
		c.Limits.Processes = 128
	}
	if c.MaxWorkers == 0 {
		c.MaxWorkers = 4
	}
	if c.Limits.MemoryBytes < 0 || c.Limits.CPUs < 0 || math.IsNaN(c.Limits.CPUs) || math.IsInf(c.Limits.CPUs, 0) || c.Limits.Processes < 0 || c.MaxWorkers < 0 {
		return nil, errors.New("resourceexec: invalid limits or capacity")
	}
	if c.MCPLimits.MemoryBytes == 0 {
		c.MCPLimits.MemoryBytes = 384 << 20
	}
	if c.MCPLimits.CPUs == 0 {
		c.MCPLimits.CPUs = 1
	}
	if c.MCPLimits.Processes == 0 {
		c.MCPLimits.Processes = 128
	}
	if c.MCPLimits.MemoryBytes < 0 || c.MCPLimits.CPUs < 0 || math.IsNaN(c.MCPLimits.CPUs) || math.IsInf(c.MCPLimits.CPUs, 0) || c.MCPLimits.Processes < 0 {
		return nil, errors.New("resourceexec: invalid MCP limits")
	}
	if c.Executable == "" {
		var err error
		c.Executable, err = os.Executable()
		if err != nil {
			return nil, err
		}
	}
	if c.Args == nil {
		c.Args = []string{WorkerCommand}
	} else {
		c.Args = append([]string{}, c.Args...)
	}
	if c.Env != nil {
		c.Env = append([]string{}, c.Env...)
	}
	if c.Start == nil {
		c.Start = execution.Start
	}
	return &Runtime{config: c, slots: make(chan struct{}, c.MaxWorkers)}, nil
}

type MCPConfig struct {
	Name, Transport, Command, URL string
	Args                          []string
	Env                           map[string]string
	Insecure                      bool
}
type Client interface {
	Call(context.Context, string, any) (string, error)
	Tools(context.Context) ([]tool.CoreTool, error)
	Close() error
}
type initRequest struct {
	MCP    *MCPConfig
	Custom *CustomConfig
}
type metadata struct {
	Name, Description, Prompt string
	Schema                    map[string]any
}
type callContext struct {
	WorkingDir, AgentID, ToolUseID, OutputDir string
	MaxOutputChars                            int
	Env                                       []string
}

func packContext(tc *tool.ToolContext) callContext {
	if tc == nil {
		return callContext{}
	}
	return callContext{tc.WorkingDir, tc.AgentID, tc.ToolUseID, tc.OutputDir, tc.MaxOutputChars, tc.Env}
}
func (c callContext) unpack() *tool.ToolContext {
	return &tool.ToolContext{WorkingDir: c.WorkingDir, AgentID: c.AgentID, ToolUseID: c.ToolUseID, OutputDir: c.OutputDir, MaxOutputChars: c.MaxOutputChars, Env: c.Env}
}

type request struct {
	Name       string
	Input      json.RawMessage
	Context    callContext
	Permission permission.Context
}

type processClient struct {
	process  *execution.Process
	ctx      context.Context
	once     sync.Once
	closeErr error
}

func (r *Runtime) start(ctx context.Context, init initRequest) (*processClient, error) {
	select {
	case r.slots <- struct{}{}:
	default:
		return nil, ErrBusy
	}
	c := r.config
	limits := c.Limits
	if init.MCP != nil {
		limits = c.MCPLimits
	}
	p, err := c.Start(ctx, c.Executable, c.Args, c.Env, limits, nil)
	if err != nil {
		<-r.slots
		return nil, err
	}
	if p == nil {
		<-r.slots
		return nil, errors.New("resourceexec: Start returned nil process")
	}
	go func() {
		_ = p.Wait()
		if p.Close() == nil {
			<-r.slots
		}
	}()
	cl := &processClient{process: p, ctx: ctx}
	if err = p.Peer.Call(ctx, "init", init, nil); err != nil {
		if closeErr := cl.Close(); closeErr != nil {
			return nil, closeErr
		}
		return nil, err
	}
	return cl, nil
}
func (r *Runtime) ConnectMCP(ctx context.Context, c MCPConfig) (Client, error) {
	cl, err := r.start(ctx, initRequest{MCP: &c})
	if err != nil {
		return nil, err
	}
	return cl, nil
}
func (r *Runtime) RunCustom(ctx context.Context, c CustomConfig, in json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
	cl, err := r.start(ctx, initRequest{Custom: &c})
	if err != nil {
		return tool.Result{}, err
	}
	defer cl.Close()
	return (&proxyTool{metadata: metadata{Name: c.Name}, client: cl}).Call(ctx, in, tc)
}
func (c *processClient) Close() error {
	c.once.Do(func() { c.closeErr = c.process.Close() })
	return c.closeErr
}
func (c *processClient) Tools(ctx context.Context) ([]tool.CoreTool, error) {
	var list []metadata
	if err := c.process.Peer.Call(ctx, "tools", nil, &list); err != nil {
		return nil, err
	}
	out := make([]tool.CoreTool, 0, len(list))
	names := map[string]bool{}
	for _, m := range list {
		if names[m.Name] {
			return nil, fmt.Errorf("resourceexec: duplicate tool %q", m.Name)
		}
		names[m.Name] = true
		out = append(out, &proxyTool{metadata: m, client: c})
	}
	return out, nil
}

type proxyTool struct {
	metadata
	client *processClient
}

func (t *proxyTool) Name() string                { return t.metadata.Name }
func (t *proxyTool) Description() string         { return t.metadata.Description }
func (t *proxyTool) Prompt() string              { return t.metadata.Prompt }
func (t *proxyTool) InputSchema() map[string]any { return t.Schema }
func (t *proxyTool) flag(method string, in json.RawMessage) bool {
	ctx, cancel := context.WithTimeout(t.client.ctx, 10*time.Second)
	defer cancel()
	var b bool
	if err := t.client.process.Peer.Call(ctx, method, request{Name: t.Name(), Input: in}, &b); err != nil {
		return false
	}
	return b
}
func (t *proxyTool) IsReadOnly(in json.RawMessage) bool        { return t.flag("readonly", in) }
func (t *proxyTool) IsConcurrencySafe(in json.RawMessage) bool { return t.flag("concurrent", in) }
func (t *proxyTool) CheckPermissions(ctx context.Context, in json.RawMessage, pc permission.Context) permission.Decision {
	var out permission.Decision
	if err := t.client.process.Peer.Call(ctx, "permission", request{Name: t.Name(), Input: in, Permission: pc}, &out); err != nil {
		return permission.Denied(err.Error())
	}
	return out
}
func (t *proxyTool) Call(ctx context.Context, in json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
	var out tool.Result
	err := t.client.process.Peer.Stream(ctx, "call", request{Name: t.Name(), Input: in, Context: packContext(tc)}, func(raw json.RawMessage) error {
		var p tool.ProgressInfo
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		if tc != nil && tc.Emit != nil {
			tc.Emit(p)
		}
		return nil
	}, &out)
	// A canceled call invalidates this connection: even an uncooperative MCP and
	// its descendants must stop. Other concurrent calls receive a closed error.
	if ctx.Err() != nil {
		_ = t.client.Close()
		return out, ctx.Err()
	}
	return out, err
}

// Call is the raw text surface for backend batch jobs, preserving their existing
// lack of tool-output truncation. Agent calls use CoreTool.Call and Capture.
func (c *processClient) Call(ctx context.Context, name string, args any) (string, error) {
	in, err := json.Marshal(args)
	if err != nil {
		return "", err
	}
	var out string
	err = c.process.Peer.Call(ctx, "batch", request{Name: name, Input: in}, &out)
	if ctx.Err() != nil {
		_ = c.Close()
		return "", ctx.Err()
	}
	return out, err
}
