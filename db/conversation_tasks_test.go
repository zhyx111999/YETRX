package db

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// 以下集成测试需要 PostgreSQL(testDSN/Open 不可用时自动 skip)。每个测试只创建并清理自己的
// 会话/任务/链接;conversation_tasks 故意没有 tasks FK,所以任务清理要显式删除链接。

func openConvTaskDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func newConvTaskConversation(t *testing.T, d *DB) *Conversation {
	t.Helper()
	conv, err := d.CreateConversation("planner", fmt.Sprintf("conv-task %s", t.Name()), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteConversation(conv.ID) })
	return conv
}

func newConvTaskTask(t *testing.T, d *DB, convID int64, label string) *Task {
	t.Helper()
	task, err := d.CreateTaskWithOptions(
		fmt.Sprintf("conv-task %s %s %d", t.Name(), label, time.Now().UnixNano()), "goal "+label,
		TaskCreateOptions{SourceConversationID: convID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = d.DeleteTask(task.ID)
		_, _ = d.Exec(`DELETE FROM conversation_tasks WHERE task_id=$1`, task.ID)
	})
	return task
}

func convTaskIntPtr(v int) *int { return &v }

type convTaskLink struct {
	ConversationID int64
	Description    string
	Goal           string
	ResultStatus   string
	ActivityID     sql.NullInt64
	DeliveredAt    sql.NullTime
}

func loadConvTaskLink(t *testing.T, d *DB, taskID int64) (convTaskLink, bool) {
	t.Helper()
	var l convTaskLink
	err := d.QueryRow(`SELECT conversation_id, description, goal, result_status, activity_id, delivered_at
FROM conversation_tasks WHERE task_id=$1`, taskID).Scan(&l.ConversationID, &l.Description, &l.Goal, &l.ResultStatus, &l.ActivityID, &l.DeliveredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return l, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return l, true
}

func countConvTaskRows(t *testing.T, d *DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := d.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// pendingConvTaskResults 返回某会话当前的待投递结果(按返回顺序),并校验全局排序键单调。
func pendingConvTaskResults(t *testing.T, d *DB, convID int64) []ConversationTaskResult {
	t.Helper()
	all, err := d.PendingConversationTaskResults(t.Context(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	out := []ConversationTaskResult{}
	for _, r := range all {
		if r.ConversationID == convID {
			out = append(out, r)
		}
	}
	return out
}

func pendingConvTaskByID(t *testing.T, d *DB, convID int64) map[int64]ConversationTaskResult {
	t.Helper()
	m := map[int64]ConversationTaskResult{}
	for _, r := range pendingConvTaskResults(t, d, convID) {
		m[r.TaskID] = r
	}
	return m
}

func hasPendingConvTasks(t *testing.T, d *DB, convID int64, receiving []int64) bool {
	t.Helper()
	got, err := d.ConversationHasPendingTasks(t.Context(), convID, receiving)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestAppendConversationTaskResultRequiresStatus(t *testing.T) {
	// 校验先于任何数据库访问:零值 DB 若走到事务会 panic。
	var d DB
	for _, status := range []string{"", "  \t"} {
		if _, err := d.AppendConversationTaskResult(t.Context(), 1, 1, status, Activity{}); err == nil {
			t.Fatalf("status %q must be rejected", status)
		}
	}
}

func TestConversationTasksSchemaConstraints(t *testing.T) {
	d := openConvTaskDB(t)
	rows, err := d.Query(`SELECT confrelid::regclass::text, confdeltype::text
FROM pg_constraint WHERE conrelid='conversation_tasks'::regclass AND contype='f'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var ref, action string
		if err := rows.Scan(&ref, &action); err != nil {
			t.Fatal(err)
		}
		got[ref] = action
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// c=CASCADE, n=SET NULL。tasks 必须不在其中,删除/归档任务后仍要能投递终态通知。
	want := map[string]string{"conversations": "c", "conversation_activities": "n"}
	if len(got) != len(want) {
		t.Fatalf("foreign keys=%v, want %v", got, want)
	}
	for ref, action := range want {
		if got[ref] != action {
			t.Fatalf("foreign keys=%v, want %v", got, want)
		}
	}
}

func TestCreateTaskWithSourceConversationLinksAtomically(t *testing.T) {
	d := openConvTaskDB(t)
	conv := newConvTaskConversation(t, d)

	task := newConvTaskTask(t, d, conv.ID, "linked")
	link, ok := loadConvTaskLink(t, d, task.ID)
	if !ok {
		t.Fatal("source conversation link was not created with the task")
	}
	if link.ConversationID != conv.ID || link.Description != task.Description || link.Goal != task.Goal {
		t.Fatalf("link snapshot=%+v, task=%+v", link, task)
	}
	if link.ResultStatus != "" || link.ActivityID.Valid || link.DeliveredAt.Valid {
		t.Fatalf("new link must have no result yet: %+v", link)
	}

	plain, err := d.CreateTaskWithOptions(fmt.Sprintf("conv-task plain %d", time.Now().UnixNano()), "goal", TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.DeleteTask(plain.ID) })
	if _, ok := loadConvTaskLink(t, d, plain.ID); ok {
		t.Fatal("task without SourceConversationID must not get a link")
	}

	deleted := newConvTaskConversation(t, d)
	if err := d.DeleteConversation(deleted.ID); err != nil {
		t.Fatal(err)
	}
	for name, convID := range map[string]int64{"missing": 1 << 62, "deleted": deleted.ID, "negative": -1} {
		t.Run(name, func(t *testing.T) {
			description := fmt.Sprintf("conv-task rollback %s %d", name, time.Now().UnixNano())
			_, err := d.CreateTaskWithOptions(description, "goal", TaskCreateOptions{SourceConversationID: convID})
			if !errors.Is(err, ErrConversationNotFound) {
				t.Fatalf("err=%v, want ErrConversationNotFound", err)
			}
			// 任务、探索图及其起点节点必须随事务一起回滚,不能先开跑后关联。
			if n := countConvTaskRows(t, d, `SELECT count(*) FROM tasks WHERE description=$1`, description); n != 0 {
				t.Fatalf("task rows left behind: %d", n)
			}
			if n := countConvTaskRows(t, d, `SELECT count(*) FROM explorations WHERE description=$1`, description); n != 0 {
				t.Fatalf("exploration rows left behind: %d", n)
			}
		})
	}
}

func TestConversationTaskResultLifecycle(t *testing.T) {
	d := openConvTaskDB(t)
	ctx := t.Context()
	conv := newConvTaskConversation(t, d)
	task := newConvTaskTask(t, d, conv.ID, "lifecycle")

	// 仍在运行的任务不返回。
	if got := pendingConvTaskResults(t, d, conv.ID); len(got) != 0 {
		t.Fatalf("running task must not be pending: %+v", got)
	}
	if err := d.SetStatus(task.ID, "done"); err != nil {
		t.Fatal(err)
	}
	got := pendingConvTaskResults(t, d, conv.ID)
	want := ConversationTaskResult{TaskID: task.ID, ConversationID: conv.ID, Description: task.Description, Goal: task.Goal, Status: "done"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("pending=%+v, want [%+v]", got, want)
	}

	activity := Activity{
		Worker: "task-result", Kind: "text", Tool: "explore", ToolUseID: "toolu_conv_task", IsError: true,
		Summary: "任务完成", Detail: "完整结果\x00正文",
		InputTokens: convTaskIntPtr(11), OutputTokens: convTaskIntPtr(22), CacheReadTokens: convTaskIntPtr(33), CacheWriteTokens: convTaskIntPtr(44),
	}
	id, err := d.AppendConversationTaskResult(ctx, task.ID, conv.ID, "done", activity)
	if err != nil || id <= 0 {
		t.Fatalf("append id=%d err=%v", id, err)
	}
	// 写入 activity 后只登记 activity_id/result_status,不得标记已投递。
	link, _ := loadConvTaskLink(t, d, task.ID)
	if !link.ActivityID.Valid || link.ActivityID.Int64 != id || link.ResultStatus != "done" || link.DeliveredAt.Valid {
		t.Fatalf("link after append=%+v", link)
	}
	var (
		convID                                                    int64
		worker, kind, tool, toolUseID, summary, detail            string
		isError                                                   bool
		inputTokens, outputTokens, cacheReadTokens, cacheWriteTok sql.NullInt64
	)
	if err := d.QueryRow(`SELECT conversation_id, worker, kind, tool, tool_use_id, is_error, summary, detail,
input_tokens, output_tokens, cache_read_tokens, cache_write_tokens FROM conversation_activities WHERE id=$1`, id).Scan(
		&convID, &worker, &kind, &tool, &toolUseID, &isError, &summary, &detail, &inputTokens, &outputTokens, &cacheReadTokens, &cacheWriteTok); err != nil {
		t.Fatal(err)
	}
	if convID != conv.ID || worker != "task-result" || kind != "text" || tool != "explore" || toolUseID != "toolu_conv_task" ||
		!isError || summary != "任务完成" || detail != "完整结果正文" ||
		inputTokens.Int64 != 11 || outputTokens.Int64 != 22 || cacheReadTokens.Int64 != 33 || cacheWriteTok.Int64 != 44 {
		t.Fatalf("activity row not fully written: conv=%d worker=%q kind=%q tool=%q tool_use=%q err=%v summary=%q detail=%q tokens=%v/%v/%v/%v",
			convID, worker, kind, tool, toolUseID, isError, summary, detail, inputTokens, outputTokens, cacheReadTokens, cacheWriteTok)
	}
	if got := pendingConvTaskResults(t, d, conv.ID); len(got) != 1 || got[0].ActivityID != id || got[0].Status != "done" {
		t.Fatalf("pending after append=%+v, want activity %d", got, id)
	}

	// 重放:返回同一 id,不产生第二条可见记录,也不改变 result_status(即使这次传了别的内容/状态)。
	replay, err := d.AppendConversationTaskResult(ctx, task.ID, conv.ID, "failed", Activity{Kind: "text", Summary: "重放"})
	if err != nil || replay != id {
		t.Fatalf("replay id=%d err=%v, want %d", replay, err, id)
	}
	if n := countConvTaskRows(t, d, `SELECT count(*) FROM conversation_activities WHERE conversation_id=$1`, conv.ID); n != 1 {
		t.Fatalf("visible records=%d, want 1", n)
	}
	if link, _ := loadConvTaskLink(t, d, task.ID); link.ResultStatus != "done" {
		t.Fatalf("replay must not rewrite result_status: %+v", link)
	}

	// 确认投递:幂等,首次时间戳保持不变。
	if err := d.CompleteConversationTaskDelivery(ctx, task.ID, conv.ID); err != nil {
		t.Fatal(err)
	}
	first, _ := loadConvTaskLink(t, d, task.ID)
	if !first.DeliveredAt.Valid {
		t.Fatalf("delivered_at not set: %+v", first)
	}
	time.Sleep(5 * time.Millisecond)
	if err := d.CompleteConversationTaskDelivery(ctx, task.ID, conv.ID); err != nil {
		t.Fatalf("second complete must be idempotent: %v", err)
	}
	if second, _ := loadConvTaskLink(t, d, task.ID); !second.DeliveredAt.Time.Equal(first.DeliveredAt.Time) {
		t.Fatalf("delivered_at moved: %v -> %v", first.DeliveredAt.Time, second.DeliveredAt.Time)
	}
	if got := pendingConvTaskResults(t, d, conv.ID); len(got) != 0 {
		t.Fatalf("delivered result must not be pending: %+v", got)
	}
	if _, err := d.AppendConversationTaskResult(ctx, task.ID, conv.ID, "done", Activity{Kind: "text"}); !errors.Is(err, ErrConversationTaskDelivered) {
		t.Fatalf("append after delivery err=%v, want ErrConversationTaskDelivered", err)
	}
	if n := countConvTaskRows(t, d, `SELECT count(*) FROM conversation_activities WHERE conversation_id=$1`, conv.ID); n != 1 {
		t.Fatalf("visible records after delivered append=%d, want 1", n)
	}
}

func TestConversationTaskResultRejectsWrongOwnership(t *testing.T) {
	d := openConvTaskDB(t)
	ctx := t.Context()
	owner := newConvTaskConversation(t, d)
	other := newConvTaskConversation(t, d)
	task := newConvTaskTask(t, d, owner.ID, "owned")
	if err := d.SetStatus(task.ID, "done"); err != nil {
		t.Fatal(err)
	}

	for name, call := range map[string]func() error{
		"append wrong conversation": func() error {
			_, err := d.AppendConversationTaskResult(ctx, task.ID, other.ID, "done", Activity{Kind: "text"})
			return err
		},
		"append missing task": func() error {
			_, err := d.AppendConversationTaskResult(ctx, 1<<62, owner.ID, "done", Activity{Kind: "text"})
			return err
		},
		"append missing conversation": func() error {
			_, err := d.AppendConversationTaskResult(ctx, task.ID, 1<<62, "done", Activity{Kind: "text"})
			return err
		},
		"complete wrong conversation": func() error { return d.CompleteConversationTaskDelivery(ctx, task.ID, other.ID) },
		"complete missing task":       func() error { return d.CompleteConversationTaskDelivery(ctx, 1<<62, owner.ID) },
	} {
		if err := call(); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("%s: err=%v, want sql.ErrNoRows", name, err)
		}
	}
	for _, convID := range []int64{owner.ID, other.ID} {
		if n := countConvTaskRows(t, d, `SELECT count(*) FROM conversation_activities WHERE conversation_id=$1`, convID); n != 0 {
			t.Fatalf("rejected append left %d activities in conversation %d", n, convID)
		}
	}
	if link, _ := loadConvTaskLink(t, d, task.ID); link.ActivityID.Valid || link.ResultStatus != "" || link.DeliveredAt.Valid {
		t.Fatalf("rejected calls modified the link: %+v", link)
	}

	// 先投递一个任务,不得影响同一会话另一个/别的会话的链接。
	sibling := newConvTaskTask(t, d, owner.ID, "sibling")
	foreign := newConvTaskTask(t, d, other.ID, "foreign")
	for _, tk := range []*Task{sibling, foreign} {
		if err := d.SetStatus(tk.ID, "done"); err != nil {
			t.Fatal(err)
		}
	}
	foreignActivity, err := d.AppendConversationTaskResult(ctx, foreign.ID, other.ID, "done", Activity{Kind: "text"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.AppendConversationTaskResult(ctx, sibling.ID, owner.ID, "done", Activity{Kind: "text"}); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteConversationTaskDelivery(ctx, sibling.ID, owner.ID); err != nil {
		t.Fatal(err)
	}
	if l, _ := loadConvTaskLink(t, d, foreign.ID); l.DeliveredAt.Valid || l.ActivityID.Int64 != foreignActivity {
		t.Fatalf("delivery of a sibling touched another conversation's link: %+v", l)
	}
	if l, _ := loadConvTaskLink(t, d, task.ID); l.DeliveredAt.Valid {
		t.Fatalf("delivery of a sibling touched the owner's other link: %+v", l)
	}
}

func TestCompleteConversationTaskDeliveryRequiresRecordedResult(t *testing.T) {
	d := openConvTaskDB(t)
	conv := newConvTaskConversation(t, d)
	task := newConvTaskTask(t, d, conv.ID, "unrecorded")
	if err := d.SetStatus(task.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteConversationTaskDelivery(t.Context(), task.ID, conv.ID); !errors.Is(err, ErrConversationTaskResultNotRecorded) {
		t.Fatalf("err=%v, want ErrConversationTaskResultNotRecorded", err)
	}
	if l, _ := loadConvTaskLink(t, d, task.ID); l.DeliveredAt.Valid {
		t.Fatalf("delivery without a recorded result: %+v", l)
	}
	if got := pendingConvTaskResults(t, d, conv.ID); len(got) != 1 {
		t.Fatalf("result must stay pending: %+v", got)
	}
}

func TestAppendConversationTaskResultRecreatesDeletedActivity(t *testing.T) {
	d := openConvTaskDB(t)
	ctx := t.Context()
	conv := newConvTaskConversation(t, d)
	task := newConvTaskTask(t, d, conv.ID, "activity-removed")
	first, err := d.AppendConversationTaskResult(ctx, task.ID, conv.ID, "failed", Activity{Kind: "text", Summary: "first"})
	if err != nil {
		t.Fatal(err)
	}
	// 记录被外部删除后 activity_id 置空(ON DELETE SET NULL);重放必须能补写,而不是返回悬空 id。
	if _, err := d.Exec(`DELETE FROM conversation_activities WHERE id=$1`, first); err != nil {
		t.Fatal(err)
	}
	if l, _ := loadConvTaskLink(t, d, task.ID); l.ActivityID.Valid || l.ResultStatus != "failed" {
		t.Fatalf("link after activity delete=%+v", l)
	}
	second, err := d.AppendConversationTaskResult(ctx, task.ID, conv.ID, "failed", Activity{Kind: "text", Summary: "second"})
	if err != nil || second == first {
		t.Fatalf("second id=%d err=%v (first %d)", second, err, first)
	}
	if n := countConvTaskRows(t, d, `SELECT count(*) FROM conversation_activities WHERE conversation_id=$1`, conv.ID); n != 1 {
		t.Fatalf("visible records=%d, want 1", n)
	}
}

func TestAppendConversationTaskResultConcurrentReplayWritesOnce(t *testing.T) {
	d := openConvTaskDB(t)
	conv := newConvTaskConversation(t, d)
	task := newConvTaskTask(t, d, conv.ID, "concurrent")
	if err := d.SetStatus(task.ID, "done"); err != nil {
		t.Fatal(err)
	}
	const workers = 8
	ids := make([]int64, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ids[i], errs[i] = d.AppendConversationTaskResult(t.Context(), task.ID, conv.ID, "done", Activity{Kind: "text", Summary: fmt.Sprintf("writer %d", i)})
		}()
	}
	close(start)
	wg.Wait()
	for i := range workers {
		if errs[i] != nil || ids[i] <= 0 || ids[i] != ids[0] {
			t.Fatalf("worker %d id=%d err=%v (worker 0 id=%d)", i, ids[i], errs[i], ids[0])
		}
	}
	if n := countConvTaskRows(t, d, `SELECT count(*) FROM conversation_activities WHERE conversation_id=$1`, conv.ID); n != 1 {
		t.Fatalf("visible records=%d, want 1", n)
	}
}

func TestAppendConversationTaskResultRacesConversationDeleteWithoutDeadlock(t *testing.T) {
	d := openConvTaskDB(t)
	for i := range 15 {
		conv := newConvTaskConversation(t, d)
		task := newConvTaskTask(t, d, conv.ID, fmt.Sprintf("race-%d", i))
		var wg sync.WaitGroup
		var appendErr, deleteErr error
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, appendErr = d.AppendConversationTaskResult(t.Context(), task.ID, conv.ID, "done", Activity{Kind: "text"})
		}()
		go func() {
			defer wg.Done()
			<-start
			deleteErr = d.DeleteConversation(conv.ID)
		}()
		close(start)
		wg.Wait()
		// Append 要么在删除前完成(随后被级联清掉),要么观察到会话已删;死锁(40P01)等其它错误都不允许。
		if deleteErr != nil || (appendErr != nil && !errors.Is(appendErr, sql.ErrNoRows)) {
			t.Fatalf("iteration %d: append err=%v delete err=%v", i, appendErr, deleteErr)
		}
		if _, ok := loadConvTaskLink(t, d, task.ID); ok {
			t.Fatalf("iteration %d: link survived conversation delete", i)
		}
		if n := countConvTaskRows(t, d, `SELECT count(*) FROM conversation_activities WHERE conversation_id=$1`, conv.ID); n != 0 {
			t.Fatalf("iteration %d: %d activities survived conversation delete", i, n)
		}
	}
}

func TestDeletedConversationDoesNotResurrectTaskResults(t *testing.T) {
	d := openConvTaskDB(t)
	ctx := t.Context()
	conv := newConvTaskConversation(t, d)
	task := newConvTaskTask(t, d, conv.ID, "orphaned")
	if err := d.DeleteConversation(conv.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadConvTaskLink(t, d, task.ID); ok {
		t.Fatal("link must cascade with its conversation")
	}
	// 任务稍后才完成:也不会再作为待投递结果出现,更不能向已删会话写入。
	if err := d.SetStatus(task.ID, "done"); err != nil {
		t.Fatal(err)
	}
	if got := pendingConvTaskResults(t, d, conv.ID); len(got) != 0 {
		t.Fatalf("deleted conversation still has pending results: %+v", got)
	}
	if _, err := d.AppendConversationTaskResult(ctx, task.ID, conv.ID, "done", Activity{Kind: "text"}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("append to deleted conversation err=%v, want sql.ErrNoRows", err)
	}
	if err := d.CompleteConversationTaskDelivery(ctx, task.ID, conv.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("complete on deleted conversation err=%v, want sql.ErrNoRows", err)
	}
	if hasPendingConvTasks(t, d, conv.ID, nil) {
		t.Fatal("deleted conversation must not report pending tasks")
	}
	if n := countConvTaskRows(t, d, `SELECT count(*) FROM conversation_activities WHERE conversation_id=$1`, conv.ID); n != 0 {
		t.Fatalf("activities written for deleted conversation: %d", n)
	}
}

func TestConversationTaskTombstonesAreDeliverable(t *testing.T) {
	d := openConvTaskDB(t)
	ctx := t.Context()
	conv := newConvTaskConversation(t, d)

	hardDeleted := newConvTaskTask(t, d, conv.ID, "hard-deleted")
	softDeleted := newConvTaskTask(t, d, conv.ID, "soft-deleted")
	archived := newConvTaskTask(t, d, conv.ID, "archived")
	recorded := newConvTaskTask(t, d, conv.ID, "recorded-then-deleted")
	running := newConvTaskTask(t, d, conv.ID, "still-running")
	for _, tk := range []*Task{hardDeleted, softDeleted, archived, recorded, running} {
		if err := d.SetStatus(tk.ID, "running"); err != nil {
			t.Fatal(err)
		}
	}
	// 与 db/task_archives_restore.go 一致:归档墓碑同时有 archived_at/deleted_at,且清空 description/goal。
	if _, err := d.Exec(`UPDATE tasks SET archived_at=now(), deleted_at=now(), description='', goal='' WHERE id=$1`, archived.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE tasks SET deleted_at=now() WHERE id=$1`, softDeleted.ID); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteTask(hardDeleted.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AppendConversationTaskResult(ctx, recorded.ID, conv.ID, "timeout", Activity{Kind: "text"}); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteTask(recorded.ID); err != nil {
		t.Fatal(err)
	}

	got := pendingConvTaskByID(t, d, conv.ID)
	if len(got) != 4 {
		t.Fatalf("pending=%+v, want the 4 tombstoned tasks only", got)
	}
	for taskID, want := range map[int64]string{
		hardDeleted.ID: ConversationTaskStatusDeleted,
		softDeleted.ID: ConversationTaskStatusDeleted,
		archived.ID:    ConversationTaskStatusArchived,
		recorded.ID:    "timeout", // 已记录的结果状态优先于任务现状
	} {
		if got[taskID].Status != want {
			t.Fatalf("task %d status=%q, want %q (all=%+v)", taskID, got[taskID].Status, want, got)
		}
	}
	// 创建快照保留,即使 tasks 行已被删除/清空。
	for _, tk := range []*Task{hardDeleted, softDeleted, archived} {
		if got[tk.ID].Description != tk.Description || got[tk.ID].Goal != tk.Goal {
			t.Fatalf("snapshot lost for task %d: %+v", tk.ID, got[tk.ID])
		}
	}
	if _, ok := got[running.ID]; ok {
		t.Fatal("running task must not be pending")
	}
	// 墓碑任务投递闭环:写入 → 确认后不再待投递。
	for _, tk := range []*Task{hardDeleted, archived} {
		if _, err := d.AppendConversationTaskResult(ctx, tk.ID, conv.ID, got[tk.ID].Status, Activity{Kind: "text"}); err != nil {
			t.Fatal(err)
		}
		if err := d.CompleteConversationTaskDelivery(ctx, tk.ID, conv.ID); err != nil {
			t.Fatal(err)
		}
	}
	if left := pendingConvTaskByID(t, d, conv.ID); len(left) != 2 {
		t.Fatalf("after delivering two tombstones pending=%+v", left)
	}
}

func TestPendingConversationTaskResultsFiltersOrdersAndLimits(t *testing.T) {
	d := openConvTaskDB(t)
	conv := newConvTaskConversation(t, d)
	other := newConvTaskConversation(t, d)

	for _, status := range []string{"created", "running", "paused"} {
		tk := newConvTaskTask(t, d, conv.ID, status)
		if err := d.SetStatus(tk.ID, status); err != nil {
			t.Fatal(err)
		}
	}
	queued := newConvTaskTask(t, d, conv.ID, "queued")
	if err := d.Enqueue(queued.ID, "bootstrap"); err != nil {
		t.Fatal(err)
	}

	readyStatuses := []string{"done", "failed", "timeout"}
	var ready []*Task
	for _, status := range readyStatuses {
		tk := newConvTaskTask(t, d, conv.ID, status)
		if err := d.SetStatus(tk.ID, status); err != nil {
			t.Fatal(err)
		}
		ready = append(ready, tk)
	}
	foreign := newConvTaskTask(t, d, other.ID, "foreign")
	if err := d.SetStatus(foreign.ID, "done"); err != nil {
		t.Fatal(err)
	}

	got := pendingConvTaskResults(t, d, conv.ID)
	if len(got) != len(ready) {
		t.Fatalf("pending=%+v, want only the %d terminal tasks", got, len(ready))
	}
	for i, tk := range ready {
		if got[i].TaskID != tk.ID || got[i].Status != readyStatuses[i] {
			t.Fatalf("pending[%d]=%+v, want task %d (%s) in creation order", i, got[i], tk.ID, readyStatuses[i])
		}
	}

	// 全局排序稳定,limit 取其前缀;limit<=0 回落默认值而非返回空。
	all, err := d.PendingConversationTaskResults(t.Context(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(all); i++ {
		if all[i].TaskID == all[i-1].TaskID {
			t.Fatalf("duplicate task in pending list: %+v", all[i])
		}
	}
	limited, err := d.PendingConversationTaskResults(t.Context(), 2)
	if err != nil || len(limited) != 2 || limited[0] != all[0] || limited[1] != all[1] {
		t.Fatalf("limit=2 -> %+v err=%v, want prefix of %+v", limited, err, all)
	}
	excluded, err := d.PendingConversationTaskResults(t.Context(), 1, all[0].TaskID, all[1].TaskID)
	if err != nil || len(excluded) != 1 || excluded[0] != all[2] {
		t.Fatalf("exclusion must apply before limit: %+v err=%v", excluded, err)
	}
	if def, err := d.PendingConversationTaskResults(t.Context(), 0); err != nil || len(def) == 0 {
		t.Fatalf("limit=0 -> %+v err=%v, want default limit", def, err)
	}
}

func TestConversationHasPendingTasks(t *testing.T) {
	d := openConvTaskDB(t)
	ctx := t.Context()

	t.Run("no links", func(t *testing.T) {
		conv := newConvTaskConversation(t, d)
		if hasPendingConvTasks(t, d, conv.ID, nil) || hasPendingConvTasks(t, d, conv.ID, []int64{}) {
			t.Fatal("conversation without links must not be pending")
		}
	})

	t.Run("non-terminal tasks keep pending even when receiving", func(t *testing.T) {
		conv := newConvTaskConversation(t, d)
		task := newConvTaskTask(t, d, conv.ID, "live")
		steps := map[string]func() error{
			"created": func() error { return nil },
			"paused":  func() error { return d.SetPaused(task.ID, true) },
			"queued":  func() error { return d.Enqueue(task.ID, "resume") },
			"running": func() error { return d.SetStatus(task.ID, "running") },
		}
		for _, name := range []string{"created", "paused", "queued", "running"} {
			if err := steps[name](); err != nil {
				t.Fatal(err)
			}
			if !hasPendingConvTasks(t, d, conv.ID, nil) || !hasPendingConvTasks(t, d, conv.ID, []int64{task.ID}) {
				t.Fatalf("%s task must stay pending (also when listed as receiving)", name)
			}
		}
	})

	t.Run("receiving list excludes only finished undelivered results", func(t *testing.T) {
		conv := newConvTaskConversation(t, d)
		done := newConvTaskTask(t, d, conv.ID, "done")
		if err := d.SetStatus(done.ID, "done"); err != nil {
			t.Fatal(err)
		}
		if !hasPendingConvTasks(t, d, conv.ID, nil) || !hasPendingConvTasks(t, d, conv.ID, []int64{done.ID + 1}) {
			t.Fatal("finished undelivered result outside the receiving list must be pending")
		}
		if hasPendingConvTasks(t, d, conv.ID, []int64{done.ID}) {
			t.Fatal("result being received must be excluded")
		}
		// 已写入结果记录但未 ack,仍属于「正在接收」。
		if _, err := d.AppendConversationTaskResult(ctx, done.ID, conv.ID, "done", Activity{Kind: "text"}); err != nil {
			t.Fatal(err)
		}
		if hasPendingConvTasks(t, d, conv.ID, []int64{done.ID}) || !hasPendingConvTasks(t, d, conv.ID, nil) {
			t.Fatal("recorded-but-unacked result must be pending unless it is being received")
		}
		// 另一个仍在跑的任务不能被 receiving 列表遮蔽;完成后又需要投递。
		live := newConvTaskTask(t, d, conv.ID, "live")
		if err := d.SetStatus(live.ID, "running"); err != nil {
			t.Fatal(err)
		}
		if !hasPendingConvTasks(t, d, conv.ID, []int64{done.ID}) {
			t.Fatal("a running sibling must keep the conversation pending")
		}
		if err := d.SetStatus(live.ID, "failed"); err != nil {
			t.Fatal(err)
		}
		if !hasPendingConvTasks(t, d, conv.ID, []int64{done.ID}) {
			t.Fatal("a finished sibling outside the receiving list must be pending")
		}
		if hasPendingConvTasks(t, d, conv.ID, []int64{done.ID, live.ID}) {
			t.Fatal("all results being received and none running must not be pending")
		}
	})

	t.Run("receiving result whose task was reopened is pending again", func(t *testing.T) {
		conv := newConvTaskConversation(t, d)
		task := newConvTaskTask(t, d, conv.ID, "reopened-while-receiving")
		if err := d.SetStatus(task.ID, "done"); err != nil {
			t.Fatal(err)
		}
		if hasPendingConvTasks(t, d, conv.ID, []int64{task.ID}) {
			t.Fatal("precondition: finished result being received is excluded")
		}
		if err := d.SetStatus(task.ID, "running"); err != nil {
			t.Fatal(err)
		}
		if !hasPendingConvTasks(t, d, conv.ID, []int64{task.ID}) {
			t.Fatal("a reopened task must be pending even though it is in the receiving list")
		}
	})

	t.Run("delivered links never block, even when the task is reopened", func(t *testing.T) {
		conv := newConvTaskConversation(t, d)
		task := newConvTaskTask(t, d, conv.ID, "delivered")
		if err := d.SetStatus(task.ID, "done"); err != nil {
			t.Fatal(err)
		}
		if _, err := d.AppendConversationTaskResult(ctx, task.ID, conv.ID, "done", Activity{Kind: "text"}); err != nil {
			t.Fatal(err)
		}
		if err := d.CompleteConversationTaskDelivery(ctx, task.ID, conv.ID); err != nil {
			t.Fatal(err)
		}
		if hasPendingConvTasks(t, d, conv.ID, nil) {
			t.Fatal("finished and delivered task must not be pending")
		}
		for _, status := range []string{"running", "paused"} {
			if err := d.SetStatus(task.ID, status); err != nil {
				t.Fatal(err)
			}
			if hasPendingConvTasks(t, d, conv.ID, nil) || hasPendingConvTasks(t, d, conv.ID, []int64{task.ID}) {
				t.Fatalf("reopened (%s) delivered task must not block the old conversation", status)
			}
		}
	})

	t.Run("deleted and archived tasks do not wait forever", func(t *testing.T) {
		conv := newConvTaskConversation(t, d)
		hard := newConvTaskTask(t, d, conv.ID, "hard")
		soft := newConvTaskTask(t, d, conv.ID, "soft")
		archived := newConvTaskTask(t, d, conv.ID, "archived")
		if _, err := d.Exec(`UPDATE tasks SET paused=true, deleted_at=now() WHERE id=$1`, soft.ID); err != nil {
			t.Fatal(err)
		}
		// 归档要求任务已暂停/终态;状态仍是非终态也属于「不再运行」。
		if _, err := d.Exec(`UPDATE tasks SET paused=true, archived_at=now(), deleted_at=now() WHERE id=$1`, archived.ID); err != nil {
			t.Fatal(err)
		}
		if err := d.DeleteTask(hard.ID); err != nil {
			t.Fatal(err)
		}
		ids := []int64{hard.ID, soft.ID, archived.ID}
		if !hasPendingConvTasks(t, d, conv.ID, nil) {
			t.Fatal("undelivered tombstone results outside the receiving list must be pending")
		}
		if hasPendingConvTasks(t, d, conv.ID, ids) {
			t.Fatal("tombstoned tasks being received must not keep the conversation pending")
		}
		for _, id := range ids {
			if _, err := d.AppendConversationTaskResult(ctx, id, conv.ID, "deleted", Activity{Kind: "text"}); err != nil {
				t.Fatal(err)
			}
			if err := d.CompleteConversationTaskDelivery(ctx, id, conv.ID); err != nil {
				t.Fatal(err)
			}
		}
		if hasPendingConvTasks(t, d, conv.ID, nil) {
			t.Fatal("delivered tombstone results must not be pending")
		}
	})

	t.Run("other conversations are independent", func(t *testing.T) {
		quiet := newConvTaskConversation(t, d)
		busy := newConvTaskConversation(t, d)
		task := newConvTaskTask(t, d, busy.ID, "busy")
		if err := d.SetStatus(task.ID, "running"); err != nil {
			t.Fatal(err)
		}
		if hasPendingConvTasks(t, d, quiet.ID, nil) {
			t.Fatal("pending task of another conversation leaked")
		}
		if !hasPendingConvTasks(t, d, busy.ID, nil) {
			t.Fatal("busy conversation must be pending")
		}
	})
}
