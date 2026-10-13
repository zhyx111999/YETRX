package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestPiLegacyHistoryPreservesContextWithoutReexecutingTools(t *testing.T) {
	items := []piLegacyActivity{
		{Kind: "user", Detail: "Earlier request", CreatedAt: time.Unix(100, 0)},
		{Kind: "tool_use", Tool: "Bash", Detail: `{"command":"legacy-command"}`},
		{Kind: "tool_result", Tool: "Bash", Detail: "legacy-result"},
		{Kind: "text", Detail: "Earlier answer"},
		{Kind: "user", Detail: "Current request"},
	}
	history, err := buildPiHistory(items, true, t.TempDir(), "profile-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 {
		t.Fatalf("expected two completed historical messages, got %d", len(history))
	}
	joined := string(history[0]) + string(history[1])
	for _, text := range []string{"Earlier request", "legacy-command", "legacy-result", "Earlier answer"} {
		if !strings.Contains(joined, text) {
			t.Errorf("history is missing %q", text)
		}
	}
	if strings.Contains(joined, "Current request") || strings.Contains(joined, `"type":"toolCall"`) {
		t.Fatal("current user message was duplicated or a historical tool call would be replayed")
	}
	var assistant struct {
		Role     string `json:"role"`
		Provider string `json:"provider"`
		Model    string `json:"model"`
	}
	if err := json.Unmarshal(history[1], &assistant); err != nil {
		t.Fatal(err)
	}
	if assistant.Role != "assistant" || assistant.Provider != "yetrx" || assistant.Model != "profile-1" {
		t.Fatalf("incorrect restored assistant identity: %+v", assistant)
	}
	history, err = buildPiHistory(items, false, t.TempDir(), "profile-1")
	if err != nil || len(history) != 3 {
		t.Fatalf("background initialization must keep the last historical user message: len=%d, err=%v", len(history), err)
	}
}

func TestYetrxJWTKeepsExistingLoginSessions(t *testing.T) {
	key := []byte("temporary-test-key-for-migration-only")
	for _, subject := range []string{"ARTEX", "YETRX"} {
		token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
			Subject: subject, ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		}).SignedString(key)
		if err != nil || !verifyJWT(token, key) {
			t.Fatalf("existing %s session was rejected: %v", subject, err)
		}
	}
	token, err := signJWT(key)
	if err != nil {
		t.Fatal(err)
	}
	claims := jwt.RegisteredClaims{}
	if _, _, err := jwt.NewParser().ParseUnverified(token, &claims); err != nil || claims.Subject != "YETRX" {
		t.Fatalf("new token should identify YETRX: subject=%q, err=%v", claims.Subject, err)
	}
}
