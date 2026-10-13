package workflow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func sampleRun(id string) Run {
	profile := int64(3)
	now := time.Now().UTC().Truncate(time.Millisecond)
	return Run{
		ID: id, OwnerSession: "conv-1", OwnerAgent: "main", Depth: 1, Status: Failed,
		CreatedAt: now, UpdatedAt: now, FinishedAt: now,
		Tasks: []Task{
			{Label: "a", Task: "x", AgentKey: "k", LLMProfileID: &profile, Status: Succeeded, Result: "结果 ✓", StartedAt: now, FinishedAt: now},
			{Label: "b", Task: "y", DependsOn: []string{"a"}, Status: Blocked, Error: "dep"},
		},
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := sampleRun("wf-0123456789abcdef")
	if err := save(dir, want); err != nil {
		t.Fatal(err)
	}
	// overwrite replaces the file in place
	want.Status = Succeeded
	if err := save(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := load(filepath.Join(dir, want.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip changed the run:\n got %+v\nwant %+v", got, want)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Errorf("save left %d files behind, want just the json", len(files))
	}
}

func TestSaveFailureLeavesOldFileAndNoTemp(t *testing.T) {
	dir := t.TempDir()
	r := sampleRun("wf-0123456789abcdef")
	if err := save(dir, r); err != nil {
		t.Fatal(err)
	}
	// A directory squatting on the target name makes the final rename fail.
	r2 := sampleRun("wf-fedcba9876543210")
	if err := os.Mkdir(filepath.Join(dir, r2.ID+".json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := save(dir, r2); err == nil {
		t.Fatal("expected save to fail")
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*.tmp"))
	if len(matches) != 0 {
		t.Errorf("failed save left temp files: %v", matches)
	}
	if _, err := load(filepath.Join(dir, r.ID+".json")); err != nil {
		t.Errorf("unrelated run damaged: %v", err)
	}
}

func TestLoadAllSkipsJunkAndCleansTemps(t *testing.T) {
	dir := t.TempDir()
	good := sampleRun("wf-0123456789abcdef")
	if err := save(dir, good); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("wf-aaaaaaaaaaaaaaaa.json", "{not json")             // corrupt
	other, _ := json.Marshal(sampleRun("wf-bbbbbbbbbbbbbbbb")) // id differs from the name
	write("wf-cccccccccccccccc.json", string(other))
	write("notes.json", "{}")                            // not ours
	write("wf-0123456789abcdef.1234.tmp", "half a file") // crash leftover

	runs, err := loadAll(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].ID != good.ID {
		t.Fatalf("loaded %+v, want only %s", runs, good.ID)
	}
	if _, err := os.Stat(filepath.Join(dir, "wf-0123456789abcdef.1234.tmp")); !os.IsNotExist(err) {
		t.Error("stale temp file was not removed")
	}
}

func TestNewIDShape(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		id := newID()
		if !idPattern.MatchString(id) || seen[id] {
			t.Fatalf("bad or repeated id %q", id)
		}
		seen[id] = true
	}
}
