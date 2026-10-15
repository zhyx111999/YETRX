package agentexec

import (
	"context"
	"encoding/json"

	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
)

func (w *worker) canUse(ctx context.Context, name string, input json.RawMessage, pc permission.Context) permission.Decision {
	out, err := w.call(ctx, "permission", rpcRequest{Name: name, Input: input, Permission: pc})
	if err != nil {
		w.fail(err)
		return permission.Denied(err.Error())
	}
	out.Decision.UpdatedInput = out.Updated
	return out.Decision
}

type brokerHooks struct{ w *worker }

func (h *brokerHooks) PreToolUse(ctx context.Context, name string, input []byte) (bool, string, []byte) {
	var out rpcReply
	if h.w.hostHooks {
		var err error
		out, err = h.w.call(ctx, "hook.pre", rpcRequest{Name: name, Input: input})
		if err != nil {
			h.w.fail(err)
			return true, err.Error(), nil
		}
		if out.Bool {
			return true, out.Text, out.Updated
		}
	}
	if h.w.todos && name == "TodoWrite" {
		actual := input
		if len(out.Updated) > 0 {
			actual = out.Updated
		}
		result, err := h.w.call(ctx, "todo.write", rpcRequest{Input: actual})
		if err != nil {
			h.w.fail(err)
			return true, err.Error(), nil
		}
		if result.Result.IsError {
			return true, result.Result.Flatten(), nil
		}
	}
	return out.Bool, out.Text, out.Updated
}
func (h *brokerHooks) PostToolUse(ctx context.Context, name string, input, result []byte, isErr bool) {
	if h.w.hostHooks {
		h.w.must(ctx, "hook.post", rpcRequest{Name: name, Input: input, Result: result, IsError: isErr})
	}
}
func (h *brokerHooks) Stop(ctx context.Context, messages []llm.Message) (bool, []string, string) {
	if !h.w.hostHooks {
		return false, nil, ""
	}
	out := h.w.must(ctx, "hook.stop", rpcRequest{Messages: messages})
	return out.Bool, out.Strings, out.Text
}

type brokerLifecycleHooks struct{ brokerHooks }

func (h *brokerLifecycleHooks) FireSessionStart(ctx context.Context) {
	h.w.must(ctx, "hook.start", rpcRequest{})
}
func (h *brokerLifecycleHooks) FireUserPromptSubmit(ctx context.Context, input string) string {
	return h.w.must(ctx, "hook.prompt", rpcRequest{Text: input}).Text
}

type brokerCompactor struct{ w *worker }

func (c *brokerCompactor) Pre(ctx context.Context, msgs []llm.Message, tokens int) []llm.Message {
	return c.w.must(ctx, "compact.pre", rpcRequest{Messages: msgs, Tokens: tokens}).Messages
}
func (c *brokerCompactor) Reactive(ctx context.Context, msgs []llm.Message) ([]llm.Message, bool) {
	out := c.w.must(ctx, "compact.reactive", rpcRequest{Messages: msgs})
	return out.Messages, out.Bool
}
func (c *brokerCompactor) IsOverflow(err error) bool {
	return c.w.must(c.w.context(), "compact.overflow", rpcRequest{Error: (*errorRefs)(nil).encode(err)}).Bool
}

// This wrapper snapshots the real loop history once per request, before any
// ContextView projection; never per token. It also covers non-transcript runs.
type snapshotCompactor struct {
	w     *worker
	inner harness.Compactor
}

func (c *snapshotCompactor) Pre(ctx context.Context, msgs []llm.Message, tokens int) []llm.Message {
	if c.inner != nil {
		msgs = c.inner.Pre(ctx, msgs, tokens)
	}
	c.w.must(ctx, "snapshot", rpcRequest{Messages: msgs})
	return msgs
}
func (c *snapshotCompactor) Reactive(ctx context.Context, msgs []llm.Message) ([]llm.Message, bool) {
	if c.inner == nil {
		return msgs, false
	}
	return c.inner.Reactive(ctx, msgs)
}
func (c *snapshotCompactor) IsOverflow(err error) bool {
	return c.inner != nil && c.inner.IsOverflow(err)
}

type snapshotView struct{ *snapshotCompactor }

func (c *snapshotView) View(ctx context.Context, msgs []llm.Message) []llm.Message {
	return c.w.must(ctx, "compact.view", rpcRequest{Messages: msgs}).Messages
}
