package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Autumn-27/artex/db"
)

var conversationDraftID atomic.Int64

// Only the current uncommitted text lives here. Completed steps still go through
// AppendConvActivity once; readers never make the model wait on a network write.
type conversationStream struct {
	mu       sync.Mutex
	worker   string
	text     strings.Builder
	draft    ActivityDTO
	cursor   int64
	revision uint64
}

func (p *conversationStream) delta(kind, text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.draft.Kind != kind {
		p.text.Reset()
		p.draft = ActivityDTO{Seq: -conversationDraftID.Add(1), Worker: p.worker,
			Kind: kind, TS: rfc3339(time.Now()), Streaming: true}
	}
	p.text.WriteString(text)
	p.revision++
}

// Persisting the completed step and withdrawing its draft share the lock, so a
// snapshot cannot pair the newly committed row with the old streaming bubble.
func (p *conversationStream) append(pg *db.DB, id int64, activity db.Activity) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	err := p.record(func() (int64, error) { return pg.AppendConvActivity(id, activity) })
	// Capture flushes before every step except usage, including tool boundaries
	// after whitespace-only output. A failed write must not join the next step's
	// text onto the previous draft either.
	if activity.Kind != "usage" {
		p.text.Reset()
		p.draft = ActivityDTO{}
	}
	return err
}

// commit persists a row the model did not write (a background result card)
// through the same lock and cursor, without touching the draft. Hosts call it
// only at response boundaries, when no text is being generated.
func (p *conversationStream) commit(write func() (int64, error)) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.record(write)
}

// record runs one durable write and publishes it to followers; mu is held.
func (p *conversationStream) record(write func() (int64, error)) error {
	seq, err := write()
	if err == nil && seq > p.cursor {
		p.cursor = seq
	}
	p.revision++
	return err
}

type conversationStreamState struct {
	stream   *conversationStream
	cursor   int64
	revision uint64
	running  bool
	draft    *ActivityDTO
}

func (s *Server) conversationStreamState(id int64) conversationStreamState {
	s.chatMu.Lock()
	state := conversationStreamState{stream: s.chatStreams[id], running: s.chatBusy[s.convBusyKey(id)]}
	s.chatMu.Unlock()
	if p := state.stream; p != nil {
		p.mu.Lock()
		state.cursor, state.revision = p.cursor, p.revision
		if p.text.Len() != 0 {
			draft := p.draft
			draft.Detail = p.text.String()
			if strings.TrimSpace(draft.Detail) != "" {
				draft.Summary = firstLine(draft.Detail, 200)
				state.draft = &draft
			}
		}
		p.mu.Unlock()
	}
	return state
}

func (s *Server) beginConversationStream(id int64, worker string) (*conversationStream, func()) {
	p := &conversationStream{worker: worker}
	s.chatMu.Lock()
	if s.chatStreams == nil {
		s.chatStreams = make(map[int64]*conversationStream)
	}
	s.chatStreams[id] = p
	s.chatMu.Unlock()
	return p, func() {
		s.chatMu.Lock()
		if s.chatStreams[id] == p {
			delete(s.chatStreams, id)
		}
		s.chatMu.Unlock()
	}
}

type conversationStreamSnapshot struct {
	Items   []ActivityDTO `json:"items"`
	Cursor  int64         `json:"cursor"`
	Running bool          `json:"running"`
	Draft   *ActivityDTO  `json:"draft"`
}

// Reconcile the durable tail with the current draft. A turn may finish or hand
// off to a queued workflow reply while the DB read is in flight; read again if
// that boundary moved. Text deltas alone do not require another DB query.
func (s *Server) readConversationStream(id, since int64) (conversationStreamSnapshot, conversationStreamState, error) {
	out := conversationStreamSnapshot{Items: []ActivityDTO{}, Cursor: since}
	for {
		if err := s.ctx.Err(); err != nil {
			return out, conversationStreamState{}, err
		}
		before := s.conversationStreamState(id)
		items, cursor, err := s.m.pg.ConvActivityList(id, out.Cursor, 500)
		if err != nil {
			return out, before, err
		}
		for _, item := range items {
			// Answers arriving live must replace their draft with the full body,
			// not a truncated preview while a lazy detail request catches up.
			if item.Kind == "text" || item.Kind == "thinking" || item.Kind == "result" {
				item.Detail, err = s.m.pg.ConvActivityDetail(id, item.ID)
				if err != nil {
					return out, before, err
				}
			}
			out.Items = append(out.Items, activityDTO(item))
		}
		out.Cursor = cursor
		after := s.conversationStreamState(id)
		if len(items) == 0 && after.cursor > cursor {
			if c, err := s.m.pg.GetConversation(id); err != nil || c == nil {
				return out, after, fmt.Errorf("conversation %d was removed", id)
			}
		}
		if len(items) == 500 || before.stream != after.stream || before.running != after.running || after.cursor > cursor {
			continue
		}
		out.Running, out.Draft = after.running, after.draft
		return out, after, nil
	}
}

// pgConversationStream follows the same authenticated SSE protocol as task
// activity and side questions. A reconnect replays committed rows by cursor and
// sends the cumulative draft, so no fragment queue needs to grow for slow tabs.
func (s *Server) pgConversationStream(w http.ResponseWriter, r *http.Request) {
	_, c, ok := s.convByID(w, r)
	if !ok {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if n, err := strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64); err == nil {
		since = n
	}
	since = max(0, since)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	tick := time.NewTicker(40 * time.Millisecond)
	defer tick.Stop()
	var previous conversationStreamState
	var lastRead time.Time
	first := true
	for {
		state := s.conversationStreamState(c.ID)
		changed := first || state.stream != previous.stream || state.revision != previous.revision || state.running != previous.running
		frame := conversationStreamSnapshot{Items: []ActivityDTO{}, Cursor: since, Running: state.running, Draft: state.draft}
		// The periodic read also catches human messages, attachments and queued
		// background audit written by the existing conversation entry points.
		if first || state.cursor > since || state.stream != previous.stream || state.running != previous.running || time.Since(lastRead) >= time.Second {
			current, err := s.m.pg.GetConversation(c.ID)
			if err != nil {
				return
			}
			if current == nil {
				fmt.Fprint(w, "event: cleared\ndata: {}\n\n")
				flusher.Flush()
				return
			}
			frame, state, err = s.readConversationStream(c.ID, since)
			if err != nil {
				return // EventSource reconnects from its last confirmed durable cursor.
			}
			lastRead = time.Now()
			changed = true // also reconcile a fast reply that beat its send acknowledgement
		}
		if changed || len(frame.Items) != 0 {
			body, err := json.Marshal(frame)
			if err != nil {
				return
			}
			_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Second))
			if _, err := fmt.Fprintf(w, "id: %d\nevent: snapshot\ndata: %s\n\n", frame.Cursor, body); err != nil {
				return
			}
			flusher.Flush()
			since, previous, first = frame.Cursor, state, false
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.ctx.Done():
			return
		case <-tick.C:
		}
	}
}
