package agent

import (
	"strings"
	"testing"
)

func TestExplorationContextPreservesCustomPrompts(t *testing.T) {
	old := PromptOverride
	t.Cleanup(func() { PromptOverride = old })
	PromptOverride = func(key string) (string, bool) { return "CUSTOM_" + key, true }
	for role, system := range map[string]string{
		"planner": plannerSystem("goal", t.TempDir(), t.TempDir()),
		"worker":  workerSystem("", "", t.TempDir(), t.TempDir()),
	} {
		if !strings.HasPrefix(system, "CUSTOM_"+role) || strings.Count(system, ExplorationSubtaskContext) != 1 {
			t.Fatalf("%s prompt lost custom body or exploration context: %q", role, system)
		}
		if strings.Contains(system, SubagentContext) || strings.Contains(system, ParentWorkflowPrompt) {
			t.Fatalf("%s received the conversational delegation prompt", role)
		}
	}
}
