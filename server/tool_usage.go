package server

import (
	"context"
	"encoding/json"
	"log"

	actool "github.com/Autumn-27/norma/tool"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

type toolUsageRecorder interface {
	InsertToolUsage(*db.ToolUsage) error
}

// meteredTool records one row immediately before a catalog tool is invoked. It
// embeds the resolved tool so schema overrides, defaults and permission behavior
// remain unchanged.
type meteredTool struct {
	actool.CoreTool
	recorder toolUsageRecorder
	toolKey  string
	agentKey string
	ri       agent.RunInfo
}

func meterTool(t actool.CoreTool, recorder toolUsageRecorder, toolKey, agentKey string, ri agent.RunInfo) actool.CoreTool {
	if recorder == nil {
		return t
	}
	return &meteredTool{CoreTool: t, recorder: recorder, toolKey: toolKey, agentKey: agentKey, ri: ri}
}

func (m *meteredTool) UnwrapTool() actool.CoreTool { return m.CoreTool }

func (m *meteredTool) Call(ctx context.Context, input json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	m.recordUsage()
	return m.CoreTool.Call(ctx, input, tc)
}

func (m *meteredTool) recordUsage() {
	if err := m.recorder.InsertToolUsage(&db.ToolUsage{
		ToolKey:       m.toolKey,
		AgentKey:      m.agentKey,
		TaskID:        m.ri.TaskID,
		ExplorationID: m.ri.ExplorationID,
		IntentID:      m.ri.IntentID,
		SessionID:     m.ri.SessionID,
	}); err != nil {
		log.Printf("[toolusage] insert %s: %v", m.toolKey, err)
	}
}

// Local calls execute in a runner; their host catalog wrapper still owns the
// authoritative attribution. Observe the call without executing it twice.
func recordLocalToolStart(_ context.Context, t actool.CoreTool) {
	for depth := 0; t != nil && depth < 64; depth++ {
		if metered, ok := t.(*meteredTool); ok {
			metered.recordUsage()
			return
		}
		wrapped, ok := t.(interface{ UnwrapTool() actool.CoreTool })
		if !ok {
			return
		}
		t = wrapped.UnwrapTool()
	}
}
