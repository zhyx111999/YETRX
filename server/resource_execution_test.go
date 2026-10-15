package server

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/execution"
	"github.com/Autumn-27/artex/resourceexec"
	actool "github.com/Autumn-27/norma/tool"
)

type resourceTestWrapper struct {
	actool.CoreTool
	original actool.CoreTool
}

func (t *resourceTestWrapper) UnwrapTool() actool.CoreTool { return t.original }

func TestResourceCustomPlacementAndObservation(t *testing.T) {
	original := (&Server{}).buildCustomTool(&db.Tool{Key: "fixture", Kind: "command", Exec: json.RawMessage(`{"command":"echo {value}"}`)})
	schema := map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string", "default": "host-default"}}}
	recorder := &recordingToolUsage{}
	resolved := &resourceTestWrapper{CoreTool: meterTool(agent.DecorateTool(original, "edited", schema), recorder, "fixture", "worker", agent.RunInfo{TaskID: 123, SessionID: "session"}), original: original}
	kind, data, err := LocalTool(resolved)
	if err != nil || kind != CustomLocalToolKind {
		t.Fatalf("placement %s %v", kind, err)
	}
	child, err := WorkerLocalTools()[kind](context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	if child.Name() != resolved.Name() || resolved.Description() != "edited" || resolved.InputSchema()["properties"] == nil {
		t.Fatal("metadata/identity lost")
	}
	if len(recorder.rows) != 0 {
		t.Fatal("preparation executed a tool")
	}
	// A tool with the same name must never acquire custom command placement.
	forged := actool.Build(actool.Spec{Name: "fixture"})
	kind, _, err = LocalTool(forged)
	if kind != "" || err != nil {
		t.Fatal("name lookup granted local execution")
	}

}

func TestResourceConfigurationRaceAndFailClosed(t *testing.T) {
	t.Cleanup(func() { _ = ConfigureResourceExecution(nil) })
	config := resourceexec.Config{Start: func(context.Context, string, []string, []string, execution.Limits, execution.Handler) (*execution.Process, error) {
		return nil, execution.ErrLimitsUnavailable
	}}
	if err := ConfigureResourceExecution(&config); err != nil {
		t.Fatal(err)
	}
	_, err := connectMCP(context.Background(), &db.MCPServer{Name: "test", Transport: "stdio", Command: "must-not-execute"})
	if !errors.Is(err, execution.ErrLimitsUnavailable) {
		t.Fatalf("MCP fallback: %v", err)
	}
	_, err = runCustomResource(context.Background(), resourceexec.CustomConfig{Name: "test", Kind: "command", Exec: json.RawMessage(`{"command":"must-not-execute"}`)}, nil, nil)
	if !errors.Is(err, execution.ErrLimitsUnavailable) {
		t.Fatalf("custom fallback: %v", err)
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			for range 20 {
				if err := ConfigureResourceExecution(&config); err != nil {
					t.Error(err)
				}
				_ = resourceRuntime.Load()
			}
		})
	}
	wg.Wait()
	old := resourceRuntime.Load()
	invalid := resourceexec.Config{MaxWorkers: -1}
	if ConfigureResourceExecution(&invalid) == nil || resourceRuntime.Load() != old {
		t.Fatal("invalid snapshot replaced good config")
	}
}
