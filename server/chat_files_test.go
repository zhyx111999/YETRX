package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
	actool "github.com/Autumn-27/norma/tool"
)

func TestChatFileCopyPreservesBytesAndRejectsInvalidSources(t *testing.T) {
	s := &Server{m: &Manager{dir: t.TempDir()}}
	dir := t.TempDir()
	source := filepath.Join(dir, "报告.bin")
	bytes := []byte{0, 1, 2, 255, 10}
	if err := os.WriteFile(source, bytes, 0600); err != nil {
		t.Fatal(err)
	}
	file, err := s.copyChatFile(context.Background(), 1, source, "")
	if err != nil {
		t.Fatal(err)
	}
	if file.Name != "报告.bin" || file.Size != int64(len(bytes)) || len(file.SHA256) != 64 {
		t.Fatalf("metadata=%+v", file)
	}
	if err = os.Remove(source); err != nil {
		t.Fatal(err)
	}
	copied, err := os.ReadFile(filepath.Join(s.chatFileDir(1), file.ID, "content"))
	if err != nil || string(copied) != string(bytes) {
		t.Fatal("copy changed", err)
	}
	if _, err = s.copyChatFile(context.Background(), 1, dir, ""); err == nil {
		t.Fatal("directory accepted")
	}
	if err = os.WriteFile(source, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../leak.txt", "file\r\nInjected: true", `a\b.txt`, strings.Repeat("n", 256)} {
		if _, err = s.copyChatFile(context.Background(), 1, source, name); err == nil {
			t.Fatalf("invalid name accepted: %q", name)
		}
	}
	f, err := os.OpenFile(source, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(maxChatUpload + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err = s.copyChatFile(context.Background(), 1, source, ""); err == nil {
		t.Fatal("oversize file accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = s.copyChatFile(ctx, 1, source, ""); err == nil {
		t.Fatal("cancelled send accepted")
	}
	entries, _ := os.ReadDir(s.chatFileDir(1))
	if len(entries) != 1 {
		t.Fatal("failed sends left artifacts", len(entries))
	}
}

func TestChatFileToolAndAuthenticatedDownload(t *testing.T) {
	e := newNativeEnv(t)
	s := &Server{m: &Manager{pg: e.pg, dir: t.TempDir()}, jwtKey: []byte("01234567890123456789012345678901")}
	conv, err := e.pg.CreateConversation("agent", "file delivery", nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := e.pg.CreateConversation("agent", "other", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.pg.DeleteConversation(conv.ID); _ = e.pg.DeleteConversation(other.ID) })
	cwd := t.TempDir()
	source := filepath.Join(cwd, "result.txt")
	content := "你好，附件\nfile contents\x00"
	if err = os.WriteFile(source, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := agent.WithRunInfo(context.Background(), agent.RunInfo{SessionID: s.convBusyKey(conv.ID)})
	result, err := s.chatFileTools()[0].Call(ctx, json.RawMessage(`{"path":"result.txt","name":"验收报告.txt","caption":"已完成，请下载。"}`), &actool.ToolContext{WorkingDir: cwd})
	if err != nil || result.IsError {
		t.Fatal("send failed", err, result.Flatten())
	}
	var sent struct {
		File chatFile `json:"file"`
	}
	if err = json.Unmarshal([]byte(result.Flatten()), &sent); err != nil {
		t.Fatal(err)
	}
	rows, _, err := e.pg.ConvActivityList(conv.ID, 0, 100)
	if err != nil || len(rows) != 1 || rows[0].Kind != "file" {
		t.Fatal("file message missing", err, rows)
	}
	detail, err := e.pg.ConvActivityDetail(conv.ID, rows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(detail, source) || !strings.Contains(detail, "已完成，请下载。") {
		t.Fatal("wrong public metadata", detail)
	}
	if err = os.WriteFile(source, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/conversations/{id}/files/{file}", s.downloadChatFile)
	mux.HandleFunc("DELETE /api/conversations/{id}", s.pgDeleteConversation)
	srv := httptest.NewServer(s.requireAuth(mux))
	defer srv.Close()
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("%s/api/conversations/%d/files/%s", srv.URL, conv.ID, sent.File.ID)
	request := func(method, url, auth, byteRange string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, url, nil)
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		if byteRange != "" {
			req.Header.Set("Range", byteRange)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	unauth := request("GET", url, "", "")
	_ = unauth.Body.Close()
	if unauth.StatusCode != 401 {
		t.Fatal("download is not authenticated", unauth.StatusCode)
	}
	response := request("GET", url, token, "")
	data, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != 200 || string(data) != content || !strings.HasPrefix(response.Header.Get("Content-Disposition"), "attachment;") || response.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("download contract failed", response.StatusCode, string(data))
	}
	partial := request("GET", url, token, "bytes=0-2")
	data, _ = io.ReadAll(partial.Body)
	_ = partial.Body.Close()
	if partial.StatusCode != 206 || string(data) != content[:3] {
		t.Fatal("range download failed")
	}
	wrong := request("GET", fmt.Sprintf("%s/api/conversations/%d/files/%s", srv.URL, other.ID, sent.File.ID), token, "")
	_ = wrong.Body.Close()
	if wrong.StatusCode != 404 {
		t.Fatal("attachment crossed conversations")
	}
	missing := request("GET", strings.Replace(url, sent.File.ID, strings.Repeat("0", 32), 1), token, "")
	_ = missing.Body.Close()
	if missing.StatusCode != 404 {
		t.Fatal("missing attachment accepted")
	}
	deleted := request("DELETE", fmt.Sprintf("%s/api/conversations/%d", srv.URL, conv.ID), token, "")
	_ = deleted.Body.Close()
	if deleted.StatusCode != 200 {
		t.Fatal("delete failed", deleted.StatusCode)
	}
	if _, err = os.Stat(s.chatFileDir(conv.ID)); !os.IsNotExist(err) {
		t.Fatal("deleted conversation retained sent files", err)
	}
}
