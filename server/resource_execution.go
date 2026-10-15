package server

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"

	"github.com/Autumn-27/artex/resourceexec"
	actool "github.com/Autumn-27/norma/tool"
)

var resourceRuntime atomic.Pointer[resourceexec.Runtime]

// ConfigureResourceExecution installs a complete immutable snapshot before server
// startup. nil explicitly disables isolation. Failed configuration leaves the old
// snapshot intact; a failed worker launch never falls back to in-process work.
func ConfigureResourceExecution(c *resourceexec.Config) error {
	if c == nil {
		resourceRuntime.Store(nil)
		return nil
	}
	r, err := resourceexec.New(*c)
	if err != nil {
		return err
	}
	resourceRuntime.Store(r)
	return nil
}

// ServeResourceWorker is the early cmd resource-worker entry point (before PG).
func ServeResourceWorker(ctx context.Context) error { return resourceexec.ServeResourceWorker(ctx) }

const CustomLocalToolKind = "artex.custom.v1"

// WorkerLocalTools supplies constructors with exactly the bridge constructor
// signature. Convert each value to agentexec.LocalToolConstructor when registering
// WorkerConfig; this package deliberately does not depend on agentexec internals.
func WorkerLocalTools() map[string]func(context.Context, json.RawMessage) (actool.CoreTool, error) {
	return map[string]func(context.Context, json.RawMessage) (actool.CoreTool, error){
		CustomLocalToolKind: func(_ context.Context, raw json.RawMessage) (actool.CoreTool, error) {
			var c resourceexec.CustomConfig
			if err := json.Unmarshal(raw, &c); err != nil {
				return nil, err
			}
			return c.Tool(), nil
		},
	}
}

// LocalTool returns trusted placement data from the assembled tool identity,
// never from a name lookup or model arguments. Empty kind means host/broker.
// The caller should fall through to agentexec.SDKLocalTool for SDK builtins.
func LocalTool(t actool.CoreTool) (kind string, data json.RawMessage, err error) {
	for depth := 0; t != nil; depth++ {
		if depth >= 64 {
			return "", nil, fmt.Errorf("resourceexec: tool unwrap cycle/depth exceeded")
		}
		if c, ok := t.(*preparedCustomTool); ok {
			data, err = json.Marshal(c.config)
			return CustomLocalToolKind, data, err
		}
		wrapped, ok := t.(interface{ UnwrapTool() actool.CoreTool })
		if !ok {
			break
		}
		t = wrapped.UnwrapTool()
	}
	return "", nil, nil
}

type preparedCustomTool struct {
	actool.CoreTool
	config resourceexec.CustomConfig
}

func (t *preparedCustomTool) UnwrapTool() actool.CoreTool { return t.CoreTool }
func (t *preparedCustomTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	return runCustomResource(ctx, t.config, in, tc)
}

func (s *Server) customConfig(key, kind string, raw json.RawMessage) resourceexec.CustomConfig {
	c := resourceexec.CustomConfig{Name: key, Kind: kind, Exec: append(json.RawMessage(nil), raw...)}
	if s.m != nil {
		c.WorkingDir = s.m.dir
		if kind == "script" {
			c.Python = s.pythonInterpreter()
		}
		if kind == "http" {
			c.RecordingProxy = s.m.ProxyAddr()
			c.RecordingCA = s.m.ProxyCACert()
		}
	}
	return c
}
func runCustomResource(ctx context.Context, c resourceexec.CustomConfig, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	if r := resourceRuntime.Load(); r != nil {
		return r.RunCustom(ctx, c, in, tc)
	}
	return c.Call(ctx, in, tc)
}
