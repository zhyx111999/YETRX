package db

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func openPersonaTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func personaTestPrefix(t *testing.T) string {
	return fmt.Sprintf("persona-test-%s-%d", t.Name(), time.Now().UnixNano())
}

func TestPersonaSchemaAndCRUD(t *testing.T) {
	d := openPersonaTestDB(t)
	prefix := personaTestPrefix(t)
	previous, err := d.ActivePersona()
	if err != nil {
		t.Fatalf("read previous active persona: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM personas WHERE name LIKE $1`, prefix+"%")
		_, _ = d.Exec(`UPDATE personas SET is_active=false WHERE is_active`)
		if previous != nil {
			_ = d.SetActivePersona(previous.ID)
		}
	})

	// Reapplying the embedded schema exercises the idempotent persona migration.
	if _, err := d.Exec(schemaSQL); err != nil {
		t.Fatalf("reapply schema: %v", err)
	}

	firstPrompt := "  保留前后空白\n第二行原文  "
	firstID, err := d.SavePersona(&Persona{Name: prefix + "-one", Prompt: firstPrompt})
	if err != nil {
		t.Fatalf("insert first persona: %v", err)
	}
	secondID, err := d.SavePersona(&Persona{Name: prefix + "-two", Prompt: "第二个人格"})
	if err != nil {
		t.Fatalf("insert second persona: %v", err)
	}
	if firstID == secondID {
		t.Fatal("persona ids must be distinct")
	}

	got, err := d.PersonaByID(firstID)
	if err != nil {
		t.Fatalf("get persona: %v", err)
	}
	if got == nil || got.Prompt != firstPrompt || got.IsActive {
		t.Fatalf("inserted persona = %+v, want prompt preserved and inactive", got)
	}

	updatedPrompt := "\nupdated prompt\n"
	if id, err := d.SavePersona(&Persona{ID: firstID, Name: prefix + "-one-renamed", Prompt: updatedPrompt}); err != nil || id != firstID {
		t.Fatalf("update persona: id=%d err=%v", id, err)
	}
	got, err = d.PersonaByID(firstID)
	if err != nil || got == nil || got.Name != prefix+"-one-renamed" || got.Prompt != updatedPrompt {
		t.Fatalf("updated persona = %+v err=%v", got, err)
	}

	if _, err := d.SavePersona(&Persona{Name: prefix + "-two", Prompt: "duplicate"}); !errors.Is(err, ErrPersonaNameExists) {
		t.Fatalf("duplicate name error=%v, want ErrPersonaNameExists", err)
	}
	if _, err := d.SavePersona(&Persona{ID: 999999999, Name: prefix + "-missing", Prompt: "missing"}); !errors.Is(err, ErrPersonaNotFound) {
		t.Fatalf("missing update error=%v, want ErrPersonaNotFound", err)
	}

	personas, err := d.ListPersonas()
	if err != nil {
		t.Fatalf("list personas: %v", err)
	}
	seen := map[int64]*Persona{}
	for _, p := range personas {
		if len(p.Prompt) > 0 && (p.Prompt == firstPrompt || p.Prompt == updatedPrompt || p.Prompt == "第二个人格") {
			seen[p.ID] = p
		}
	}
	if len(seen) != 2 {
		t.Fatalf("list did not contain test personas: %+v", personas)
	}

	var activeTestCount int
	if err := d.QueryRow(`SELECT count(*) FROM personas WHERE name LIKE $1 AND is_active`, prefix+"%").Scan(&activeTestCount); err != nil {
		t.Fatal(err)
	}
	if activeTestCount != 0 {
		t.Fatalf("initial test active persona count=%d, want 0", activeTestCount)
	}
	if err := d.SetActivePersona(firstID); err != nil {
		t.Fatalf("activate first persona: %v", err)
	}
	active, err := d.ActivePersona()
	if err != nil || active == nil || active.ID != firstID {
		t.Fatalf("active first persona=%+v err=%v", active, err)
	}
	if err := d.SetActivePersona(secondID); err != nil {
		t.Fatalf("activate second persona: %v", err)
	}
	var activeCount int
	if err := d.QueryRow(`SELECT count(*) FROM personas WHERE is_active`).Scan(&activeCount); err != nil {
		t.Fatal(err)
	}
	if activeCount != 1 {
		t.Fatalf("active persona count=%d, want 1", activeCount)
	}

	cleared, err := d.DeletePersonaContext(t.Context(), secondID)
	if err != nil || !cleared {
		t.Fatalf("delete active persona: cleared=%v err=%v", cleared, err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM personas WHERE name LIKE $1 AND is_active`, prefix+"%").Scan(&activeTestCount); err != nil {
		t.Fatal(err)
	}
	if activeTestCount != 0 {
		t.Fatalf("after deleting test active persona count=%d, want 0", activeTestCount)
	}
	if err := d.SetActivePersona(999999999); !errors.Is(err, ErrPersonaNotFound) {
		t.Fatalf("missing activation error=%v, want ErrPersonaNotFound", err)
	}
}

func TestConcurrentPersonaActivationKeepsSingleActive(t *testing.T) {
	d := openPersonaTestDB(t)
	prefix := personaTestPrefix(t)
	previous, err := d.ActivePersona()
	if err != nil {
		t.Fatalf("read previous active persona: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM personas WHERE name LIKE $1`, prefix+"%")
		_, _ = d.Exec(`UPDATE personas SET is_active=false WHERE is_active`)
		if previous != nil {
			_ = d.SetActivePersona(previous.ID)
		}
	})

	ids := make([]int64, 2)
	for i := range ids {
		id, err := d.SavePersona(&Persona{Name: fmt.Sprintf("%s-%d", prefix, i), Prompt: "prompt"})
		if err != nil {
			t.Fatalf("insert persona %d: %v", i, err)
		}
		ids[i] = id
	}

	const attempts = 24
	start := make(chan struct{})
	errs := make(chan error, attempts)
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			<-start
			errs <- d.SetActivePersona(id)
		}(ids[i%len(ids)])
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent activation: %v", err)
		}
	}

	var activeCount int
	if err := d.QueryRow(`SELECT count(*) FROM personas WHERE is_active`).Scan(&activeCount); err != nil {
		t.Fatal(err)
	}
	if activeCount != 1 {
		t.Fatalf("active persona count=%d, want exactly 1", activeCount)
	}
	active, err := d.ActivePersona()
	if err != nil || active == nil {
		t.Fatalf("final active persona=%+v err=%v", active, err)
	}
	if active.ID != ids[0] && active.ID != ids[1] {
		t.Fatalf("final active id=%d, want one of %v", active.ID, ids)
	}
}
