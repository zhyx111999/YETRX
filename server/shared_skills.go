package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/skill"
	"gopkg.in/yaml.v3"
)

// Read one directory independently so duplicate frontmatter names cannot be
// silently replaced by the SDK registry before we can validate the catalog.
func readSharedSkill(path string) (skill.Skill, error) {
	info, err := os.Stat(path)
	if err != nil {
		return skill.Skill{}, err
	}
	dir, name := filepath.Dir(path), strings.TrimSuffix(filepath.Base(path), ".md")
	if info.IsDir() {
		dir, name = path, filepath.Base(path)
		path = filepath.Join(path, "SKILL.md")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return skill.Skill{}, err
	}
	body := string(data)
	item := skill.Skill{Name: name, Dir: dir}
	if strings.HasPrefix(body, "---") {
		rest := strings.TrimLeft(strings.TrimPrefix(body, "---"), "\r\n")
		if index := strings.Index(rest, "\n---"); index >= 0 {
			var meta struct {
				Name          string `yaml:"name"`
				Description   string `yaml:"description"`
				License       string `yaml:"license"`
				Compatibility string `yaml:"compatibility"`
				WhenToUse     string `yaml:"whenToUse"`
				WhenToUseAlt  string `yaml:"when_to_use"`
				MCPs          any    `yaml:"mcps"`
				MCP           any    `yaml:"mcp"`
			}
			if err := yaml.Unmarshal([]byte(rest[:index]), &meta); err != nil {
				log.Printf("[skills] %s: invalid frontmatter: %v", dir, err)
				item.Instructions = strings.TrimSpace(strings.TrimLeft(rest[index+len("\n---"):], "-\r\n"))
				return item, nil
			}
			if meta.Name != "" {
				item.Name = meta.Name
			}
			item.Description, item.License, item.Compatibility = meta.Description, meta.License, meta.Compatibility
			item.WhenToUse = meta.WhenToUse
			if item.WhenToUse == "" {
				item.WhenToUse = meta.WhenToUseAlt
			}
			links := meta.MCPs
			if links == nil {
				links = meta.MCP
			}
			switch values := links.(type) {
			case string:
				for _, name := range strings.Split(strings.Trim(values, " []"), ",") {
					if name = strings.Trim(strings.TrimSpace(name), `"'`); name != "" {
						item.MCPs = append(item.MCPs, name)
					}
				}
			case []any:
				for _, value := range values {
					if name, ok := value.(string); ok && strings.TrimSpace(name) != "" {
						item.MCPs = append(item.MCPs, strings.TrimSpace(name))
					}
				}
			}
			body = strings.TrimLeft(rest[index+len("\n---"):], "-\r\n")
		}
	}
	item.Instructions = strings.TrimSpace(body)
	return item, nil
}

func loadSharedSkills(ctx context.Context, pg *db.DB, dir string) (*skill.Registry, error) {
	states, err := pg.SkillEnabledStates(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var items []skill.Skill
	names := map[string]int{}
	for _, entry := range entries {
		if !entry.IsDir() && !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		if enabled, explicit := states[entry.Name()]; explicit && !enabled {
			continue
		}
		item, err := readSharedSkill(filepath.Join(dir, entry.Name()))
		if err != nil {
			if !os.IsNotExist(err) {
				log.Printf("[skills] %s: %v", entry.Name(), err)
			}
			continue
		}
		names[item.Name]++
		items = append(items, item)
	}
	reg := skill.NewRegistry()
	for _, item := range items {
		if names[item.Name] > 1 {
			log.Printf("[skills] duplicate name %q in %s; skipped until renamed", item.Name, item.Dir)
			continue
		}
		reg.Add(item)
	}
	if len(reg.List()) == 0 {
		return nil, nil
	}
	return reg, nil
}

func (s *Server) fsSetSkillEnabled(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	name := r.PathValue("name")
	if !validSkillName(name) {
		writeErr(w, http.StatusBadRequest, "invalid skill name")
		return
	}
	if info, err := os.Stat(filepath.Join(s.skillDir, name, "SKILL.md")); err != nil || !info.Mode().IsRegular() {
		writeErr(w, http.StatusNotFound, "skill not found")
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil || body.Enabled == nil {
		writeErr(w, http.StatusBadRequest, "enabled is required")
		return
	}
	if err := pg.SetSkillEnabled(r.Context(), name, *body.Enabled); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"enabled": *body.Enabled})
}
