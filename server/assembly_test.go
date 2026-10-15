package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

func hasSkillTool(tools []actool.CoreTool) bool {
	for _, tool := range tools {
		if tool.Name() == "Skill" {
			return true
		}
	}
	return false
}

func TestSharedResourcesAcrossEveryPreset(t *testing.T) {
	e := newNativeEnv(t)
	presets, err := e.pg.ListAgents()
	if err != nil {
		t.Fatal(err)
	}
	for _, preset := range presets {
		got := e.read(preset.Key)
		if !got.sdk || !got.skill || !got.mcp || got.custom == "" || got.system == "" {
			t.Errorf("%s lacks shared enabled resources: %+v", preset.Key, got)
		}
	}
	if err := e.pg.SetSkillEnabled(context.Background(), e.skill, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.pg.DeleteSkillSettings(context.Background(), e.skill) })
	for _, preset := range presets {
		if got := e.read(preset.Key); got.skill {
			t.Errorf("%s sees disabled skill", preset.Key)
		}
	}
	// Global choices do not erase or rewrite historical binding rows.
	for _, key := range e.keys {
		bound, err := e.pg.AgentSkillNames(e.ids[key])
		if err != nil || !contains(bound, e.skill) {
			t.Errorf("%s bindings changed: %v, %v", key, bound, err)
		}
	}
}

func TestSharedExecutionRolesUseTheSameCatalog(t *testing.T) {
	e := newNativeEnv(t)
	task, err := e.pg.CreateTaskWithOptions("shared resources", "verify assembly", db.TaskCreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.pg.DeleteTask(task.ID) })
	ts, assets := e.pg.Exploration(task.ExplorationID), e.pg.Assets()
	intentID, err := ts.AddNode("intent", map[string]any{"summary": "verify shared resources"}, 0, "running", "planner", nil)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := ts.GetNode(intentID)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"planner", "worker", "mainagent", "goals"} {
		t.Run(role, func(t *testing.T) {
			provider := &recordingProvider{callTool: e.custom}
			ctx, dir := context.Background(), t.TempDir()
			var runErr error
			switch role {
			case "planner":
				runner := agent.NewPlanner(provider, "fixture", dir, nil, 100000, 3)
				runner.SetNonStreaming(func() bool { return true })
				_, _, runErr = runner.Plan(ctx, task.ID, assets, ts, "verify assembly", nil, nil)
			case "worker":
				runner := agent.NewWorker(provider, "fixture", dir, nil, 100000, 3)
				runner.SetNonStreaming(func() bool { return true })
				_, _, runErr = runner.Execute(ctx, "fixture", task.ID, assets, ts, intent, nil, nil, nil, nil)
			case "mainagent":
				runner := agent.NewMainAgent(provider, "fixture", dir, nil, 100000, 3)
				runner.SetNonStreaming(func() bool { return true })
				_, runErr = runner.Chat(ctx, task.ID, 0, assets, ts, "verify assembly", "hello", nil, nil, nil, nil, nil)
			case "goals":
				agent.DecomposeGoalsWithProvider(ctx, provider, dir, "verify assembly", "", assets, ts, task.ID, true, 0, agent.WebSearchOpts{}, nil)
			}
			if runErr != nil {
				t.Fatal(runErr)
			}
			if len(provider.reqs) < 2 {
				t.Fatalf("no real shared tool round-trip: %d calls", len(provider.reqs))
			}
			available := map[string]bool{}
			for _, tool := range provider.reqs[0].Tools {
				available[tool.Name] = true
			}
			for _, key := range []string{"Skill", e.custom, "Read", "Bash", "Glob", "Grep", "MultiEdit", "record_fact", "list_goals"} {
				if !available[key] {
					t.Errorf("missing %s", key)
				}
			}
			if !strings.Contains(strings.Join(provider.reqs[0].System, "\n"), e.mcpTool) {
				t.Error("global MCP absent from deferred discovery")
			}
			if !strings.Contains(string(mustJSON(provider.reqs[1].Messages)), "nr-e2e") {
				t.Error("custom tool result did not reach next request")
			}
		})
	}
}

func TestSharedSkillSwitchHTTPPersists(t *testing.T) {
	e := newNativeEnv(t)
	s := &Server{m: &Manager{pg: e.pg}, skillDir: e.skillDir}
	for _, enabled := range []bool{false, true} {
		body, _ := json.Marshal(map[string]bool{"enabled": enabled})
		r := httptest.NewRequest("PUT", "/api/skills/"+e.skill+"/enabled", strings.NewReader(string(body)))
		r.SetPathValue("name", e.skill)
		w := httptest.NewRecorder()
		s.fsSetSkillEnabled(w, r)
		if w.Code != 200 {
			t.Fatalf("update: %d %s", w.Code, w.Body.String())
		}
		states, err := e.pg.SkillEnabledStates(context.Background())
		if err != nil || states[e.skill] != enabled {
			t.Fatalf("persisted state: %v, %v", states, err)
		}
		w = httptest.NewRecorder()
		s.fsListSkills(w, httptest.NewRequest("GET", "/api/skills", nil))
		var result struct {
			Skills []skillFileNode `json:"skills"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Skills) != 1 || result.Skills[0].Enabled != enabled {
			t.Fatalf("list state: %+v", result)
		}
		if got := e.both(t, "toggle"); got.skill != enabled {
			t.Fatalf("runtime state: %+v", got)
		}
	}
	t.Cleanup(func() { _ = e.pg.DeleteSkillSettings(context.Background(), e.skill) })
}

func TestSharedSkillDuplicatesNeverOverwrite(t *testing.T) {
	e := newNativeEnv(t)
	const duplicateDir = "duplicate-skill"
	folder := filepath.Join(e.skillDir, duplicateDir)
	if err := os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	data := "---\nname: " + e.skill + "\ndescription: collision\n---\nother instructions"
	if err := os.WriteFile(filepath.Join(folder, "SKILL.md"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	for _, key := range e.keys {
		if got := e.read(key); got.skill {
			t.Errorf("%s silently accepted a duplicate skill name", key)
		}
	}
	if err := e.pg.SetSkillEnabled(context.Background(), duplicateDir, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.pg.DeleteSkillSettings(context.Background(), duplicateDir) })
	if got := e.both(t, "duplicate disabled"); !got.skill || got.skillTxt != "skill v1" {
		t.Fatalf("enabled original lost: %+v", got)
	}
	combined, _, cleanup := agent.AugmentTools(context.Background(), "planner", nil)
	defer cleanup()
	if !hasSkillTool(combined) {
		t.Fatal("planner lacks unbound enabled skill")
	}
}
