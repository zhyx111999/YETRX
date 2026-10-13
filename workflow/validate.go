package workflow

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"
)

const maxLabelRunes = 64

// normalize validates a request and returns a private, clean copy of it with
// every state field reset: labels unique, dependencies known and acyclic.
func normalize(tasks []Task) ([]Task, error) {
	if len(tasks) == 0 || len(tasks) > MaxTasks {
		return nil, fmt.Errorf("%w: need 1..%d tasks, got %d", ErrInvalid, MaxTasks, len(tasks))
	}
	out := cloneTasks(tasks)
	seen := make(map[string]bool, len(out))
	for i := range out {
		t := &out[i]
		t.Label = strings.TrimSpace(t.Label)
		t.AgentKey = strings.TrimSpace(t.AgentKey)
		switch {
		case t.Label == "" || utf8.RuneCountInString(t.Label) > maxLabelRunes:
			return nil, fmt.Errorf("%w: task %d: label must be 1..%d characters", ErrInvalid, i+1, maxLabelRunes)
		case seen[t.Label]:
			return nil, fmt.Errorf("%w: duplicate label %q", ErrInvalid, t.Label)
		case strings.TrimSpace(t.Task) == "":
			return nil, fmt.Errorf("%w: task %q has an empty prompt", ErrInvalid, t.Label)
		}
		seen[t.Label] = true
		*t = Task{Label: t.Label, Task: t.Task, AgentKey: t.AgentKey,
			DependsOn: t.DependsOn, Status: Pending}
	}

	indegree := make(map[string]int, len(out))
	dependents := make(map[string][]string, len(out))
	for i := range out {
		t := &out[i]
		deps := make(map[string]bool, len(t.DependsOn))
		for j, d := range t.DependsOn {
			d = strings.TrimSpace(d)
			t.DependsOn[j] = d
			switch {
			case d == t.Label:
				return nil, fmt.Errorf("%w: task %q depends on itself", ErrInvalid, d)
			case !seen[d]:
				return nil, fmt.Errorf("%w: task %q depends on unknown task %q", ErrInvalid, t.Label, d)
			case deps[d]:
				return nil, fmt.Errorf("%w: task %q lists dependency %q twice", ErrInvalid, t.Label, d)
			}
			deps[d] = true
			dependents[d] = append(dependents[d], t.Label)
		}
		indegree[t.Label] = len(t.DependsOn)
	}

	// Kahn: whatever cannot be peeled off is on, or behind, a cycle.
	var queue []string
	for _, t := range out {
		if indegree[t.Label] == 0 {
			queue = append(queue, t.Label)
		}
	}
	for len(queue) > 0 {
		l := queue[0]
		queue = queue[1:]
		for _, d := range dependents[l] {
			if indegree[d]--; indegree[d] == 0 {
				queue = append(queue, d)
			}
		}
		delete(indegree, l)
	}
	if len(indegree) > 0 {
		return nil, fmt.Errorf("%w: dependency cycle among %s", ErrInvalid, strings.Join(slices.Sorted(maps.Keys(indegree)), ", "))
	}
	return out, nil
}
