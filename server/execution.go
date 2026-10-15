package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/agentexec"
	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/execution"
	"github.com/Autumn-27/artex/resourceexec"
	"github.com/Autumn-27/norma/agentcore"
	actool "github.com/Autumn-27/norma/tool"
)

var ErrExecutionBusy = errors.New("agent execution capacity is busy; retry after a running session finishes")

const executionProbeCommand = "execution-probe"
const shellLocalKind = "artex.shell.v1"

// RunExecutionWorker is called before flags, configuration, stores or task
// recovery. Worker stdout belongs exclusively to the framed protocol.
func RunExecutionWorker(ctx context.Context, args []string) (bool, error) {
	if len(args) == 0 {
		return false, nil
	}
	switch args[0] {
	case agentexec.WorkerCommand:
		constructors := map[string]agentexec.LocalToolConstructor{}
		for kind, makeTool := range WorkerLocalTools() {
			constructors[kind] = agentexec.LocalToolConstructor(makeTool)
		}
		constructors[shellLocalKind] = func(_ context.Context, raw json.RawMessage) (actool.CoreTool, error) {
			var name string
			if err := json.Unmarshal(raw, &name); err != nil {
				return nil, err
			}
			for _, t := range actool.ShellSessionTools() {
				if t.Name() == name {
					return t, nil
				}
			}
			return nil, fmt.Errorf("unknown interactive shell tool %q", name)
		}
		return true, agentexec.ServeWorker(ctx, agentexec.WorkerConfig{LocalTools: constructors})
	case resourceexec.WorkerCommand:
		return true, ServeResourceWorker(ctx)
	case executionProbeCommand:
		peer := execution.NewPeer(os.Stdin, os.Stdout, func(_ context.Context, method string, _ json.RawMessage, _ func(json.RawMessage) error) (json.RawMessage, error) {
			if method != "ping" {
				return nil, fmt.Errorf("unknown probe method %q", method)
			}
			return json.Marshal(os.Getpid())
		})
		stop := context.AfterFunc(ctx, func() { _ = peer.Close() })
		defer stop()
		<-peer.Done()
		return true, nil
	}
	return false, nil
}

// ConfigureExecution installs both execution paths before any Server or Manager
// is created. A failed OS setup or worker handshake prevents startup.
func ConfigureExecution(ctx context.Context, c config.ExecutionSettings) error {
	if !c.Enabled {
		if err := ConfigureResourceExecution(nil); err != nil {
			return err
		}
		agent.SetSessionFactory(nil)
		log.Print("[execution] process isolation explicitly disabled")
		return nil
	}
	if c.MaxSessions <= 0 || c.MaxResources <= 0 || c.MemoryMiB <= 0 || c.ResourceMemoryMiB <= 0 || c.CPUs <= 0 || c.ResourceCPUs <= 0 || c.Processes <= 0 || c.ResourceProcesses <= 0 || c.TotalMemoryMiB <= 0 || c.TotalCPUs <= 0 || c.TotalProcesses <= 0 {
		return errors.New("execution requires positive capacity and CPU, memory and process limits")
	}
	if err := execution.InitCgroup(); err != nil {
		return err
	}
	if err := execution.ConfigureBudget(execution.Limits{MemoryBytes: int64(c.TotalMemoryMiB) << 20, CPUs: c.TotalCPUs, Processes: c.TotalProcesses}); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	env := executionEnvironment(os.Environ())
	limits := execution.Limits{MemoryBytes: int64(c.MemoryMiB) << 20, CPUs: c.CPUs, Processes: c.Processes}
	resourceLimits := execution.Limits{MemoryBytes: int64(c.ResourceMemoryMiB) << 20, CPUs: c.ResourceCPUs, Processes: c.ResourceProcesses}
	for _, limit := range []execution.Limits{limits, resourceLimits} {
		if err := probeExecution(ctx, exe, env, limit); err != nil {
			return fmt.Errorf("execution startup probe: %w", err)
		}
	}
	if err := ConfigureResourceExecution(&resourceexec.Config{Executable: exe, Env: env, Limits: resourceLimits, MCPLimits: resourceLimits, MaxWorkers: c.MaxResources}); err != nil {
		return err
	}
	factory := agentexec.NewFactory(agentexec.Config{
		Executable: exe, Env: env, Limits: limits,
		LocalTool: executionToolPlacement, OnLocalToolStart: recordLocalToolStart,
	})
	agent.SetSessionFactory(limitSessionFactory(factory, c.MaxSessions))
	log.Printf("[execution] aggregate memory=%dMiB cpu=%g pids=%d (website excluded)", c.TotalMemoryMiB, c.TotalCPUs, c.TotalProcesses)
	log.Printf("[execution] sessions=%d memory=%dMiB cpu=%g pids=%d; resources=%d memory=%dMiB cpu=%g pids=%d", c.MaxSessions, c.MemoryMiB, c.CPUs, c.Processes, c.MaxResources, c.ResourceMemoryMiB, c.ResourceCPUs, c.ResourceProcesses)
	return nil
}

func probeExecution(ctx context.Context, exe string, env []string, limits execution.Limits) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	p, err := execution.Start(ctx, exe, []string{executionProbeCommand}, env, limits, nil)
	if err != nil {
		return err
	}
	var pid int
	callErr := p.Peer.Call(ctx, "ping", nil, &pid)
	closeErr := p.Close()
	if callErr != nil {
		return callErr
	}
	if pid != p.PID() || pid == os.Getpid() {
		return errors.New("execution probe returned an invalid worker identity")
	}
	return closeErr
}

func executionToolPlacement(t actool.CoreTool) (*agentexec.LocalToolSpec, error) {
	kind, data, err := LocalTool(t)
	if err != nil {
		return nil, err
	}
	if kind != "" {
		return &agentexec.LocalToolSpec{Kind: kind, Data: data}, nil
	}
	for _, shell := range actool.ShellSessionTools() {
		if shell.Name() == t.Name() {
			raw, err := json.Marshal(t.Name())
			return &agentexec.LocalToolSpec{Kind: shellLocalKind, Data: raw}, err
		}
	}
	return agentexec.SDKLocalTool(t)
}

// Keep the execution environment, including PATH and tool/proxy settings, while
// removing server bootstrap credentials that workers do not consume.
func executionEnvironment(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "ARTEX_PG_DSN", "ARTEX_CONFIG", "ARTEX_MIGRATION_TEST_DSN":
			continue
		}
		out = append(out, entry)
	}
	return out
}

// Admission is nonblocking: an executing parent can synchronously delegate, so
// waiting here could deadlock a full pool. Existing per-conversation scheduling
// remains responsible for its own queue. Each transcript has one live writer.
func limitSessionFactory(next agent.SessionFactory, max int) agent.SessionFactory {
	var mu sync.Mutex
	active := 0
	writers := map[string]bool{}
	return func(ctx context.Context, opts agentcore.Options) (agent.RuntimeSession, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := ""
		if opts.Transcript != nil && opts.SessionID != "" {
			key, _ = filepath.Abs(opts.Transcript.MainPath(opts.SessionID))
		}
		mu.Lock()
		if active >= max || key != "" && writers[key] {
			mu.Unlock()
			return nil, ErrExecutionBusy
		}
		active++
		if key != "" {
			writers[key] = true
		}
		mu.Unlock()
		release := func() { mu.Lock(); active--; delete(writers, key); mu.Unlock() }
		session, err := next(ctx, opts)
		if err != nil {
			release()
			return nil, err
		}
		return &limitedSession{RuntimeSession: session, release: release}, nil
	}
}

type limitedSession struct {
	agent.RuntimeSession
	once     sync.Once
	release  func()
	closeErr error
}

func (s *limitedSession) Close() error {
	s.once.Do(func() {
		s.closeErr = s.RuntimeSession.Close()
		// Failed cleanup keeps capacity reserved; a still-live process must never
		// make room for overlapping work.
		if s.closeErr == nil {
			s.release()
		}
	})
	return s.closeErr
}
