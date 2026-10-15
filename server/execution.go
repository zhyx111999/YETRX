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
	"sync/atomic"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/agentexec"
	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/execution"
	"github.com/Autumn-27/artex/resourceexec"
	"github.com/Autumn-27/norma/agentcore"
	actool "github.com/Autumn-27/norma/tool"
)

var ErrTranscriptBusy = errors.New("agent transcript already has an active writer; retry after it finishes")

var executionState atomic.Pointer[config.ExecutionSettings]

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

// ConfigureExecution installs both execution paths after saved settings are read,
// before any agents or background work start. Failed OS setup prevents startup.
func ConfigureExecution(ctx context.Context, c config.ExecutionSettings) error {
	if !c.Enabled {
		if err := ConfigureResourceExecution(nil); err != nil {
			return err
		}
		agent.SetSessionFactory(nil)
		executionState.Store(&c)
		log.Print("[execution] process isolation explicitly disabled")
		return nil
	}
	if c.MaxResources < 1 || c.MaxResources > config.MaxExecutionConcurrency {
		return errors.New("execution requires valid helper capacity")
	}
	if err := c.Budget.Validate(); err != nil {
		return err
	}
	if err := execution.InitCgroup(); err != nil {
		return err
	}
	if err := execution.ConfigureBudget(processLimits(c.Budget.Total)); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	env := executionEnvironment(os.Environ())
	limits := processLimits(c.Budget.Agent)
	resourceLimits := processLimits(c.Budget.Helper)
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
	agent.SetSessionFactory(singleWriterSessionFactory(factory))
	executionState.Store(&c)
	log.Printf("[execution] aggregate memory=%dMiB cpu=%g pids=%d (website excluded)", c.Budget.Total.MemoryMiB, c.Budget.Total.CPUs, c.Budget.Total.Processes)
	log.Printf("[execution] no shared Agent count cap; agent memory=%dMiB cpu=%g pids=%d; resources=%d memory=%dMiB cpu=%g pids=%d", c.Budget.Agent.MemoryMiB, c.Budget.Agent.CPUs, c.Budget.Agent.Processes, c.MaxResources, c.Budget.Helper.MemoryMiB, c.Budget.Helper.CPUs, c.Budget.Helper.Processes)
	return nil
}

func processLimits(b config.ProcessBudget) execution.Limits {
	return execution.Limits{MemoryBytes: int64(b.MemoryMiB) << 20, CPUs: b.CPUs, Processes: b.Processes}
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

// Keep one live writer per transcript, without a shared Agent count limit.
func singleWriterSessionFactory(next agent.SessionFactory) agent.SessionFactory {
	var mu sync.Mutex
	writers := map[string]bool{}
	return func(ctx context.Context, opts agentcore.Options) (agent.RuntimeSession, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := ""
		if opts.Transcript != nil && opts.SessionID != "" {
			var err error
			key, err = filepath.Abs(opts.Transcript.MainPath(opts.SessionID))
			if err != nil {
				return nil, err
			}
		}
		mu.Lock()
		if key != "" && writers[key] {
			mu.Unlock()
			return nil, ErrTranscriptBusy
		}
		if key != "" {
			writers[key] = true
		}
		mu.Unlock()
		release := func() { mu.Lock(); delete(writers, key); mu.Unlock() }
		session, err := next(ctx, opts)
		if err != nil {
			var cleanup *execution.CleanupError
			if !errors.As(err, &cleanup) {
				release()
			}
			return nil, err
		}
		return &writerSession{RuntimeSession: session, release: release}, nil
	}
}

type writerSession struct {
	agent.RuntimeSession
	once     sync.Once
	release  func()
	closeErr error
}

func (s *writerSession) Close() error {
	s.once.Do(func() {
		s.closeErr = s.RuntimeSession.Close()
		// Keep the transcript reserved while cleanup has not proved its writer dead.
		if s.closeErr == nil {
			s.release()
		}
	})
	return s.closeErr
}
