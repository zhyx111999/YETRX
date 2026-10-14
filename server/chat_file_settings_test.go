package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
	actool "github.com/Autumn-27/norma/tool"
)

func TestChatFileSettingsRuntimeLimitAndPersistence(t *testing.T) {
	e := newNativeEnv(t)
	s := &Server{m: &Manager{pg: e.pg, dir: t.TempDir()}}
	original, existed, err := e.pg.GetSetting(settingChatFileMaxMiB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = e.pg.SetSetting(settingChatFileMaxMiB, original)
		} else {
			_, _ = e.pg.Exec(`DELETE FROM settings WHERE key=$1`, settingChatFileMaxMiB)
		}
	})
	if _, err = e.pg.Exec(`DELETE FROM settings WHERE key=$1`, settingChatFileMaxMiB); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("PUT /api/settings", s.putSettings)
	request := func(method, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, "/api/settings", strings.NewReader(body)))
		return rec
	}
	readLimit := func(rec *httptest.ResponseRecorder) int64 {
		t.Helper()
		if rec.Code != 200 {
			t.Fatalf("settings: %d %s", rec.Code, rec.Body)
		}
		var payload struct {
			Limit int64 `json:"chat_file_max_mib"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return payload.Limit
	}
	if limit := readLimit(request("GET", "")); limit != 128 {
		t.Fatal("missing setting must default to 128", limit)
	}
	if _, exists, _ := e.pg.GetSetting(settingChatFileMaxMiB); exists {
		t.Fatal("reading default should not write a setting")
	}
	for _, body := range []string{`{"chat_file_max_mib":-1}`, `{"chat_file_max_mib":1.5}`, `{"chat_file_max_mib":"256"}`, `{"chat_file_max_mib":8796093022208}`, `{"chat_file_max_mib":9223372036854775808}`} {
		if rec := request("PUT", body); rec.Code != 400 {
			t.Fatalf("invalid value accepted: %s %d", body, rec.Code)
		}
	}
	workers := s.m.Workers()
	if rec := request("PUT", fmt.Sprintf(`{"chat_file_max_mib":-1,"workers":%d}`, workers+1)); rec.Code != 400 || s.m.Workers() != workers {
		t.Fatal("invalid file setting changed other settings")
	}
	conv, err := e.pg.CreateConversation("agent", "configurable file limit", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.pg.DeleteConversation(conv.ID) })
	source := filepath.Join(t.TempDir(), "result.bin")
	ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{SessionID: s.convBusyKey(conv.ID)})
	tool := s.chatFileTools()[0] // Retain this instance across all settings changes.
	input, _ := json.Marshal(map[string]string{"path": source})
	resize := func(size int64) {
		t.Helper()
		file, err := os.OpenFile(source, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		err = file.Truncate(size)
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	send := func(wantSuccess bool) chatFile {
		t.Helper()
		result, err := tool.Call(ctx, input, &actool.ToolContext{})
		if err != nil || result.IsError == wantSuccess {
			t.Fatalf("send success=%v err=%v result=%s", wantSuccess, err, result.Flatten())
		}
		var out struct {
			File chatFile `json:"file"`
		}
		if wantSuccess {
			if err = json.Unmarshal([]byte(result.Flatten()), &out); err != nil {
				t.Fatal(err)
			}
		}
		return out.File
	}
	setLimit := func(limit int64) {
		t.Helper()
		if got := readLimit(request("PUT", `{"chat_file_max_mib":`+strconv.FormatInt(limit, 10)+`}`)); got != limit {
			t.Fatal("wrong saved limit", got)
		}
	}
	setLimit(1)
	resize(1 << 20)
	saved := send(true)
	resize((1 << 20) + 1)
	send(false)
	setLimit(2)
	file := send(true)
	_ = os.RemoveAll(filepath.Join(s.chatFileDir(conv.ID), saved.ID))
	saved = file
	if readLimit(request("PUT", `{}`)) != 2 || readLimit(request("PUT", `{"chat_file_max_mib":null}`)) != 2 {
		t.Fatal("partial update reset limit")
	}
	// A fresh server instance reads the persisted setting, without cached state.
	reloaded := &Server{m: &Manager{pg: e.pg}}
	if value, err := reloaded.chatFileMaxMiB(); err != nil || value != 2 {
		t.Fatal("setting did not survive reload", value, err)
	}
	// Raising above the old fixed cap must actually permit a larger file.
	resize((129 << 20) + 1)
	setLimit(129)
	send(false)
	setLimit(130)
	file = send(true)
	_ = os.RemoveAll(filepath.Join(s.chatFileDir(conv.ID), file.ID))
	setLimit(0)
	file = send(true)
	_ = os.RemoveAll(filepath.Join(s.chatFileDir(conv.ID), file.ID))
	if readLimit(request("GET", "")) != 0 {
		t.Fatal("unlimited did not persist")
	}
	// Lowering the cap does not revoke an already-delivered download.
	setLimit(1)
	mux.HandleFunc("GET /api/conversations/{id}/files/{file}", s.downloadChatFile)
	req := httptest.NewRequest("GET", fmt.Sprintf("/api/conversations/%d/files/%s", conv.ID, saved.ID), nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Body.Len() != (1<<20)+1 {
		t.Fatal("existing download affected by current cap", rec.Code)
	}
	if err = e.pg.SetSetting(settingChatFileMaxMiB, "broken"); err != nil {
		t.Fatal(err)
	}
	if request("GET", "").Code != 503 {
		t.Fatal("invalid persisted config silently accepted")
	}
	resize(1)
	send(false)
	setLimit(128) // Invalid stored data can still be repaired from settings.
}

func TestChatFileDescriptionUpgradePreservesCustomTool(t *testing.T) {
	e := newNativeEnv(t)
	s := &Server{m: &Manager{pg: e.pg}}
	before, err := e.pg.GetTool("send_file")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if before == nil {
			_, _ = e.pg.Exec(`DELETE FROM tools WHERE key='send_file'`)
		} else {
			_ = e.pg.UpdateTool(before.Key, before.Description, before.Schema, mustJSON(before.Agents), before.Enabled)
		}
	})
	old := "将已经生成的本地文件作为可下载附件发送给当前会话用户。文件先复制保存，源文件后续修改或工作树删除不影响下载。支持任意普通文件，单个不超过 128 MiB；目录请先打包。仅在主对话调用，子任务将文件路径交给主代理审查后发送。"
	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"保留说明","default":"file.txt"}}}`)
	if err = e.pg.SeedTool("send_file", old, schema, mustJSON([]string{e.keys[1]})); err != nil {
		t.Fatal(err)
	}
	if err = e.pg.UpdateTool("send_file", old, schema, mustJSON([]string{e.keys[1]}), false); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = s.upgradeDelegationDescriptions(); err != nil {
			t.Fatal(err)
		}
	}
	row, err := e.pg.GetTool("send_file")
	if err != nil {
		t.Fatal(err)
	}
	if row.Description != chatFileDescription || row.Enabled || len(row.Agents) != 1 || row.Agents[0] != e.keys[1] || !strings.Contains(string(row.Schema), "file.txt") {
		t.Fatal("built-in description migration changed preferences")
	}
	if err = e.pg.UpdateTool("send_file", "用户说明", schema, mustJSON(row.Agents), false); err != nil {
		t.Fatal(err)
	}
	if err = s.upgradeDelegationDescriptions(); err != nil {
		t.Fatal(err)
	}
	row, _ = e.pg.GetTool("send_file")
	if row.Description != "用户说明" {
		t.Fatal("custom tool description overwritten")
	}
}
