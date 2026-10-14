package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Autumn-27/norma/agentcore"
	actool "github.com/Autumn-27/norma/tool"
)

// ChatControl is invocation-local: a cached ChatAgent can concurrently serve
// ordinary conversations and children without sharing controls or cwd.
type ChatControl struct {
	WorkingDirectory string
	Child            bool
	DrainMessages    func() []string
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

func childTools(tools []actool.CoreTool, def DeferredInfo) ([]actool.CoreTool, DeferredInfo) {
	blocked := func(name string) bool { return name == "subagents" || name == "subagents_control" }
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

func controlledSession(opts agentcore.Options, control ChatControl) (*agentcore.Session, promptSession) {
	if control.DrainMessages == nil {
		s := agentcore.NewSession(opts)
		return s, s
	}
	steer := newSteeredSession(&opts, control.DrainMessages)
	return steer.session, steer
}
