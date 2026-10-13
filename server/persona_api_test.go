package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func openPersonaTestServer(t *testing.T) (*Server, *http.ServeMux) {
	t.Helper()
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = m.Close()
	})
	s := New(ctx, m, t.TempDir(), t.TempDir(), t.TempDir())
	mux := http.NewServeMux()
	s.RegisterPersonaRoutes(mux)
	return s, mux
}

func personaRequest(mux *http.ServeMux, method, path string, body string) *httptest.ResponseRecorder {
	var data *bytes.Reader
	if body == "" {
		data = bytes.NewReader(nil)
	} else {
		data = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(method, path, data)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestPersonaRoutesCRUDActivationAndDTO(t *testing.T) {
	s, mux := openPersonaTestServer(t)
	prefix := fmt.Sprintf("persona-api-%d", time.Now().UnixNano())
	t.Cleanup(func() { _, _ = s.m.pg.Exec(`DELETE FROM personas WHERE name LIKE $1`, prefix+"%") })

	prompt := "  原文 prompt\n第二行  "
	create := personaRequest(mux, http.MethodPost, "/api/personas", fmt.Sprintf(`{"name":"%s-one","prompt":%s,"api_key":"must-not-leak"}`, prefix, strconv.Quote(prompt)))
	if create.Code != http.StatusOK {
		t.Fatalf("create persona status=%d body=%s", create.Code, create.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil || created.ID <= 0 {
		t.Fatalf("create response=%s err=%v", create.Body.String(), err)
	}

	second := personaRequest(mux, http.MethodPost, "/api/personas", fmt.Sprintf(`{"name":"%s-two","prompt":"second"}`, prefix))
	if second.Code != http.StatusOK {
		t.Fatalf("create second persona status=%d body=%s", second.Code, second.Body.String())
	}
	var createdSecond struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(second.Body.Bytes(), &createdSecond); err != nil || createdSecond.ID <= 0 {
		t.Fatalf("second create response=%s err=%v", second.Body.String(), err)
	}

	duplicate := personaRequest(mux, http.MethodPost, "/api/personas", fmt.Sprintf(`{"name":"%s-one","prompt":"duplicate"}`, prefix))
	if duplicate.Code != http.StatusConflict {
		t.Fatalf("duplicate status=%d body=%s", duplicate.Code, duplicate.Body.String())
	}
	blankPrompt := personaRequest(mux, http.MethodPost, "/api/personas", fmt.Sprintf(`{"name":"%s-blank","prompt":" \n\t"}`, prefix))
	if blankPrompt.Code != http.StatusBadRequest {
		t.Fatalf("blank prompt status=%d body=%s", blankPrompt.Code, blankPrompt.Body.String())
	}

	list := personaRequest(mux, http.MethodGet, "/api/personas", "")
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), "must-not-leak") {
		t.Fatalf("list status=%d body=%s", list.Code, list.Body.String())
	}
	var listed struct {
		Personas []PersonaDTO `json:"personas"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	var first *PersonaDTO
	for i := range listed.Personas {
		if listed.Personas[i].ID == strconv.FormatInt(created.ID, 10) {
			first = &listed.Personas[i]
			break
		}
	}
	if first == nil || first.Prompt != prompt || first.IsActive {
		t.Fatalf("listed persona=%+v, want exact prompt and inactive", first)
	}

	updatedPrompt := "\nupdated\n"
	update := personaRequest(mux, http.MethodPost, "/api/personas", fmt.Sprintf(`{"id":%d,"name":"%s-one-renamed","prompt":%s}`, created.ID, prefix, strconv.Quote(updatedPrompt)))
	if update.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", update.Code, update.Body.String())
	}

	activate := personaRequest(mux, http.MethodPost, "/api/personas/active", fmt.Sprintf(`{"id":%d}`, created.ID))
	if activate.Code != http.StatusOK {
		t.Fatalf("activate status=%d body=%s", activate.Code, activate.Body.String())
	}

	deleteActive := personaRequest(mux, http.MethodDelete, "/api/personas/"+strconv.FormatInt(created.ID, 10), "")
	if deleteActive.Code != http.StatusOK {
		t.Fatalf("delete active status=%d body=%s", deleteActive.Code, deleteActive.Body.String())
	}
	var deleted struct {
		Deleted       int64 `json:"deleted"`
		ActiveCleared bool  `json:"active_cleared"`
	}
	if err := json.Unmarshal(deleteActive.Body.Bytes(), &deleted); err != nil || deleted.Deleted != created.ID || !deleted.ActiveCleared {
		t.Fatalf("delete active response=%s err=%v", deleteActive.Body.String(), err)
	}

	missing := personaRequest(mux, http.MethodDelete, "/api/personas/"+strconv.FormatInt(created.ID, 10), "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("deleting missing persona status=%d body=%s", missing.Code, missing.Body.String())
	}
}
