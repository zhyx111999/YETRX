package agentexec

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

type bridgedTool struct {
	spec  toolSpec
	w     *worker
	local tool.CoreTool
}

func (w *worker) makeTool(ctx context.Context, spec toolSpec) (tool.CoreTool, error) {
	t := &bridgedTool{spec: spec, w: w}
	if spec.Local != nil {
		var err error
		if spec.Local.Kind == "sdk" {
			var name string
			if err = json.Unmarshal(spec.Local.Data, &name); err != nil {
				return nil, err
			}
			for _, sdk := range tool.DefaultTools() {
				if sdk.Name() == name {
					t.local = sdk
					break
				}
			}
			if t.local == nil {
				return nil, fmt.Errorf("agentexec: unknown SDK tool %q", name)
			}
		} else {
			ctor := w.config.LocalTools[spec.Local.Kind]
			if ctor == nil {
				return nil, fmt.Errorf("agentexec: no local constructor for %q", spec.Local.Kind)
			}
			t.local, err = ctor(ctx, spec.Local.Data)
			if err != nil {
				return nil, err
			}
			if t.local == nil {
				return nil, fmt.Errorf("agentexec: constructor %q returned nil", spec.Local.Kind)
			}
		}
		if t.local.Name() != spec.Name {
			return nil, fmt.Errorf("agentexec: local tool name mismatch: %q != %q", t.local.Name(), spec.Name)
		}
		// Metadata alone is insufficient: DB scalar defaults must be injected by
		// the same decorator used on the host before calling the SDK implementation.
		t.local = agent.DecorateTool(t.local, spec.Description, spec.Schema)
	}
	return t, nil
}
func (t *bridgedTool) Name() string                { return t.spec.Name }
func (t *bridgedTool) Description() string         { return t.spec.Description }
func (t *bridgedTool) Prompt() string              { return t.spec.Prompt }
func (t *bridgedTool) InputSchema() map[string]any { return t.spec.Schema }
func (t *bridgedTool) AcceptsRawInput() bool       { return t.spec.Raw }
func (t *bridgedTool) IsReadOnly(in json.RawMessage) bool {
	return t.w.must(t.w.context(), "tool.readonly", rpcRequest{Name: t.Name(), Input: in}).Bool
}
func (t *bridgedTool) IsConcurrencySafe(in json.RawMessage) bool {
	return t.w.must(t.w.context(), "tool.concurrent", rpcRequest{Name: t.Name(), Input: in}).Bool
}
func (t *bridgedTool) CheckPermissions(ctx context.Context, in json.RawMessage, pc permission.Context) permission.Decision {
	out, err := t.w.call(ctx, "tool.permission", rpcRequest{Name: t.Name(), Input: in, Permission: pc})
	if err != nil {
		t.w.fail(err)
		return permission.Denied(err.Error())
	}
	out.Decision.UpdatedInput = out.Updated
	return out.Decision
}
func (t *bridgedTool) Call(ctx context.Context, in json.RawMessage, tc *tool.ToolContext) (result tool.Result, err error) {
	if t.local != nil {
		return t.callLocal(ctx, in, tc)
	}
	var out rpcReply
	id := ""
	if tc != nil {
		id = tc.ToolUseID
	}
	deadline, _ := ctx.Deadline()
	err = t.w.peer.Stream(ctx, "tool.call", rpcRequest{Name: t.Name(), Input: in, ToolUseID: id, Deadline: deadline}, func(raw json.RawMessage) error {
		var progress tool.ProgressInfo
		if err := json.Unmarshal(raw, &progress); err != nil {
			return err
		}
		if tc != nil && tc.Emit != nil {
			tc.Emit(progress)
		}
		return nil
	}, &out)
	if err == nil {
		err = (*errorRefs)(nil).decode(out.Error)
	}
	if t.w.unlock != nil {
		t.w.unlock.Add(out.Strings...)
	}
	return out.Result, err
}

func (t *bridgedTool) callLocal(ctx context.Context, in json.RawMessage, tc *tool.ToolContext) (result tool.Result, err error) {
	if t.w.observeLocal {
		deadline, _ := ctx.Deadline()
		if _, err = t.w.call(ctx, "local.start", rpcRequest{Name: t.Name(), Deadline: deadline}); err != nil {
			return tool.Result{}, err
		}
	}
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("agentexec: local tool %s panicked (%T)", t.Name(), p)
			result = tool.Errorf(err.Error())
		}
	}()
	return t.local.Call(ctx, in, tc)
}
