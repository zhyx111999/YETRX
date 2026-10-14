package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

type conversationStreamProvider struct {
	chunks chan string
}

func (p *conversationStreamProvider) Complete(context.Context, llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return llm.Message{}, "", llm.Usage{}, fmt.Errorf("fixture requires streaming")
}

func (p *conversationStreamProvider) Stream(ctx context.Context, _ llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		for {
			select {
			case <-ctx.Done():
				yield(llm.StreamEvent{}, ctx.Err())
				return
			case text, ok := <-p.chunks:
				if !ok {
					if yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn", Usage: llm.Usage{OutputTokens: 5}}, nil) {
						yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
					}
					return
				}
				if !yield(llm.StreamEvent{Type: llm.SETextDelta, Text: text}, nil) {
					return
				}
			}
		}
	}
}

func openConversationStream(t *testing.T, url, token, lastID string) <-chan conversationStreamSnapshot {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if lastID != "" {
		req.Header.Set("Last-Event-ID", lastID)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || res.Header.Get("Content-Type") != "text/event-stream" || res.Header.Get("X-Accel-Buffering") != "no" {
		res.Body.Close()
		t.Fatalf("stream response: %d %v", res.StatusCode, res.Header)
	}
	t.Cleanup(func() { res.Body.Close() })
	frames := make(chan conversationStreamSnapshot, 64)
	go func() {
		defer close(frames)
		scan := bufio.NewScanner(res.Body)
		scan.Buffer(make([]byte, 4096), 2<<20)
		for scan.Scan() {
			data, ok := strings.CutPrefix(scan.Text(), "data: ")
			if !ok {
				continue
			}
			var frame conversationStreamSnapshot
			if json.Unmarshal([]byte(data), &frame) != nil {
				continue
			}
			select {
			case frames <- frame:
			case <-ctx.Done():
				return
			}
		}
	}()
	return frames
}

func awaitConversationFrame(t *testing.T, frames <-chan conversationStreamSnapshot, accept func(conversationStreamSnapshot) bool) conversationStreamSnapshot {
	t.Helper()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				t.Fatal("stream closed before the expected frame")
			}
			if accept(frame) {
				return frame
			}
		case <-timeout.C:
			t.Fatal("stream did not deliver the expected frame")
		}
	}
}

func TestConversationStreamDeliversPartialTextAndReconnectsWithoutDuplicatingTheAnswer(t *testing.T) {
	s, e, _, conv := newNativeWorkflowServer(t)
	provider := &conversationStreamProvider{chunks: make(chan string, 8)}
	s.chatAgent = agent.NewChatAgent(provider, "fixture", t.TempDir(), nil, 100000)
	s.jwtKey = []byte("conversation-stream-test-key-32-bytes")
	token, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/conversations/{id}/stream", s.pgConversationStream)
	host := httptest.NewServer(s.requireAuth(mux))
	t.Cleanup(host.Close)
	url := fmt.Sprintf("%s/api/conversations/%d/stream?since=0", host.URL, conv.ID)
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous stream: %d", res.StatusCode)
	}
	frames := openConversationStream(t, url, token, "")
	awaitConversationFrame(t, frames, func(f conversationStreamSnapshot) bool { return !f.Running })
	human, err := e.pg.AppendConvActivity(conv.ID, db.Activity{Kind: "user", Summary: "hello", Detail: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	key := s.convBusyKey(conv.ID)
	s.chatMu.Lock()
	s.chatBusy[key] = true
	s.chatMu.Unlock()
	s.runConversation(conv, "hello", key, false, "")
	provider.chunks <- "你"
	first := awaitConversationFrame(t, frames, func(f conversationStreamSnapshot) bool { return f.Draft != nil && f.Draft.Detail == "你" })
	if !first.Running || !first.Draft.Streaming || first.Draft.Seq >= 0 {
		t.Fatalf("invalid live draft: %+v", first)
	}
	rows, _, err := e.pg.ConvActivityList(conv.ID, 0, 100)
	if err != nil || len(rows) != 1 || rows[0].ID != human {
		t.Fatalf("partial tokens leaked into the durable ledger: %+v, %v", rows, err)
	}
	provider.chunks <- "好，**世界** 👋"
	second := awaitConversationFrame(t, frames, func(f conversationStreamSnapshot) bool {
		return f.Draft != nil && strings.Contains(f.Draft.Detail, "世界")
	})
	if second.Draft.Seq != first.Draft.Seq {
		t.Fatal("each fragment replaced the bubble identity")
	}
	reconnected := openConversationStream(t, url, token, fmt.Sprint(human))
	joined := awaitConversationFrame(t, reconnected, func(f conversationStreamSnapshot) bool { return f.Draft != nil })
	if joined.Draft.Detail != "你好，**世界** 👋" || len(joined.Items) != 0 {
		t.Fatalf("reconnect lost the prefix or replayed acknowledged history: %+v", joined)
	}
	other, err := e.pg.CreateConversation("agent", "other stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.pg.DeleteConversation(other.ID) })
	isolated, _, err := s.readConversationStream(other.ID, 0)
	if err != nil || isolated.Draft != nil || len(isolated.Items) != 0 {
		t.Fatalf("cross-conversation leak: %+v %v", isolated, err)
	}
	// Child activity must still wait for the parent's complete turn boundary.
	s.queueWorkflowDelivery(key, workflowDelivery{Activity: db.Activity{Kind: "text", Tool: "subagents", Detail: "QUEUED_CHILD", Summary: "QUEUED_CHILD"}})
	before, _, err := s.readConversationStream(conv.ID, human)
	if err != nil || len(before.Items) != 0 || before.Draft == nil {
		t.Fatalf("queued child interrupted the live answer: %+v %v", before, err)
	}
	close(provider.chunks)
	final := awaitConversationFrame(t, frames, func(f conversationStreamSnapshot) bool {
		for _, row := range f.Items {
			if row.Kind == "result" {
				return true
			}
		}
		return false
	})
	if final.Draft != nil {
		t.Fatal("completed answer and stale draft were displayed together")
	}
	for _, row := range final.Items {
		if row.Kind == "result" && (row.Detail != "你好，**世界** 👋" || row.Streaming) {
			t.Fatalf("final row lost its complete body: %+v", row)
		}
	}
	waitNativeWorkflow(t, func() bool { return !s.conversationStreamState(conv.ID).running })
	rows, _, err = deliveryActivityList(e.pg, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	answers, results, child := 0, -1, -1
	for i, row := range rows {
		if row.Kind == "result" {
			answers++
			results = i
		}
		if row.Detail == "QUEUED_CHILD" {
			child = i
		}
	}
	if answers != 1 || child <= results || s.conversationStreamState(conv.ID).stream != nil {
		t.Fatalf("ledger order or stream cleanup: answers=%d result=%d child=%d", answers, results, child)
	}
}

func TestConversationStreamStepBoundariesClearDraftsAndUsageKeepsThem(t *testing.T) {
	s, e, _, conv := newNativeWorkflowServer(t)
	stream, closeStream := s.beginConversationStream(conv.ID, "agent")
	defer closeStream()
	stream.delta("text", "\n\n")
	if s.conversationStreamState(conv.ID).draft != nil {
		t.Fatal("whitespace-only output created an empty bubble")
	}
	if err := stream.append(e.pg, conv.ID, db.Activity{Kind: "tool_use", Tool: "Read", Detail: "{}"}); err != nil {
		t.Fatal(err)
	}
	stream.delta("text", "new answer")
	if err := stream.append(e.pg, conv.ID, db.Activity{Kind: "usage"}); err != nil {
		t.Fatal(err)
	}
	state := s.conversationStreamState(conv.ID)
	if state.draft == nil || state.draft.Detail != "new answer" {
		t.Fatalf("tool boundary retained whitespace or usage cleared the answer: %+v", state.draft)
	}
	// The FK failure represents a step that could not be committed. Its draft
	// must still end at the same capture boundary before a later step starts.
	if err := stream.append(e.pg, -1, db.Activity{Kind: "text", Detail: "new answer"}); err == nil {
		t.Fatal("expected the missing conversation to reject the write")
	}
	if s.conversationStreamState(conv.ID).draft != nil {
		t.Fatal("failed persistence retained a stale draft")
	}
	stream.delta("text", "after failed write")
	if got := s.conversationStreamState(conv.ID).draft.Detail; got != "after failed write" {
		t.Fatalf("next step inherited stale text: %q", got)
	}
	if err := e.pg.DeleteConversation(conv.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.readConversationStream(conv.ID, 0); err == nil {
		t.Fatal("deleted conversation retained an unreadable durable cursor")
	}
}

func TestConversationStreamCancellationRetainsPartialTextAndClearsTheDraft(t *testing.T) {
	s, _, _, conv := newNativeWorkflowServer(t)
	provider := &conversationStreamProvider{chunks: make(chan string, 1)}
	s.chatAgent = agent.NewChatAgent(provider, "fixture", t.TempDir(), nil, 100000)
	key := s.convBusyKey(conv.ID)
	s.chatBusy[key] = true
	s.runConversation(conv, "cancel", key, false, "")
	provider.chunks <- "尚未完成的文字"
	waitNativeWorkflow(t, func() bool { return s.conversationStreamState(conv.ID).draft != nil })
	s.chatMu.Lock()
	cancel := s.chatCancel[key]
	s.chatMu.Unlock()
	cancel(agent.AbortChatStoppedByUser)
	waitNativeWorkflow(t, func() bool { return !s.conversationStreamState(conv.ID).running })
	frame, _, err := s.readConversationStream(conv.ID, 0)
	if err != nil || frame.Running || frame.Draft != nil {
		t.Fatalf("cancelled stream: %+v %v", frame, err)
	}
	partial, stopped := false, false
	for _, item := range frame.Items {
		partial = partial || item.Kind == "text" && item.Detail == "尚未完成的文字"
		stopped = stopped || item.Kind == "result" && strings.Contains(item.Detail, "停止")
	}
	if !partial || !stopped {
		t.Fatalf("cancelled answer audit was lost: %+v", frame.Items)
	}
}
