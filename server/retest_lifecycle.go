package server

import (
	"context"
	"fmt"
	"log"

	"github.com/Autumn-27/artex/db"
)

// A retest can span several parent turns. Only an execution with no remaining
// delegated work seals the verdict; a foreground stop never cancels its children.
func (s *Server) beginRetestTurn(ctx context.Context, c *db.Conversation) (func(string, string), bool) {
	noop := func(string, string) {}
	turn := handoffTurnFrom(ctx)
	if c.AgentKey != db.FindingRetestAgentKey {
		return noop, true
	}
	r, err := s.m.pg.FindingRetestForConversation(context.WithoutCancel(ctx), c.ID)
	if err != nil {
		log.Printf("[conv %d] load retest: %v", c.ID, err)
		if !turn.automatic {
			if err := s.m.pg.FailPendingRetestForConversation(c.ID, "复测状态读取失败，请重新发起"); err != nil {
				log.Printf("[conv %d] seal retest: %v", c.ID, err)
			}
		}
		return noop, false
	}
	if r == nil || (r.Status != "pending" && r.Status != "running") {
		return noop, true
	}
	finish := func(status, reason string) {
		if turn.automatic && !turn.hasStarted() {
			return // Retained results retry after cancellation or setup/storage failure.
		}
		pending, err := s.conversationHasBackground(ctx, c.ID)
		if err != nil {
			// A storage failure is not evidence that background work has finished.
			log.Printf("[retest %d] check background work: %v", r.ID, err)
			return
		}
		if pending {
			return
		}
		if ctx.Err() != nil {
			status, reason = "stopped", "当前复测执行已停止或服务已关闭"
		}
		s.finishRetest(r.ID, status, reason)
	}
	if ctx.Err() != nil {
		return finish, true
	}
	if r.Status == "pending" {
		started, err := s.m.pg.StartFindingRetest(ctx, r.ID)
		if err != nil {
			finish("failed", err.Error())
			return noop, false
		}
		if !started {
			return noop, false
		}
	}
	return finish, true
}

// Once the parent has staged its final verdict it must not start fresh work
// whose evidence could invalidate that verdict in the same turn.
func (s *Server) checkRetestDelegation(ctx context.Context, id int64) error {
	if id == 0 {
		return nil
	}
	r, err := s.m.pg.FindingRetestForConversation(ctx, id)
	if err != nil {
		return err
	}
	if r != nil && r.Status == "running" && r.Verdict != "" {
		return fmt.Errorf("复测结论已保存，不能再派发新的后台工作")
	}
	return nil
}

// Stopping a workflow is an explicit background action. It does not start an
// automatic model reply, but a waiting retest must not remain running forever.
func (s *Server) finishRetestAfterBackgroundStop(ctx context.Context, c *db.Conversation) error {
	if c.AgentKey != db.FindingRetestAgentKey {
		return nil
	}
	pending, err := s.conversationHasBackground(ctx, c.ID)
	if err != nil || pending {
		return err
	}
	r, err := s.m.pg.FindingRetestForConversation(ctx, c.ID)
	if err == nil && r != nil && (r.Status == "pending" || r.Status == "running") {
		return s.m.pg.FinishFindingRetest(r.ID, "stopped", "关联的后台工作已停止，未自动生成复测结论")
	}
	return err
}
