package workflow

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var idPattern = regexp.MustCompile(`^wf-[0-9a-f]{16}$`)

func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return "wf-" + hex.EncodeToString(b[:])
}

// save replaces <dir>/<id>.json atomically: write a temp file in the same
// directory, flush it, then rename over the target, so a crash leaves either the
// old or the new file, never half of one.
func save(dir string, r Run) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, r.ID+".*.tmp")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(dir, r.ID+".json"))
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

// loadAll reads every run in dir and removes temp files left by a crash. A file
// that cannot be read or does not match its name is logged and skipped; it never
// blocks startup.
func loadAll(dir string) ([]Run, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var runs []Run
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".tmp") {
			os.Remove(filepath.Join(dir, name))
			continue
		}
		id, ok := strings.CutSuffix(name, ".json")
		if !ok || !idPattern.MatchString(id) {
			continue
		}
		r, err := load(filepath.Join(dir, name))
		if err == nil && r.ID != id {
			err = fmt.Errorf("id %q does not match file name", r.ID)
		}
		if err != nil {
			log.Printf("workflow: skip %s: %v", name, err)
			continue
		}
		runs = append(runs, r)
	}
	return runs, nil
}

func load(path string) (Run, error) {
	var r Run
	data, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(data, &r)
	}
	return r, err
}
