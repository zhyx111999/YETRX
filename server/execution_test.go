package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/agentexec"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/execution"
	"github.com/Autumn-27/artex/resourceexec"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

// Use the exact production dispatch, before TestMain can inspect PG/config.
func init() {
	if os.Getenv("YETRX_EXECUTION_FIXTURE") == "1" {
		worker, err := RunExecutionWorker(context.Background(), os.Args[1:])
		if !worker || err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(0)
	}
}

func processFactoryForTest(t *testing.T) agent.SessionFactory {
	t.Helper()
	if os.Getenv("YETRX_EXECUTION_LINUX_TEST") == "1" {
		if err := execution.InitCgroup(); err != nil {
			t.Fatal(err)
		}
	}
	return agentexec.NewFactory(agentexec.Config{
		Env:       append(executionEnvironment(os.Environ()), "YETRX_EXECUTION_FIXTURE=1"),
		Limits:    execution.Limits{MemoryBytes: 768 << 20, CPUs: 1, Processes: 128},
		LocalTool: executionToolPlacement, OnLocalToolStart: recordLocalToolStart,
	})
}

func TestExecutionProcessKeepsSharedRolesAndResponseDelivery(t *testing.T) {
	agent.SetSessionFactory(singleWriterSessionFactory(processFactoryForTest(t)))
	limits := execution.Limits{MemoryBytes: 768 << 20, CPUs: 1, Processes: 128}
	if err := ConfigureResourceExecution(&resourceexec.Config{Env: append(executionEnvironment(os.Environ()), "YETRX_EXECUTION_FIXTURE=1"), Limits: limits, MCPLimits: limits, MaxWorkers: 8}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.SetSessionFactory(nil); _ = ConfigureResourceExecution(nil) })
	t.Run("roles", TestSharedExecutionRolesUseTheSameCatalog)
	t.Run("mcp-unavailable", func(t *testing.T) {
		e := newNativeEnv(t)
		id, err := e.pg.SaveMCP(&db.MCPServer{Name: "invalid_mcp_fixture", Transport: "http", Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = e.pg.DeleteMCP(id) })
		tools, def, cleanup := agent.ToolAugment(t.Context(), "agent")
		if cleanup != nil {
			cleanup()
		}
		if def.Err != nil || len(tools) == 0 {
			t.Fatalf("optional MCP blocked shared resources: %v", def.Err)
		}
		current := resourceRuntime.Load()
		t.Cleanup(func() { resourceRuntime.Store(current) })
		if err := ConfigureResourceExecution(&resourceexec.Config{Start: func(context.Context, string, []string, []string, execution.Limits, execution.Handler) (*execution.Process, error) {
			return nil, execution.ErrLimitsUnavailable
		}}); err != nil {
			t.Fatal(err)
		}
		_, def, cleanup = agent.ToolAugment(t.Context(), "agent")
		if cleanup != nil {
			cleanup()
		}
		if !errors.Is(def.Err, execution.ErrLimitsUnavailable) {
			t.Fatalf("hard isolation failure ignored: %v", def.Err)
		}
	})
	t.Run("response", TestResponseBoundaryShowsResultsAfterTheResponseAndTheNextRequestReadsThem)
	t.Run("final", TestResponseBoundaryFinalResponseKeepsAutomaticFollowUp)
	t.Run("stop", TestResponseBoundaryStopKeepsDisplayedResultAndFollowsUpWithoutRerunningTheTool)
	t.Run("delete", TestResponseBoundaryDeletionMutesDisplayedResultsAndNeverRevives)
	t.Run("retest", TestResponseBoundaryRetestDistinguishesDisplayedFromRead)
	t.Run("stream", TestConversationStreamDeliversPartialTextAndReconnectsWithoutDuplicatingTheAnswer)
	t.Run("stream-stop", TestConversationStreamCancellationRetainsPartialTextAndClearsTheDraft)
}

type executionFakeSession struct {
	closeErr error
	closes   atomic.Int32
}

func (*executionFakeSession) Messages() []llm.Message { return nil }
func (*executionFakeSession) Prompt(context.Context, string) iter.Seq2[harness.Event, error] {
	return func(func(harness.Event, error) bool) {}
}
func (*executionFakeSession) Resume(string) error { return nil }
func (s *executionFakeSession) Close() error      { s.closes.Add(1); return s.closeErr }

func TestExecutionAdmissionKeepsWriterWithoutSharedCountCap(t *testing.T) {
	var next *executionFakeSession
	factory := singleWriterSessionFactory(func(context.Context, agentcore.Options) (agent.RuntimeSession, error) { return next, nil })
	store := transcript.NewStore(t.TempDir())
	opts := agentcore.Options{Transcript: store, SessionID: "same"}
	next = &executionFakeSession{}
	s, err := factory(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := factory(t.Context(), opts); !errors.Is(err, ErrTranscriptBusy) {
		t.Fatalf("concurrent transcript writer admitted: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil || next.closes.Load() != 1 {
		t.Fatal("non-idempotent close")
	}
	next = &executionFakeSession{closeErr: errors.New("tree still alive")}
	s, err = factory(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if s.Close() == nil {
		t.Fatal("cleanup failure hidden")
	}
	if _, err := factory(t.Context(), opts); !errors.Is(err, ErrTranscriptBusy) {
		t.Fatal("unreaped writer released")
	}
	for i := range 40 {
		next = &executionFakeSession{}
		other, err := factory(t.Context(), agentcore.Options{Transcript: store, SessionID: fmt.Sprintf("other-%d", i)})
		if err != nil {
			t.Fatalf("independent writer %d blocked by a shared count cap: %v", i, err)
		}
		defer other.Close()
	}
}

func TestExecutionLocalCatalogMeterAndShellManager(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	var calls int
	provider := &responseModel{t: t, steps: []modelStep{
		{tools: []llm.ContentBlock{toolCall("custom-1", "local_fixture", `{}`), toolCall("shell-1", "shell_list", `{}`)}},
		{chunks: []string{"done"}},
	}, onRequest: func(n int, m []llm.Message) { calls = n }}
	original := (&Server{}).buildCustomTool(&db.Tool{Key: "local_fixture", Kind: "command", Exec: json.RawMessage(`{"command":"echo {value}"}`)})
	schema := map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string", "default": "process-default"}}}
	recorder := &recordingToolUsage{}
	decorated := meterTool(agent.DecorateTool(original, "custom", schema), recorder, "local_fixture", "agent", agent.RunInfo{SessionID: "host-session"})
	shell := actool.NewShellList()
	tools := []actool.CoreTool{decorated, shell}
	factory := processFactoryForTest(t)
	s, err := factory(ctx, agentcore.Options{Provider: provider, Tools: tools, WorkingDir: t.TempDir(), NonStreaming: true, PermissionMode: permission.ModeBypass, MaxTurns: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for ev, err := range s.Prompt(ctx, "run fixtures") {
		if err != nil {
			t.Fatal(err)
		}
		if ev.ToolResult != nil && ev.ToolResult.IsError {
			t.Fatalf("local fixture failed: %+v", ev.ToolResult)
		}
	}
	if calls != 2 || !strings.Contains(string(mustJSON(s.Messages())), "process-default") {
		t.Fatal("local result or scalar default missing")
	}
	if len(recorder.rows) != 1 || recorder.rows[0].SessionID != "host-session" {
		t.Fatalf("local attribution: %+v", recorder.rows)
	}
}

func TestExecutionEnvironmentAndWorkerDispatch(t *testing.T) {
	env := executionEnvironment([]string{"PATH=fixture", "ARTEX_PG_DSN=private", "artex_config=private", "HTTP_PROXY=http://localhost", "TOOL_ENV=retained"})
	if len(env) != 3 || strings.Contains(strings.Join(env, "\n"), "private") {
		t.Fatalf("worker bootstrap environment: %v", env)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("YETRX_EXECUTION_LINUX_TEST") == "1" {
		if err := execution.InitCgroup(); err != nil {
			t.Fatal(err)
		}
	}
	if err := probeExecution(t.Context(), exe, append(env, "YETRX_EXECUTION_FIXTURE=1"), execution.Limits{MemoryBytes: 768 << 20, CPUs: 1, Processes: 128}); err != nil {
		t.Fatal(err)
	}
	if yes, err := RunExecutionWorker(t.Context(), []string{"-addr", ":0"}); yes || err != nil {
		t.Fatalf("normal server dispatch: %v %v", yes, err)
	}
}
