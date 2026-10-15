package db

import (
	"crypto/sha256"
	"fmt"
	"testing"
	"time"
)

func TestUpgradePromptDefaultPreservesEditsAndHistory(t *testing.T) {
	d := openPersonaTestDB(t)
	key := fmt.Sprintf("prompt_upgrade_%d", time.Now().UnixNano())
	var id int64
	if err := d.QueryRow(`INSERT INTO agents(key,name,role,enabled) VALUES ($1,$1,'assistant',true) RETURNING id`, key).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = d.Exec(`DELETE FROM agents WHERE id=$1`, id) })
	old, next := "known old default\n正文", "background default\n新正文"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(old)))
	if _, err := d.SavePrompt(id, old, "fixture", "test"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := d.UpgradePromptDefault(key, digest, next); err != nil {
			t.Fatal(err)
		}
	}
	got, err := d.CurrentPrompt(id)
	versions, _ := d.ListPromptVersions(id)
	if err != nil || got != next || len(versions) != 2 || versions[1].Template != old {
		t.Fatalf("prompt=%q versions=%+v err=%v", got, versions, err)
	}
	if _, err := d.SavePrompt(id, "user custom prompt", "fixture", "test"); err != nil {
		t.Fatal(err)
	}
	if err := d.UpgradePromptDefault(key, digest, next); err != nil {
		t.Fatal(err)
	}
	got, _ = d.CurrentPrompt(id)
	versions, _ = d.ListPromptVersions(id)
	if got != "user custom prompt" || len(versions) != 3 {
		t.Fatal("overwrote user prompt")
	}
}
