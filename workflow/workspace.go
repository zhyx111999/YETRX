package workflow

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Workspace describes an execution's source checkout. Retained trees are never
// recycled by resume; each new execution reserves a fresh path and branch.
type Workspace struct {
	CWD          string `json:"cwd"`
	RepoRoot     string `json:"repo_root,omitempty"`
	Branch       string `json:"branch,omitempty"`
	Isolated     bool   `json:"isolated"`
	Removed      bool   `json:"removed,omitempty"`
	CleanupError string `json:"cleanup_error,omitempty"`
}

func normalizeTaskDirectories(tasks []Task, base string) error {
	for i := range tasks {
		if tasks[i].CWD == "" {
			continue
		}
		path := tasks[i].CWD
		if !filepath.IsAbs(path) {
			path = filepath.Join(base, path)
		}
		cwd, err := canonicalDirectory(path)
		if err != nil {
			return fmt.Errorf("%w: task %q cwd: %v", ErrInvalid, tasks[i].Label, err)
		}
		tasks[i].CWD = cwd
	}
	return nil
}

func canonicalDirectory(path string) (string, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("%s is not a directory", path)
	}
	return path, nil
}

type cappedOutput struct{ bytes.Buffer }

func (b *cappedOutput) Write(p []byte) (int, error) {
	size := len(p)
	if remaining := 1024*1024 - b.Len(); remaining > 0 {
		_, _ = b.Buffer.Write(p[:min(remaining, size)])
	}
	return size, nil
}

// Every invocation has bounded output and a deadline. Platform-specific
// cancellation terminates descendants as well as Git itself.
func gitCommand(ctx context.Context, limit time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.WaitDelay = 2 * time.Second
	configureGitProcess(cmd)
	var out, stderr cappedOutput
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", fmt.Errorf("git interrupted: %w", ctx.Err())
	}
	if err != nil {
		return "", fmt.Errorf("git: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

var branchSlug = regexp.MustCompile(`[^a-z0-9]+`)

func createWorkspace(ctx context.Context, base string, t Task) (*Workspace, error) {
	if base == "" {
		base = "."
	}
	if t.CWD != "" {
		if filepath.IsAbs(t.CWD) {
			base = t.CWD
		} else {
			base = filepath.Join(base, t.CWD)
		}
	}
	cwd, err := canonicalDirectory(base)
	if err != nil {
		return nil, Permanent(err)
	}
	w := &Workspace{CWD: cwd}
	if t.Isolation == "" {
		return w, nil
	}
	root, err := gitCommand(ctx, 30*time.Second, "-C", cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, Permanent(fmt.Errorf("worktree isolation failed: %w", err))
	}
	parent := filepath.Join(root, ".yetrx", "worktrees")
	if err := os.MkdirAll(parent, 0700); err != nil {
		return nil, Permanent(err)
	}
	slug := strings.Trim(branchSlug.ReplaceAllString(strings.ToLower(t.Label), "-"), "-")
	if len(slug) > 32 {
		slug = slug[:32]
	}
	if slug == "" {
		slug = "agent"
	}
	for range 8 {
		var random [4]byte
		if _, err := rand.Read(random[:]); err != nil {
			return nil, Permanent(err)
		}
		id := hex.EncodeToString(random[:])
		w = &Workspace{CWD: filepath.Join(parent, id), RepoRoot: root, Branch: "yetrx/wf/" + slug + "-" + id, Isolated: true}
		if err := os.Mkdir(w.CWD, 0700); os.IsExist(err) {
			continue
		} else if err != nil {
			return nil, Permanent(err)
		}
		existing, err := gitCommand(ctx, 30*time.Second, "-C", root, "for-each-ref", "--format=%(refname)", "refs/heads/"+w.Branch)
		if err != nil || existing != "" {
			_ = os.Remove(w.CWD)
			if err != nil {
				return nil, Permanent(err)
			}
			continue
		}
		_, err = gitCommand(ctx, 30*time.Second, "-C", root, "worktree", "add", "-b", w.Branch, w.CWD, "HEAD")
		if err == nil {
			return w, nil
		}
		cleanupFailedWorkspace(w)
		return nil, Permanent(fmt.Errorf("worktree isolation failed: %w", err))
	}
	return nil, Permanent(fmt.Errorf("unable to reserve a fresh worktree"))
}

func cleanupFailedWorkspace(w *Workspace) {
	ctx := context.Background()
	_, _ = gitCommand(ctx, 5*time.Second, "-C", w.RepoRoot, "worktree", "remove", "--force", w.CWD)
	_ = os.RemoveAll(w.CWD) // only the empty path this execution atomically reserved
	_, _ = gitCommand(ctx, 5*time.Second, "-C", w.RepoRoot, "worktree", "remove", "--force", w.CWD)
	_, _ = gitCommand(ctx, 5*time.Second, "-C", w.RepoRoot, "branch", "-D", w.Branch)
}

func removeWorkspace(w *Workspace) {
	if w == nil || !w.Isolated || w.Removed {
		return
	}
	ctx := context.Background()
	if _, err := gitCommand(ctx, 30*time.Second, "-C", w.RepoRoot, "worktree", "remove", "--force", w.CWD); err != nil {
		w.CleanupError = err.Error()
		return
	}
	w.Removed = true
	if _, err := gitCommand(ctx, 30*time.Second, "-C", w.RepoRoot, "branch", "-D", w.Branch); err != nil {
		w.CleanupError = err.Error()
	}
}
