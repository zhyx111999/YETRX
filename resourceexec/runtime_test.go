package resourceexec

import (
	"bufio"
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/execution"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

func TestMain(m *testing.M) {
	switch os.Getenv("YETRX_RESOURCE_TEST_MODE") {
	case "worker":
		if ServeResourceWorker(context.Background()) != nil {
			os.Exit(2)
		}
		os.Exit(0)
	case "mcp":
		fixtureStdio()
		os.Exit(0)
	case "idle":
		for {
			time.Sleep(time.Hour)
		}
	}
	if os.Getenv("YETRX_EXECUTION_LINUX_TEST") == "1" {
		if err := execution.InitCgroup(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	os.Exit(m.Run())
}
func fixtureStdio() {
	var mu sync.Mutex
	send := func(f mcpFrame) { mu.Lock(); defer mu.Unlock(); _ = json.NewEncoder(os.Stdout).Encode(f) }
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var f mcpFrame
		if json.Unmarshal(scanner.Bytes(), &f) != nil || f.ID == 0 {
			continue
		}
		go fixtureReply(f, send)
	}
}
func fixtureReply(f mcpFrame, send func(mcpFrame)) {
	reply := func(v any) { raw, _ := json.Marshal(v); send(mcpFrame{JSONRPC: "2.0", ID: f.ID, Result: raw}) }
	switch f.Method {
	case "initialize":
		reply(map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "serverInfo": map[string]string{"name": "fake", "version": "1"}})
	case "tools/list":
		list := []any{map[string]any{"name": "probe", "description": "fixture metadata", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"mode": map[string]any{"type": "string"}}}}}
		if os.Getenv("RESOURCE_DUPLICATE") == "yes" {
			list = append(list, list[0])
		}
		reply(map[string]any{"tools": list})
	case "tools/call":
		var p struct {
			Arguments struct {
				Mode string `json:"mode"`
			} `json:"arguments"`
			Meta struct {
				Token any `json:"progressToken"`
			} `json:"_meta"`
		}
		_ = json.Unmarshal(f.Params, &p)
		progress, _ := json.Marshal(map[string]any{"progressToken": p.Meta.Token, "progress": 1, "total": 2, "message": "fixture progress"})
		send(mcpFrame{JSONRPC: "2.0", Method: "notifications/progress", Params: progress})
		if p.Arguments.Mode == "hang" {
			return
		} // intentionally ignores protocol cancellation
		if p.Arguments.Mode == "rpc-error" {
			send(mcpFrame{JSONRPC: "2.0", ID: f.ID, Error: &mcpError{Code: -32000, Message: "fixture failure"}})
			return
		}
		child := 0
		if p.Arguments.Mode == "spawn" {
			exe, _ := os.Executable()
			cmd := exec.Command(exe)
			cmd.Env = append(os.Environ(), "YETRX_RESOURCE_TEST_MODE=idle")
			if cmd.Start() == nil {
				child = cmd.Process.Pid
				go cmd.Wait()
			}
		}
		text, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "parent": os.Getppid(), "descendant": child, "env": os.Getenv("RESOURCE_TEST_SECRET")})
		reply(map[string]any{"content": []any{map[string]string{"type": "text", "text": string(text)}}, "isError": p.Arguments.Mode == "tool-error"})
	}
}
func testRuntime(t *testing.T, max int) (*Runtime, *atomic.Int64) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	pid := new(atomic.Int64)
	r, err := New(Config{Executable: exe, Args: []string{}, Env: append(os.Environ(), "YETRX_RESOURCE_TEST_MODE=worker", "ARTEX_CONFIG=C:/__resourceexec_absent_config__"), MaxWorkers: max, MCPLimits: execution.Limits{MemoryBytes: 768 << 20},
		Start: func(ctx context.Context, exe string, args, env []string, limits execution.Limits, h execution.Handler) (*execution.Process, error) {
			if limits.MemoryBytes <= 0 || limits.CPUs <= 0 || limits.Processes <= 0 {
				t.Error("missing real limits")
			}
			p, err := execution.Start(ctx, exe, args, env, limits, h)
			if err == nil {
				pid.Store(int64(p.PID()))
			}
			return p, err
		}})
	if err != nil {
		t.Fatal(err)
	}
	return r, pid
}
func stdioConfig(t *testing.T) MCPConfig {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return MCPConfig{Name: "fixture", Transport: "stdio", Command: exe, Env: map[string]string{"YETRX_RESOURCE_TEST_MODE": "mcp", "RESOURCE_TEST_SECRET": "passed-secret"}}
}
func connectTest(t *testing.T, r *Runtime, ctx context.Context, cfg MCPConfig) Client {
	t.Helper()
	cl, err := r.ConnectMCP(ctx, cfg)
	if errors.Is(err, execution.ErrLimitsUnavailable) {
		t.Skipf("OS hard limits unavailable (no fallback): %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	return cl
}
func oneTool(t *testing.T, cl Client) tool.CoreTool {
	t.Helper()
	ts, err := cl.Tools(context.Background())
	if err != nil || len(ts) != 1 {
		t.Fatalf("tools=%v err=%v", ts, err)
	}
	return ts[0]
}
func assertDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processAlive(pid) {
		t.Fatalf("PID %d survived tree cleanup", pid)
	}
}
func TestMCPStdioProcess(t *testing.T) {
	r, pid := testRuntime(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cl := connectTest(t, r, ctx, stdioConfig(t))
	probe := oneTool(t, cl)
	if pid.Load() == int64(os.Getpid()) || pid.Load() == 0 {
		t.Fatal("execution stayed on host")
	}
	if probe.Name() != "mcp__fixture__probe" || probe.Description() != "fixture metadata" || probe.InputSchema()["type"] != "object" {
		t.Fatal("metadata lost")
	}
	if probe.IsReadOnly(nil) || probe.IsConcurrencySafe(nil) || probe.CheckPermissions(ctx, nil, permission.Context{}).Behavior != permission.Ask {
		t.Fatal("flags/permissions changed")
	}
	progress := []string{}
	res, err := probe.Call(ctx, json.RawMessage(`{"mode":"spawn"}`), &tool.ToolContext{Emit: func(p tool.ProgressInfo) { progress = append(progress, p.Message) }})
	if err != nil || res.IsError {
		t.Fatalf("call=%+v err=%v", res, err)
	}
	var info struct {
		PID, Parent, Descendant int
		Env                     string
	}
	if err = json.Unmarshal([]byte(res.Flatten()), &info); err != nil {
		t.Fatal(err)
	}
	if info.PID == os.Getpid() || info.Parent != int(pid.Load()) || info.Descendant == 0 || info.Env != "passed-secret" {
		t.Fatalf("bad process/env evidence: %+v worker=%d", info, pid.Load())
	}
	if len(progress) != 1 || progress[0] != "fixture progress" {
		t.Fatalf("progress=%v", progress)
	}
	for _, mode := range []string{"tool-error", "rpc-error"} {
		res, err = probe.Call(ctx, json.RawMessage(fmt.Sprintf(`{"mode":%q}`, mode)), nil)
		if err != nil || !res.IsError {
			t.Fatalf("%s: result=%+v err=%v", mode, res, err)
		}
	}
	text, err := cl.Call(ctx, "probe", map[string]any{})
	if err != nil || !strings.Contains(text, "pid") {
		t.Fatalf("batch %q %v", text, err)
	}
	if _, err = r.ConnectMCP(ctx, stdioConfig(t)); !errors.Is(err, ErrBusy) {
		t.Fatalf("expected nonblocking busy, got %v", err)
	}
	if err = cl.Close(); err != nil {
		t.Fatal(err)
	}
	assertDead(t, int(pid.Load()))
	assertDead(t, info.PID)
	assertDead(t, info.Descendant)
}
func TestMCPCancelAndTimeout(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(strconv.FormatBool(deadline), func(t *testing.T) {
			r, pid := testRuntime(t, 1)
			cl := connectTest(t, r, context.Background(), stdioConfig(t))
			probe := oneTool(t, cl)
			spawned, err := probe.Call(context.Background(), json.RawMessage(`{"mode":"spawn"}`), nil)
			if err != nil {
				t.Fatal(err)
			}
			var info struct{ PID, Descendant int }
			_ = json.Unmarshal([]byte(spawned.Flatten()), &info)
			var ctx context.Context
			var cancel context.CancelFunc
			if deadline {
				ctx, cancel = context.WithTimeout(context.Background(), 300*time.Millisecond)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			_, err = probe.Call(ctx, json.RawMessage(`{"mode":"hang"}`), &tool.ToolContext{Emit: func(tool.ProgressInfo) {
				if !deadline {
					cancel()
				}
			}})
			want := context.Canceled
			if deadline {
				want = context.DeadlineExceeded
			}
			if !errors.Is(err, want) {
				t.Fatalf("want %v got %v", want, err)
			}
			assertDead(t, int(pid.Load()))
			assertDead(t, info.PID)
			assertDead(t, info.Descendant)
		})
	}
}
func TestMCPDuplicateAndLimitsFailure(t *testing.T) {
	r, _ := testRuntime(t, 1)
	cfg := stdioConfig(t)
	cfg.Env["RESOURCE_DUPLICATE"] = "yes"
	cl, err := r.ConnectMCP(context.Background(), cfg)
	if cl != nil {
		cl.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate accepted: %v", err)
	}
	var calls atomic.Int64
	failing, err := New(Config{Start: func(context.Context, string, []string, []string, execution.Limits, execution.Handler) (*execution.Process, error) {
		calls.Add(1)
		return nil, execution.ErrLimitsUnavailable
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = failing.ConnectMCP(context.Background(), stdioConfig(t))
	if !errors.Is(err, execution.ErrLimitsUnavailable) || calls.Load() != 1 {
		t.Fatalf("failure silently bypassed: %v", err)
	}
}
func TestMCPHTTPProcessProgressAndCancel(t *testing.T) {
	var initialized, listed atomic.Int64
	cancelled := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(204)
			return
		}
		if r.Header.Get("Authorization") != "Bearer fixture-secret" {
			t.Error("header lost")
			w.WriteHeader(401)
			return
		}
		var f mcpFrame
		if json.NewDecoder(r.Body).Decode(&f) != nil {
			w.WriteHeader(400)
			return
		}
		if f.Method == "initialize" {
			initialized.Add(1)
			w.Header().Set("Mcp-Session-Id", "fixture-session")
		} else if r.Header.Get("Mcp-Session-Id") != "fixture-session" {
			t.Error("session changed")
		}
		if f.Method == "tools/list" {
			listed.Add(1)
		}
		if f.ID == 0 {
			w.WriteHeader(202)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fixtureReply(f, func(reply mcpFrame) {
			raw, _ := json.Marshal(reply)
			fmt.Fprintf(w, "data: %s\n\n", raw)
			w.(http.Flusher).Flush()
		})
		if strings.Contains(string(f.Params), `"hang"`) {
			<-r.Context().Done()
			select {
			case cancelled <- struct{}{}:
			default:
			}
		}
	}))
	defer srv.Close()
	r, pid := testRuntime(t, 1)
	cfg := MCPConfig{Name: "fixture", Transport: "http", URL: srv.URL, Env: map[string]string{"Authorization": "Bearer fixture-secret"}}
	cl := connectTest(t, r, context.Background(), cfg)
	probe := oneTool(t, cl)
	if pid.Load() == 0 || int(pid.Load()) == os.Getpid() {
		t.Fatal("no real worker")
	}
	var progress []string
	res, err := probe.Call(context.Background(), json.RawMessage(`{}`), &tool.ToolContext{Emit: func(p tool.ProgressInfo) { progress = append(progress, p.Message) }})
	if err != nil || res.IsError || len(progress) != 1 {
		t.Fatalf("result=%+v err=%v progress=%v", res, err, progress)
	}
	if initialized.Load() != 1 || listed.Load() != 1 {
		t.Fatalf("connection not reused: init=%d list=%d", initialized.Load(), listed.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err = probe.Call(ctx, json.RawMessage(`{"mode":"hang"}`), &tool.ToolContext{Emit: func(tool.ProgressInfo) { cancel() }})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP request did not cancel")
	}
	assertDead(t, int(pid.Load()))
}
func TestCustomProcessDefaultsEnvironment(t *testing.T) {
	interp, err := exec.LookPath("python")
	if err != nil {
		interp, err = exec.LookPath("python3")
	}
	if err != nil {
		t.Skip("Python unavailable")
	}
	r, pid := testRuntime(t, 1)
	dir := t.TempDir()
	schema := map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string", "default": "a'b ; default"}}}
	code := `import os,json,sys
p=json.load(sys.stdin)
print(json.dumps({"value":p["value"],"mirrored":os.environ["TOOL_VALUE"],"cwd":os.getcwd(),"proxy":os.environ["HTTPS_PROXY"],"parent":os.getppid(),"pid":os.getpid()}))`
	raw, _ := json.Marshal(map[string]any{"code": code, "timeout_ms": 5000})
	cfg := CustomConfig{Name: "custom", Kind: "script", Exec: raw, Python: interp, WorkingDir: dir, Schema: schema}
	// The bridge uses this same existing decorator for DB scalar defaults.
	local := agent.DecorateTool(tool.Build(tool.Spec{Name: cfg.Name, Run: func(ctx context.Context, in json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
		return r.RunCustom(ctx, cfg, in, tc)
	}}), "edited description", schema)
	res, err := local.Call(context.Background(), json.RawMessage(`{}`), &tool.ToolContext{Env: []string{"HTTPS_PROXY=http://127.0.0.1:12345"}})
	if err != nil || res.IsError {
		t.Fatalf("%+v %v", res, err)
	}
	var got struct {
		Value, Mirrored, Cwd, Proxy string
		Parent, PID                 int
	}
	if err = json.Unmarshal([]byte(res.Flatten()), &got); err != nil {
		t.Fatalf("%s %v", res.Flatten(), err)
	}
	if got.Value != "a'b ; default" || got.Mirrored != got.Value || !strings.EqualFold(got.Cwd, dir) || got.Proxy != "http://127.0.0.1:12345" || got.Parent != int(pid.Load()) || got.PID == os.Getpid() {
		t.Fatalf("bad inherited custom configuration %+v", got)
	}
	assertDead(t, int(pid.Load()))
	assertDead(t, got.PID)
	entries, err := os.ReadDir(dir + "/.tools")
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary scripts leaked: %v %v", entries, err)
	}
}

func TestMCPLegacySSEProcess(t *testing.T) {
	events := make(chan mcpFrame, 32)
	var listed atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Fixture") != "secret" {
			t.Error("SSE header lost")
			w.WriteHeader(401)
			return
		}
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: endpoint\ndata: /message\n\n")
			w.(http.Flusher).Flush()
			for {
				select {
				case <-r.Context().Done():
					return
				case f := <-events:
					raw, _ := json.Marshal(f)
					fmt.Fprintf(w, "event: message\ndata: %s\n\n", raw)
					w.(http.Flusher).Flush()
				}
			}
		}
		var f mcpFrame
		if json.NewDecoder(r.Body).Decode(&f) != nil {
			w.WriteHeader(400)
			return
		}
		if f.Method == "tools/list" {
			listed.Add(1)
		}
		w.WriteHeader(202)
		if f.ID != 0 {
			fixtureReply(f, func(f mcpFrame) { events <- f })
		}
	}))
	defer srv.Close()
	r, pid := testRuntime(t, 1)
	cl := connectTest(t, r, context.Background(), MCPConfig{Name: "fixture", Transport: "sse", URL: srv.URL + "/sse", Env: map[string]string{"X-Fixture": "secret"}})
	defer cl.Close()
	probe := oneTool(t, cl)
	progress := 0
	res, err := probe.Call(context.Background(), json.RawMessage(`{}`), &tool.ToolContext{Emit: func(tool.ProgressInfo) { progress++ }})
	if err != nil || res.IsError || progress != 1 || listed.Load() != 1 {
		t.Fatalf("SSE result=%+v err=%v progress=%d list=%d", res, err, progress, listed.Load())
	}
	cl.Close()
	assertDead(t, int(pid.Load()))
}

func TestCustomCommandProcessAndProxy(t *testing.T) {
	r, pid := testRuntime(t, 1)
	dir := t.TempDir()
	schema := map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string", "default": "quoted value"}}}
	command := "printf '%s\n' {value}; printf '%s\n' \"$RESOURCE_FIXTURE_ENV\"; pwd"
	if runtime.GOOS == "windows" {
		command = "Write-Output {value}; Write-Output $env:RESOURCE_FIXTURE_ENV; (Get-Location).Path"
	}
	raw, _ := json.Marshal(map[string]any{"command": command, "timeout_ms": 5000})
	cfg := CustomConfig{Name: "command_fixture", Kind: "command", Exec: raw, WorkingDir: dir, Schema: schema}
	prepared := agent.DecorateTool(tool.Build(tool.Spec{Name: cfg.Name, Run: func(ctx context.Context, in json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
		return r.RunCustom(ctx, cfg, in, tc)
	}}), "", schema)
	res, err := prepared.Call(context.Background(), json.RawMessage(`{}`), &tool.ToolContext{Env: []string{"RESOURCE_FIXTURE_ENV=inherited-command-env"}})
	if err != nil || res.IsError || !strings.Contains(res.Flatten(), "quoted value") || !strings.Contains(res.Flatten(), "inherited-command-env") {
		t.Fatalf("command %s %v", res.Flatten(), err)
	}
	// Git Bash prints Windows temporary paths with forward slashes.
	normalized := strings.ReplaceAll(strings.ToLower(res.Flatten()), "\\", "/")
	if !strings.Contains(normalized, strings.ToLower(strings.ReplaceAll(dir, "\\", "/")[2:])) {
		t.Fatalf("command cwd missing: %s want %s", res.Flatten(), dir)
	}
	assertDead(t, int(pid.Load()))
	var used atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Host != "fixture.invalid" || req.Header.Get("X-Value") != "from-param" {
			t.Errorf("bad proxy request %s", req.URL)
		}
		used.Add(1)
		fmt.Fprint(w, "recorded-fixture")
	}))
	defer proxy.Close()
	raw, _ = json.Marshal(map[string]any{"url": "http://fixture.invalid/path", "headers": map[string]string{"X-Value": "{value}"}, "use_recording_proxy": true, "proxy": "http://127.0.0.1:1"})
	cfg = CustomConfig{Name: "http_fixture", Kind: "http", Exec: raw, RecordingProxy: strings.TrimPrefix(proxy.URL, "http://")}
	res, err = r.RunCustom(context.Background(), cfg, json.RawMessage(`{"value":"from-param"}`), nil)
	if err != nil || res.IsError || used.Load() != 1 || !strings.Contains(res.Flatten(), "recorded-fixture") {
		t.Fatalf("proxy %s %v used=%d", res.Flatten(), err, used.Load())
	}
}

func TestCustomScriptTimeout(t *testing.T) {
	interp, err := exec.LookPath("python")
	if err != nil {
		interp, err = exec.LookPath("python3")
	}
	if err != nil {
		t.Skip("Python unavailable")
	}
	r, _ := testRuntime(t, 1)
	raw, _ := json.Marshal(map[string]any{"code": "import time; time.sleep(60)", "timeout_ms": 100})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := r.RunCustom(ctx, CustomConfig{Name: "timeout", Kind: "script", Exec: raw, Python: interp, WorkingDir: t.TempDir()}, json.RawMessage(`{}`), nil)
	if err != nil || !strings.Contains(res.Flatten(), "超时终止") {
		t.Fatalf("script timeout behavior: %s %v", res.Flatten(), err)
	}
}

func TestCustomRecordingCAProcess(t *testing.T) {
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "trusted-recording-ca") }))
	defer target.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(405)
			return
		}
		upstream, err := net.Dial("tcp", r.Host)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		downstream, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		fmt.Fprint(downstream, "HTTP/1.1 200 Connection Established"+string([]byte{13, 10, 13, 10}))
		done := make(chan struct{})
		go func() { _, _ = io.Copy(upstream, downstream); upstream.Close(); close(done) }()
		_, _ = io.Copy(downstream, upstream)
		downstream.Close()
		<-done
	}))
	defer proxy.Close()
	ca := filepath.Join(t.TempDir(), "recording-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: target.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"url": target.URL, "use_recording_proxy": true, "timeout_ms": 5000})
	cfg := CustomConfig{Name: "ca", Kind: "http", Exec: raw, RecordingProxy: strings.TrimPrefix(proxy.URL, "http://"), RecordingCA: ca}
	r, _ := testRuntime(t, 1)
	res, err := r.RunCustom(context.Background(), cfg, json.RawMessage(`{}`), nil)
	if err != nil || res.IsError || !strings.Contains(res.Flatten(), "trusted-recording-ca") {
		t.Fatalf("CA lost: %s %v", res.Flatten(), err)
	}
}

func TestMCPConcurrentCallsAndLifetime(t *testing.T) {
	r, pid := testRuntime(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cl := connectTest(t, r, ctx, stdioConfig(t))
	probe := oneTool(t, cl)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			events := 0
			res, err := probe.Call(context.Background(), json.RawMessage(`{}`), &tool.ToolContext{Emit: func(tool.ProgressInfo) { events++ }})
			if err != nil || res.IsError || events != 1 {
				t.Errorf("concurrent routing: %+v %v progress=%d", res, err, events)
			}
		})
	}
	wg.Wait()
	cancel()
	assertDead(t, int(pid.Load()))
}

func TestResourceDefaultBudgets(t *testing.T) {
	r, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if r.config.Limits != (execution.Limits{MemoryBytes: 512 << 20, CPUs: 1, Processes: 128}) || r.config.MCPLimits != (execution.Limits{MemoryBytes: 384 << 20, CPUs: 1, Processes: 128}) || cap(r.slots) != 4 {
		t.Fatalf("wrong defaults: %+v", r.config)
	}
}
