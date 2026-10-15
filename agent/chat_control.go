package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	actool "github.com/Autumn-27/norma/tool"
)

// ChatControl is invocation-local: a cached ChatAgent can concurrently serve
// ordinary conversations and children without sharing controls or cwd.
//
// The three boundary callbacks are the whole contract between a running chat
// and its host. They take and return plain data only and never mention host
// types, so a session executor in another process can forward each one as an
// event (OnPrompt, OnResponseEnd) or request/reply (DrainMessages) over one
// ordered channel. All run synchronously on the goroutine that drives the
// model loop; none may wait for the model.
//
//   - DrainMessages runs before every provider request, streaming or not.
//     Empty means send the request. A non-empty batch cancels that request
//     before it is sent: the strings are joined into ONE user message appended
//     after the completed tool results, and the loop restarts on the same
//     session, so the tool-call/tool-result protocol is never split. Until the
//     OnPrompt that follows, the batch still belongs to the host. If the run
//     ends first (cancellation, budget, error) the batch was not delivered:
//     a parent conversation must keep it, a subagent may discard it.
//   - OnPrompt runs each time the session has accepted a user message into its
//     history and transcript and is about to run the loop: once for the
//     original message, then once after each drained batch. Once it returns
//     that message is irrevocably part of the conversation the model reads.
//   - OnResponseEnd runs after a model response that requests tool calls has
//     been recorded completely: all of its activity has been emitted and any
//     buffered text flushed. It is never called while text is streaming and
//     not for a final response without tool calls (the turn ends instead).
//     Tool results of that response may already have been emitted or still be
//     pending; the tools are never cancelled or re-run because of it.
//
// Order around one response:
//
//	DrainMessages -> request, streamed text, the response's activity
//	-> OnResponseEnd (only when it asked for tools; tools may still run)
//	-> tool results -> DrainMessages -> ...
type ChatControl struct {
	WorkingDirectory string
	Child            bool
	DrainMessages    func() []string
	OnPrompt         func()
	OnResponseEnd    func()
	RollbackOnError  bool
}
type chatControlKey struct{}

func WithChatControl(ctx context.Context, control ChatControl) context.Context {
	return context.WithValue(ctx, chatControlKey{}, control)
}
func chatControlFrom(ctx context.Context) ChatControl {
	control, _ := ctx.Value(chatControlKey{}).(ChatControl)
	return control
}

// IsChildChat reports whether ctx belongs to a child conversation (subagent).
// Host tool handlers use it as a second line of defence behind childTools.
func IsChildChat(ctx context.Context) bool { return chatControlFrom(ctx).Child }

// childDelegationTools is the only list of delegation entry points a child
// conversation may not use: subagents (conversation delegation) and spawn_task
// (exploration delegation). Schema filtering, deferred/global names and the
// permission deny rule are all derived from it. Exploration's internal
// Planner->Worker flow never runs under a child ChatControl and is unaffected.
var childDelegationTools = []string{"subagents", "subagents_control", "spawn_task"}

// childDisallowedTools returns a fresh deny-rule list for agentcore.Options.
func childDisallowedTools() []string { return slices.Clone(childDelegationTools) }

func childTools(tools []actool.CoreTool, def DeferredInfo) ([]actool.CoreTool, DeferredInfo) {
	blocked := func(name string) bool { return slices.Contains(childDelegationTools, name) }
	out := make([]actool.CoreTool, 0, len(tools))
	for _, t := range tools {
		if !blocked(t.Name()) {
			out = append(out, t)
		}
	}
	filter := func(names []string) []string {
		result := make([]string, 0, len(names))
		for _, n := range names {
			if !blocked(n) {
				result = append(result, n)
			}
		}
		return result
	}
	def.Deferred, def.GlobalNames = filter(def.Deferred), filter(def.GlobalNames)
	return out, def
}

// Thread failures restore the previous transcript leaf. The failed suffix is
// retained separately for audit; it is never resumed as thread history.
func transcriptRollback(path string) (func() error, error) {
	var size int64
	stat, err := os.Stat(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if stat != nil {
		size = stat.Size()
	}
	return func() error {
		f, err := os.OpenFile(path, os.O_RDWR, 0600)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil || st.Size() <= size {
			return err
		}
		archive, err := os.OpenFile(fmt.Sprintf("%s.failed-%d", path, time.Now().UnixNano()), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = io.Copy(archive, io.NewSectionReader(f, size, st.Size()-size))
		if err == nil {
			err = archive.Sync()
		}
		closeErr := archive.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if err = f.Truncate(size); err != nil {
			return err
		}
		return f.Sync()
	}, nil
}

func controlledWorkDir(base, session string, control ChatControl) (string, error) {
	path := filepath.Join(base, "sessions", session)
	if control.WorkingDirectory == "" {
		if err := os.MkdirAll(path, 0755); err != nil {
			return "", err
		}
	} else {
		path = control.WorkingDirectory
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("working directory is not a directory: %s", path)
	}
	return path, nil
}
