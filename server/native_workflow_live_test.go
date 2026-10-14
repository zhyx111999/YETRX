package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/artex/workflow"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

// This opt-in test requires a disposable database and a private Config JSON
// outside the repository. Its provider budget includes parent/child requests.
// The single injected transport failure tests retry without stressing a live API.
type liveWorkflowProbe struct {
	inner    llm.Provider
	calls    atomic.Int32
	injected atomic.Bool
}

func (p *liveWorkflowProbe) before(ctx context.Context, req llm.CompletionRequest) error {
	if p.calls.Add(1) > 12 {
		return workflow.Permanent(errors.New("live validation request budget exhausted"))
	}
	for _, tool := range req.Tools {
		if strings.HasPrefix(agent.RunInfoFrom(ctx).SessionID, "workflow-") && (tool.Name == "subagents" || tool.Name == "subagents_control") {
			return workflow.Permanent(errors.New("child received forbidden orchestration tool"))
		}
	}
	if p.injected.CompareAndSwap(false, true) {
		return errors.New("injected recoverable validation transport failure")
	}
	return nil
}
func (p *liveWorkflowProbe) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	if err := p.before(ctx, req); err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	return p.inner.Complete(ctx, req)
}
func (p *liveWorkflowProbe) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		if err := p.before(ctx, req); err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		for event, err := range p.inner.Stream(ctx, req) {
			if !yield(event, err) {
				return
			}
		}
	}
}

func TestNativeWorkflowLiveParity(t *testing.T) {
	path := os.Getenv("YETRX_PARITY_LIVE_PROFILE")
	if path == "" {
		t.Skip("explicit private live profile required")
	}
	dsn, _, err := db.DSN()
	if err != nil {
		t.Fatal(err)
	}
	// Verify the destination without running any schema migration on it first.
	preflight, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal("cannot connect to disposable database")
	}
	var databaseName string
	err = preflight.QueryRow("SELECT current_database()").Scan(&databaseName)
	_ = preflight.Close()
	if err != nil || !strings.HasPrefix(databaseName, "yetrx_parity_") {
		t.Fatal("live validation requires an explicitly named disposable database")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg agent.Config
	if err = json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal("invalid private profile")
	}
	if cfg.Model != "gpt-6-astra" {
		t.Fatal("live validation requires the selected Astra model")
	}
	provider, err := cfg.NewProvider()
	if err != nil {
		t.Fatal("cannot construct live provider")
	}
	e := newNativeEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	root := t.TempDir()
	probe := &liveWorkflowProbe{inner: llmrec.Wrap(provider, e.pg, cfg.Model, "Astra parity validation", cfg.ThinkingType, cfg.ReasoningEffort, func() bool { return true })}
	ca := agent.NewChatAgent(probe, cfg.Model, root, transcript.NewStore(filepath.Join(root, "transcripts")), cfg.CompactionWindow())
	ca.SetNonStreaming(func() bool { return !cfg.Stream })
	ca.SetMaxTokens(func() int { return 4000 })
	s := &Server{ctx: ctx, m: &Manager{pg: e.pg, dir: root}, skillDir: e.skillDir, chatAgent: ca,
		chatBusy: map[string]bool{}, chatCancel: map[string]context.CancelCauseFunc{}, workflowPending: map[string][]string{}, workflowMuted: map[string]bool{}}
	conv, err := e.pg.CreateConversation("agent", "isolated Astra parity validation", nil)
	if err != nil {
		t.Fatal(err)
	}
	session := s.convBusyKey(conv.ID)
	s.chatBusy[session] = true
	s.initWorkflows()
	wireAgentAugment(e.pg, e.skillDir, s.hostTools)
	if s.workflows == nil {
		t.Fatal("manager unavailable")
	}
	defer func() { cancel(); s.workflows.Wait(); _ = e.pg.DeleteConversation(conv.ID) }()
	repo := filepath.Join(root, "project")
	if err = os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git validation: %v %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init")
	if err = os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("ORIGINAL"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "seed.txt")
	git("-c", "user.name=YETRX test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "commit", "-m", "fixture")
	request := func(body any) string {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	caller := agent.WithRunInfo(ctx, agent.RunInfo{SessionID: session})
	task := workflow.Task{Label: "editor", Task: "受限验收任务，最多 4 次模型请求。只操作当前工作树 seed.txt。第一步必须调用 Bash 执行 sleep 2，给主代理留出补充指令时间。工具完成后严格遵守最新补充指令修改 seed.txt；最终只输出 ASTRA_PARITY_DONE 并给出当前目录。不要联网，不查询其它源码，不调用子代理，不提交不合并。", Isolation: "worktree"}
	started, err := s.startWorkflow(caller, json.RawMessage(request(map[string]any{"cwd": repo, "agent_retries": 1, "agent_timeout_ms": 120000, "tasks": []workflow.Task{task}})))
	if err != nil || started.IsError {
		t.Fatal("start failed", err, started.Flatten())
	}
	var start struct {
		RunID string `json:"run_id"`
	}
	if err = json.Unmarshal([]byte(started.Flatten()), &start); err != nil {
		t.Fatal(err)
	}
	sent := false
	for !sent && ctx.Err() == nil {
		rows, _, err := e.pg.ConvActivityList(conv.ID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.Worker == "subagent:editor" && row.Kind == "tool_use" && row.Tool == "Bash" {
				result, err := s.controlWorkflow(caller, json.RawMessage(request(map[string]any{"action": "message", "run_id": start.RunID, "label": "editor", "text": "补充指令 ASTRA_STEER_RECEIVED：现在将 seed.txt 的全部内容改为 STEERED_BY_PARENT，最终回复必须包含 ASTRA_PARITY_DONE 和 ASTRA_STEER_RECEIVED。"})))
				if err != nil || result.IsError {
					t.Fatal("message failed", err, result.Flatten())
				}
				sent = true
				break
			}
		}
		if !sent {
			r, _ := s.workflows.Status(session, start.RunID)
			if r.Status != workflow.Running {
				t.Fatalf("task ended before steering: %+v", r.Tasks)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitRun := func(id string) workflow.Run {
		t.Helper()
		for ctx.Err() == nil {
			r, err := s.workflows.Status(session, id)
			if err != nil {
				t.Fatal(err)
			}
			if r.Status != workflow.Running {
				return r
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("live validation timed out")
		return workflow.Run{}
	}
	run := waitRun(start.RunID)
	if run.Status != workflow.Succeeded || run.Tasks[0].Attempts != 2 || !strings.Contains(run.Tasks[0].Result, "ASTRA_STEER_RECEIVED") {
		t.Fatalf("live task failed: %+v", run.Tasks)
	}
	w := run.Tasks[0].Workspace
	if w == nil || !w.Isolated || w.Removed {
		t.Fatal("worktree not retained")
	}
	content, err := os.ReadFile(filepath.Join(w.CWD, "seed.txt"))
	if err != nil || strings.TrimSpace(string(content)) != "STEERED_BY_PARENT" {
		t.Fatal("steering did not reach file edit", err)
	}
	original, _ := os.ReadFile(filepath.Join(repo, "seed.txt"))
	if string(original) != "ORIGINAL" {
		t.Fatal("parent source changed")
	}
	tx := transcript.NewStore(filepath.Join(root, "transcripts"))
	child, _ := os.ReadFile(tx.MainPath(run.Tasks[0].SessionID))
	if !strings.Contains(string(child), "ASTRA_STEER_RECEIVED") {
		t.Fatal("instruction absent from child history")
	}
	calls := probe.calls.Load()
	resumed, err := s.controlWorkflow(caller, json.RawMessage(request(map[string]any{"action": "resume", "run_id": run.ID, "tasks": []workflow.Task{{Label: "review", Checkpoint: &workflow.CheckpointInput{ID: "live-review", Kind: "review", Payload: json.RawMessage(`{"ready":true}`)}, DependsOn: []string{"editor"}}, task}})))
	if err != nil || resumed.IsError {
		t.Fatal("edited resume failed", err, resumed.Flatten())
	}
	paused := waitRun(run.ID)
	if paused.Status != workflow.Paused || paused.Checkpoint == nil || probe.calls.Load() != calls {
		t.Fatal("reordered task was not reused or checkpoint did not pause")
	}
	resumed, err = s.controlWorkflow(caller, json.RawMessage(request(map[string]any{"action": "resume", "run_id": run.ID, "checkpoint_id": "live-review", "response": nil})))
	if err != nil || resumed.IsError || waitRun(run.ID).Status != workflow.Succeeded {
		t.Fatal("checkpoint response failed", err, resumed.Flatten())
	}
	denied, err := s.startWorkflow(context.WithValue(caller, workflowCallerKey{}, workflowCaller{Session: session, Agent: "agent", Depth: 1}), json.RawMessage(`{"tasks":[{"label":"forbidden","task":"do not run"}]}`))
	if err != nil || !denied.IsError {
		t.Fatal("nested dispatch was not rejected")
	}
	var wireCalls int
	if err = e.pg.QueryRow(`SELECT count(*) FROM llm_records WHERE model=$1 AND raw_request::jsonb->>'model'=$1 AND session_id=$2`, cfg.Model, run.Tasks[0].SessionID).Scan(&wireCalls); err != nil {
		t.Fatal(err)
	}
	if wireCalls < 2 {
		t.Fatal("actual Astra HTTP requests were not recorded")
	}
	for _, tool := range s.chatFileTools() {
		schema, _ := json.Marshal(tool.InputSchema())
		if err := e.pg.SeedTool(tool.Name(), tool.Description(), schema, json.RawMessage(`["agent"]`)); err != nil {
			t.Fatal(err)
		}
	}
	answer, err := ca.Chat(caller, "agent", session, "受限附件验收，最多 4 次模型请求。先调用 Write 创建当前目录的 delivery.txt，全部内容为 ASTRA_FILE_DELIVERY_OK；再调用 send_file 发送这个文件，name 为 验收附件.txt，caption 为 文件已生成。不要派发任务，不联网。发送成功后只回复 ASTRA_FILE_SENT。", 4, time.Minute, false, nil)
	if err != nil || !strings.Contains(answer, "ASTRA_FILE_SENT") {
		t.Fatal("live file tool failed", err, answer)
	}
	rows, _, err := e.pg.ConvActivityList(conv.ID, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	var delivered *chatFile
	for _, row := range rows {
		if row.Kind != "file" {
			continue
		}
		detail, err := e.pg.ConvActivityDetail(conv.ID, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		var message chatFileMessage
		if err := json.Unmarshal([]byte(detail), &message); err != nil {
			t.Fatal(err)
		}
		if len(message.Attachments) == 1 {
			delivered = &message.Attachments[0]
		}
	}
	if delivered == nil || delivered.Name != "验收附件.txt" {
		t.Fatal("real model did not emit the file attachment")
	}
	bytes, err := os.ReadFile(filepath.Join(s.chatFileDir(conv.ID), delivered.ID, "content"))
	if err != nil || strings.TrimSpace(string(bytes)) != "ASTRA_FILE_DELIVERY_OK" {
		t.Fatal("live delivered file contents mismatch", err)
	}
	git("worktree", "remove", "--force", w.CWD)
	git("branch", "-D", w.Branch)
	t.Logf("ASTRA_PARITY_PASS run=%s attempts=2 model=%s provider_calls=%d recorded_HTTP_calls=%d steering=delivered reuse=verified checkpoint=consumed parent_source=unchanged nesting=denied file=sent", run.ID, cfg.Model, probe.calls.Load(), wireCalls)
}
