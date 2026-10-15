package server

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/workflow"
)

// A receipt belongs to one automatic parent turn, not to the lifetime of a
// conversation. Pending task results are acknowledged only after that turn ran.
type backgroundReceipt struct {
	taskIDs []int64
	runKeys []string
	started bool
}

type backgroundReceiptKey struct{}

func receiptFrom(ctx context.Context) *backgroundReceipt {
	receipt, _ := ctx.Value(backgroundReceiptKey{}).(*backgroundReceipt)
	return receipt
}

func (s *Server) startTaskDeliveryWorker() {
	if s.m.pg == nil {
		return
	}
	s.taskDeliveryWG.Add(1)
	go func() {
		defer s.taskDeliveryWG.Done()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			if s.ctx.Err() != nil {
				return
			}
			s.retryWorkflowDeliveries()
			if err := s.collectTaskDeliveries(); err != nil && s.ctx.Err() == nil {
				log.Printf("[task delivery] reconcile: %v", err)
			}
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// PostgreSQL is the source of truth, including results completed while the
// server was down. The in-memory set only prevents duplicate queue entries.
func (s *Server) collectTaskDeliveries() error {
	s.chatMu.Lock()
	excluded := make([]int64, 0, len(s.taskDelivering))
	for id := range s.taskDelivering {
		excluded = append(excluded, id)
	}
	s.chatMu.Unlock()
	results, err := s.m.pg.PendingConversationTaskResults(s.ctx, 200, excluded...)
	if err != nil {
		return err
	}
	for _, result := range results {
		if s.engine != nil && s.engine.inflightCount(strconv.FormatInt(result.TaskID, 10)) > 0 {
			continue // Wait for the task's final evidence and cancellation to settle.
		}
		session := s.convBusyKey(result.ConversationID)
		s.chatMu.Lock()
		if s.taskDelivering == nil {
			s.taskDelivering = make(map[int64]bool)
		}
		if s.taskDelivering[result.TaskID] || s.workflowMuted[session] {
			s.chatMu.Unlock()
			continue
		}
		s.taskDelivering[result.TaskID] = true
		s.chatMu.Unlock()
		body := taskResultReport(result)
		s.queueWorkflowDelivery(session, workflowDelivery{
			TaskID: result.TaskID, TaskStatus: result.Status,
			Activity: db.Activity{Kind: "text", Tool: "spawn_task", Summary: "探索长任务返回", Detail: body},
			Prompt:   body,
		})
	}
	return nil
}

func taskResultReport(result db.ConversationTaskResult) string {
	var body strings.Builder
	fmt.Fprintf(&body, "探索长任务 task_id=%d 已结束，状态：%s。\n任务：%s\n目标：%s\n",
		result.TaskID, result.Status, clippedTaskText(result.Description), clippedTaskText(result.Goal))
	if result.Status == "deleted" || result.Status == "archived" {
		body.WriteString("任务已删除或归档，请先核对任务状态及可用的归档记录。不得把删除或归档视为目标达成或漏洞已修复。")
	} else {
		fmt.Fprintf(&body, "详情：/function/tasks/detail?id=%d\n请用 get_task_graph、list_task_findings、list_task_worker_traces / get_task_worker_trace 按 task_id 读取实际结果和证据，核验后再汇报。任务结束不等于目标已验证，更不等于漏洞已修复。", result.TaskID)
	}
	return body.String()
}

func clippedTaskText(text string) string {
	const limit = 4000
	runes := []rune(text)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return text
}

func workflowResultKey(run workflow.Run) string {
	return fmt.Sprintf("%s:%d", run.ID, run.FinishedAt.UnixNano())
}

// A retest may finish only after all delegated work and earlier result batches
// have been considered. This turn's results are acknowledged after it returns.
func (s *Server) conversationHasBackground(ctx context.Context, id int64) (bool, error) {
	session := s.convBusyKey(id)
	receipt := receiptFrom(ctx)
	if s.workflows != nil {
		for _, run := range s.workflows.List(session) {
			if run.Status == workflow.Running || run.Status == workflow.Paused {
				return true, nil
			}
			if (run.Status == workflow.Succeeded || run.Status == workflow.Failed) && !run.FinishedAt.Before(s.workflowEpoch) {
				key := workflowResultKey(run)
				s.chatMu.Lock()
				reported := s.workflowReported[key]
				s.chatMu.Unlock()
				if !reported && (receipt == nil || !slices.Contains(receipt.runKeys, key)) {
					return true, nil
				}
			}
		}
	}
	s.chatMu.Lock()
	for _, message := range s.workflowPending[session] {
		if message.Prompt != "" {
			s.chatMu.Unlock()
			return true, nil
		}
	}
	s.chatMu.Unlock()
	var receiving []int64
	if receipt != nil && s.ctx.Err() == nil {
		receiving = receipt.taskIDs
	}
	return s.m.pg.ConversationHasPendingTasks(context.WithoutCancel(ctx), id, receiving)
}
