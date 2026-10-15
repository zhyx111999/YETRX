package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// 会话发起的探索任务与来源会话的持久关联(表 conversation_tasks,见 schema.sql §I)。
//
// 生命周期:
//  1. CreateTaskWithOptions 在创建任务的同一事务内写入链接(SourceConversationID>0)。
//  2. 任务进入终态/被删除/被归档后,PendingConversationTaskResults 把它作为「待投递结果」返回。
//  3. AppendConversationTaskResult 把结果写成会话可见记录(幂等,每个任务至多一条)。
//  4. 主模型处理完结果后调用 CompleteConversationTaskDelivery 确认,链接才算已投递。
//
// 链接只跟踪首次交付:一旦 delivered_at 置位,即使任务之后被用户重开,也不再阻塞或重复投递。

// 链接上除任务真实状态外的两个派生状态。tasks 行被物理删除,或带 deleted_at/archived_at
// 墓碑时任务不再运行,此时结果状态取这两个值(归档优先,便于提示「来源已归档」)。
const (
	ConversationTaskStatusDeleted  = "deleted"
	ConversationTaskStatusArchived = "archived"
)

const defaultConversationTaskResultLimit = 50

var (
	// ErrConversationNotFound 表示创建任务时指定的来源会话不存在(或已被删除)。
	ErrConversationNotFound = errors.New("conversation not found")
	// ErrConversationTaskDelivered 表示该任务结果已确认投递,不再接受写入。
	ErrConversationTaskDelivered = errors.New("conversation task result already delivered")
	// ErrConversationTaskResultNotRecorded 表示尚未写入结果记录,不能确认投递。
	ErrConversationTaskResultNotRecorded = errors.New("conversation task result not recorded")
	errConversationTaskStatusRequired    = errors.New("conversation task result status is required")
)

// conversationTaskLiveSQL 为真表示任务仍可能产出结果:tasks 行存在、没有墓碑、状态未终态。
// 其否定(行缺失/已删除/已归档/已终态)即「不再运行」。要求 tasks 以别名 t 左连接。
const conversationTaskLiveSQL = `(t.id IS NOT NULL AND t.deleted_at IS NULL AND t.archived_at IS NULL
 AND t.status NOT IN ('done','failed','timeout'))`

// ConversationTaskResult 是一条待投递给来源会话的任务结果。
// Status 为 done/failed/timeout,或 archived/deleted(任务已不存在于任务列表);
// 已写入结果记录的链接以记录时的状态为准。ActivityID 为 0 表示结果记录尚未写入。
type ConversationTaskResult struct {
	TaskID         int64
	ConversationID int64
	Description    string
	Goal           string
	Status         string
	ActivityID     int64
}

// PendingConversationTaskResults 返回尚未确认投递且已可投递的结果,按 created_at、task_id
// 稳定排序,最多 limit 条(limit<=0 取默认 50)。已有 result_status 的链接直接可投递;
// 其余仅当任务已终态、已归档或已删除(含 tasks 行缺失)时才返回,仍在运行的不返回。
func (d *DB) PendingConversationTaskResults(ctx context.Context, limit int, excludedTaskIDs ...int64) ([]ConversationTaskResult, error) {
	if limit <= 0 {
		limit = defaultConversationTaskResultLimit
	}
	rows, err := d.QueryContext(ctx, `
SELECT ct.task_id, ct.conversation_id, ct.description, ct.goal,
       CASE
           WHEN ct.result_status <> '' THEN ct.result_status
           WHEN t.id IS NULL THEN '`+ConversationTaskStatusDeleted+`'
           WHEN t.archived_at IS NOT NULL THEN '`+ConversationTaskStatusArchived+`'
           WHEN t.deleted_at IS NOT NULL THEN '`+ConversationTaskStatusDeleted+`'
           ELSE t.status
       END,
       COALESCE(ct.activity_id, 0)
FROM conversation_tasks ct
LEFT JOIN tasks t ON t.id = ct.task_id
WHERE ct.delivered_at IS NULL
  AND ct.task_id <> ALL(COALESCE($2::bigint[], ARRAY[]::bigint[]))
  AND (ct.result_status <> '' OR NOT `+conversationTaskLiveSQL+`)
ORDER BY ct.created_at, ct.task_id
LIMIT $1`, limit, excludedTaskIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ConversationTaskResult{}
	for rows.Next() {
		var r ConversationTaskResult
		if err := rows.Scan(&r.TaskID, &r.ConversationID, &r.Description, &r.Goal, &r.Status, &r.ActivityID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// AppendConversationTaskResult 把任务结果写成来源会话的可见记录并登记到链接上,返回记录 id。
// 同一事务内先锁会话再锁链接,因此与会话删除互斥(不会向已删会话写入),并发/重放时
// 已有 activity_id 直接复用,每个任务至多一条可见记录。不会标记已投递(见
// CompleteConversationTaskDelivery)。
//
// 错误:链接不存在或不属于 conversationID(含会话已删) → sql.ErrNoRows;
// 已确认投递 → ErrConversationTaskDelivered;status 为空 → 校验错误。
func (d *DB) AppendConversationTaskResult(ctx context.Context, taskID, conversationID int64, status string, a Activity) (int64, error) {
	status = strings.TrimSpace(utf8Clean(status))
	if status == "" {
		return 0, errConversationTaskStatusRequired
	}
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	// 固定加锁顺序:会话 → 链接。DeleteConversation 先锁会话再级联链接,
	// 反向加锁会与之死锁。
	if err := lockConversation(ctx, tx, conversationID); err != nil {
		return 0, err
	}
	var activityID sql.NullInt64
	var delivered bool
	err = tx.QueryRowContext(ctx, `
SELECT activity_id, delivered_at IS NOT NULL
FROM conversation_tasks WHERE task_id=$1 AND conversation_id=$2 FOR UPDATE`, taskID, conversationID).Scan(&activityID, &delivered)
	if err != nil {
		return 0, err
	}
	if delivered {
		return 0, ErrConversationTaskDelivered
	}
	if activityID.Valid {
		return activityID.Int64, nil
	}
	id, err := insertConvActivity(ctx, tx, conversationID, a)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conversation_tasks SET activity_id=$3, result_status=$4
WHERE task_id=$1 AND conversation_id=$2`, taskID, conversationID, id, status); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// CompleteConversationTaskDelivery 在主模型确认处理完结果后把链接标为已投递。
// 幂等(重复调用保持首次 delivered_at),只作用于 (taskID, conversationID) 这一条链接。
// 链接不存在或属于别的会话 → sql.ErrNoRows;结果记录尚未写入 → ErrConversationTaskResultNotRecorded。
func (d *DB) CompleteConversationTaskDelivery(ctx context.Context, taskID, conversationID int64) error {
	res, err := d.ExecContext(ctx, `
UPDATE conversation_tasks SET delivered_at=COALESCE(delivered_at, now())
WHERE task_id=$1 AND conversation_id=$2 AND activity_id IS NOT NULL`, taskID, conversationID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n > 0 {
		return err
	}
	var exists bool
	if err := d.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM conversation_tasks WHERE task_id=$1 AND conversation_id=$2)`,
		taskID, conversationID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return sql.ErrNoRows
	}
	return ErrConversationTaskResultNotRecorded
}

// ConversationHasPendingTasks 报告会话是否还有「未投递」的任务结果需要等待,
// 用于主模型最终保存前的复测。只看 delivered_at IS NULL 的链接(已交付的永不再阻塞,
// 即使任务之后被重开),且满足任一条件即为 true:
//   - 链接不在 receivingTaskIDs 中(尚未交给主模型处理);
//   - 任务实际仍在运行(未终态且未被删除/归档),即使它在 receivingTaskIDs 中也算。
//
// receivingTaskIDs 是主模型当前正在处理、尚未 ack 的结果,复测时据此排除这些结果本身。
// 任务被删除/归档(不再运行)不会让会话永久等待;paused/queued 等非终态保持 true。
func (d *DB) ConversationHasPendingTasks(ctx context.Context, convID int64, receivingTaskIDs []int64) (bool, error) {
	var pending bool
	err := d.QueryRowContext(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM conversation_tasks ct
    LEFT JOIN tasks t ON t.id = ct.task_id
    WHERE ct.conversation_id = $1
      AND ct.delivered_at IS NULL
      AND (ct.task_id <> ALL(COALESCE($2::bigint[], ARRAY[]::bigint[])) OR `+conversationTaskLiveSQL+`)
)`, convID, receivingTaskIDs).Scan(&pending)
	return pending, err
}

// lockConversation 对会话行加 FOR KEY SHARE:与 DELETE 互斥,但不阻塞标题/置顶/touch
// 等普通更新。会话不存在(或已被并发删除)返回 sql.ErrNoRows。
func lockConversation(ctx context.Context, tx *sql.Tx, conversationID int64) error {
	var id int64
	return tx.QueryRowContext(ctx, `SELECT id FROM conversations WHERE id=$1 FOR KEY SHARE`, conversationID).Scan(&id)
}

// insertConversationTask 在创建任务的事务内登记来源会话,保存 description/goal 快照。
// 会话不存在或已删除返回 ErrConversationNotFound,调用方据此回滚整个任务创建。
func insertConversationTask(tx *sql.Tx, taskID, conversationID int64, description, goal string) error {
	ctx := context.Background()
	if err := lockConversation(ctx, tx, conversationID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrConversationNotFound
		}
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO conversation_tasks(task_id, conversation_id, description, goal)
VALUES ($1,$2,$3,$4)`, taskID, conversationID, description, goal)
	return err
}
