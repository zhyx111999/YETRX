package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func TestChatPersonaPromptStaysAtRequestBoundary(t *testing.T) {
	provider := &parentPromptProvider{}
	chat := NewChatAgent(provider, "fixture", t.TempDir(), nil, 100000)
	chat.SetNonStreaming(func() bool { return true })
	chat.SetPersonaResolver(func() string { return "以简洁、温和的方式回答。\n保留这行格式。" })

	if _, err := chat.Chat(context.Background(), "agent", "conv-persona", "ORIGINAL_USER_TEXT", 2, time.Minute, false, func(db.Activity) {}); err != nil {
		t.Fatal(err)
	}

	if len(provider.request.System) < 2 {
		t.Fatalf("system segments=%d, want base prompt plus persona", len(provider.request.System))
	}
	if provider.request.System[len(provider.request.System)-1] != RenderPersonaPrompt("以简洁、温和的方式回答。\n保留这行格式。") {
		t.Fatalf("persona was not the first request-boundary addendum: %q", provider.request.System)
	}
	foundUser := false
	for _, message := range provider.request.Messages {
		for _, block := range message.Content {
			if strings.Contains(block.Text, PersonaPromptHeader) {
				t.Fatal("persona leaked into the persisted user message")
			}
			if block.Text == "ORIGINAL_USER_TEXT" {
				foundUser = true
			}
		}
	}
	if !foundUser {
		t.Fatalf("original user message missing or changed: %+v", provider.request.Messages)
	}
}

func TestRenderPersonaPromptOmitsEmptyValues(t *testing.T) {
	for _, value := range []string{"", " \n\t"} {
		if got := RenderPersonaPrompt(value); got != "" {
			t.Fatalf("RenderPersonaPrompt(%q)=%q, want empty", value, got)
		}
	}
}
