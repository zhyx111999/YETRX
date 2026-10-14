package workflow

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Mailbox belongs to exactly one attempt. Close discards instructions that did
// not reach a model boundary; they never leak into a retry or resumed task.
type Mailbox struct {
	mu      sync.Mutex
	pending []string
	closed  bool
}

func (b *Mailbox) Drain() []string {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := b.pending
	b.pending = nil
	return out
}
func (b *Mailbox) enqueue(text string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return false
	}
	b.pending = append(b.pending, text)
	return true
}
func (b *Mailbox) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	b.pending = nil
}

type Instruction struct {
	Text     string    `json:"text"`
	Attempt  int       `json:"attempt"`
	QueuedAt time.Time `json:"queued_at"`
}

func (m *Manager) Message(owner, id, label, text string) error {
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("%w: text is required", ErrInvalid)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, err := m.owned(owner, id)
	if err != nil {
		return err
	}
	i, ok := e.idx[label]
	if !ok {
		return ErrNoTask
	}
	box := e.mailboxes[label]
	if e.stopping || e.pausing || e.ctx == nil || e.ctx.Err() != nil || box == nil {
		return ErrNotRunning
	}
	t := &e.run.Tasks[i]
	before := len(t.Messages)
	t.Messages = append(t.Messages, Instruction{Text: text, Attempt: t.Attempts, QueuedAt: time.Now()})
	if err := save(m.dir, e.run); err != nil {
		t.Messages = t.Messages[:before]
		return err
	}
	if !box.enqueue(text) {
		t.Messages = t.Messages[:before]
		m.persist(e)
		return ErrNotRunning
	}
	return nil
}
