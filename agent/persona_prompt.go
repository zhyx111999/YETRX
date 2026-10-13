package agent

import "strings"

// PersonaPromptHeader identifies the request-scoped global persona block. The
// persona is trusted configuration, but it remains delimited so the model can
// distinguish it from the host-controlled prompt around it.
const PersonaPromptHeader = "【当前激活的全局人格】"

// RenderPersonaPrompt returns the system-prompt segment for an active persona.
// Empty or whitespace-only values produce no segment. The persona body itself is
// preserved exactly so editing it does not silently rewrite user-authored text.
func RenderPersonaPrompt(persona string) string {
	if strings.TrimSpace(persona) == "" {
		return ""
	}
	return PersonaPromptHeader + "\n" + persona
}
