package workflow

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNormalizeRejects(t *testing.T) {
	cases := map[string][]Task{
		"no tasks":          nil,
		"too many":          independent(MaxTasks + 1),
		"empty label":       {{Label: "  ", Task: "x"}},
		"long label":        {{Label: strings.Repeat("长", 65), Task: "x"}},
		"duplicate label":   {task("a"), task("a")},
		"duplicate trimmed": {task("a"), {Label: " a ", Task: "x"}},
		"empty prompt":      {{Label: "a", Task: " \n"}},
		"unknown dep":       {task("a", "ghost")},
		"self dep":          {task("a", "a")},
		"repeated dep":      {task("a"), task("b", "a", "a")},
		"two-cycle":         {task("a", "b"), task("b", "a")},
		"cycle behind root": {task("r"), task("a", "r", "c"), task("b", "a"), task("c", "b")},
	}
	for name, tasks := range cases {
		if _, err := normalize(tasks); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestNormalizeAccepts(t *testing.T) {
	if _, err := normalize(independent(MaxTasks)); err != nil {
		t.Errorf("exactly %d tasks: %v", MaxTasks, err)
	}
	// diamond, listed with dependents before their dependencies
	got, err := normalize([]Task{task("d", "b", "c"), task("b", "a"), task("c", "a"), task("a")})
	if err != nil {
		t.Fatalf("diamond: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("len = %d", len(got))
	}
}

func TestNormalizeCopiesAndResetsState(t *testing.T) {
	id := int64(7)
	in := []Task{{
		Label: " a ", Task: "x", AgentKey: " k ", LLMProfileID: &id,
		Status: Succeeded, Result: "stale", Error: "stale",
	}, {Label: "b", Task: "y", DependsOn: []string{" a "}}}
	out, err := normalize(in)
	if err != nil {
		t.Fatal(err)
	}
	if out[0].Label != "a" || out[0].AgentKey != "k" || out[1].DependsOn[0] != "a" {
		t.Errorf("not trimmed: %+v", out)
	}
	if out[0].Status != Pending || out[0].Result != "" || out[0].Error != "" {
		t.Errorf("state not reset: %+v", out[0])
	}
	if in[0].Label != " a " || in[1].DependsOn[0] != " a " {
		t.Error("caller's slice was modified")
	}
	*out[0].LLMProfileID = 9
	if id != 7 {
		t.Error("LLMProfileID is shared with the caller")
	}
}

func TestStartValidatesOwnerAndDepth(t *testing.T) {
	e := open(t, t.TempDir(), 1, block(nil))
	for _, tc := range []struct {
		session string
		depth   int
		want    error
	}{
		{"", 0, ErrInvalid},
		{"conv-1", -1, ErrInvalid},
		{"conv-1", MaxDepth, ErrDepth},
		{"conv-1", MaxDepth + 5, ErrDepth},
	} {
		if _, err := e.m.Start(tc.session, "main", tc.depth, independent(1)); !errors.Is(err, tc.want) {
			t.Errorf("Start(%q, depth %d) = %v, want %v", tc.session, tc.depth, err, tc.want)
		}
	}
	if _, err := e.m.Start("conv-1", "main", MaxDepth-1, independent(1)); err != nil {
		t.Errorf("depth %d must be allowed: %v", MaxDepth-1, err)
	}
	if got := e.m.List("conv-1"); len(got) != 1 {
		t.Errorf("rejected requests left runs behind: %d", len(got))
	}
}

func TestStartAfterShutdown(t *testing.T) {
	e := open(t, t.TempDir(), 1, block(nil))
	e.close()
	if _, err := e.m.Start("conv-1", "main", 0, independent(1)); !errors.Is(err, ErrClosed) {
		t.Errorf("Start after shutdown = %v, want ErrClosed", err)
	}
	if _, err := New(context.Background(), t.TempDir(), 1, nil, nil); err == nil {
		t.Error("New accepted a nil Runner")
	}
}
