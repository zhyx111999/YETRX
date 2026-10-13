package agent

import "github.com/Autumn-27/artex/minimalprompt"

// PiSystemPrompt uses the same editable prompt and template variables as the
// other conversation modes. The code default contains only tool instructions.
func PiSystemPrompt(workspace string) string {
	return renderSystem(minimalprompt.AgentKey, minimalprompt.Default, struct {
		DataDir string
		Now     string
	}{DataDir: workspace, Now: nowStr()})
}
