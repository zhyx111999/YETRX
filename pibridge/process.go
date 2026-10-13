// Package pibridge connects the web conversation lifecycle to a persistent pi runtime.
package pibridge

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ProcessOptions are private runtime inputs. Credentials travel over stdin, never argv.
type ProcessOptions struct {
	NodeBinary    string
	Script        string
	Command       []string // optional executable and arguments, useful for alternate hosts
	CWD           string
	AgentDir      string
	SessionFile   string
	BaseURL       string
	APIKey        string
	Models        []Model
	ModelID       string
	ThinkingLevel string
	SystemPrompt  string
	Tools         []string
	History       []json.RawMessage
	OnEvent       func(json.RawMessage)
}

type wireResponse struct {
	Type   string          `json:"type"`
	ID     string          `json:"id"`
	OK     bool            `json:"ok"`
	Error  string          `json:"error"`
	Event  json.RawMessage `json:"event"`
	Tools  []string        `json:"tools"`
	Skills []string        `json:"skills"`
}

// Process owns one conversation's pi session, including background workflow work.
// It stays alive between prompts; Close is reserved for deletion or host shutdown.
type Process struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	writeMu   sync.Mutex
	mu        sync.Mutex
	pending   map[string]chan wireResponse
	err       error
	stderr    tailBuffer
	done      chan struct{}
	seq       atomic.Uint64
	busy      atomic.Bool
	closing   atomic.Bool
	closeOnce sync.Once
	onEvent   func(json.RawMessage)
	apiKey    string
	tools     []string
	skills    []string
}

func StartProcess(ctx context.Context, options ProcessOptions) (*Process, error) {
	if options.CWD == "" || options.AgentDir == "" || options.SessionFile == "" {
		return nil, errors.New("pi runtime requires workspace, agent directory and session file")
	}
	for _, dir := range []string{options.CWD, options.AgentDir, filepath.Dir(options.SessionFile)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("prepare pi runtime directory: %w", err)
		}
	}
	command := options.Command
	if len(command) == 0 {
		node := options.NodeBinary
		if node == "" {
			node = "node"
		}
		if options.Script == "" {
			return nil, errors.New("pi runtime script is not configured")
		}
		command = []string{node, options.Script}
	}
	cmd := exec.Command(command[0], command[1:]...)
	cmd.Dir = options.CWD
	cmd.Env = replaceEnv(os.Environ(), "PI_CODING_AGENT_DIR", options.AgentDir)
	cmd.Env = replaceEnv(cmd.Env, "NODE_USE_ENV_PROXY", "1")
	cmd.WaitDelay = 3 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	p := &Process{cmd: cmd, stdin: stdin, pending: make(map[string]chan wireResponse), done: make(chan struct{}), onEvent: options.OnEvent, apiKey: options.APIKey}
	cmd.Stderr = &p.stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("start pi runtime: %w", err)
	}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		p.readLoop(stdout)
	}()
	go func() {
		// Wait only after stdout has been drained, preserving final protocol records.
		<-readDone
		err := cmd.Wait()
		p.mu.Lock()
		if p.err == nil && err != nil {
			p.err = fmt.Errorf("pi runtime exited: %w", err)
		}
		for _, reply := range p.pending {
			reply <- wireResponse{Type: "response", Error: p.errorTextLocked()}
		}
		p.pending = make(map[string]chan wireResponse)
		p.mu.Unlock()
		p.busy.Store(false)
		close(p.done)
		if !p.closing.Load() && p.onEvent != nil {
			event, _ := json.Marshal(map[string]string{"type": "runtime_error", "error": p.Error().Error()})
			p.onEvent(event)
		}
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = p.Close()
		case <-p.done:
		}
	}()
	initCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	history := options.History
	if history == nil {
		history = []json.RawMessage{}
	}
	tools := options.Tools
	if tools == nil {
		tools = []string{}
	}
	response, err := p.request(initCtx, map[string]any{
		"type": "init", "cwd": options.CWD, "agentDir": options.AgentDir,
		"sessionFile": options.SessionFile, "baseUrl": options.BaseURL, "apiKey": options.APIKey,
		"models": options.Models, "modelId": options.ModelID, "thinkingLevel": options.ThinkingLevel,
		"history": history, "tools": tools, "systemPrompt": options.SystemPrompt,
	})
	if err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("initialize pi runtime: %w", err)
	}
	p.tools = append([]string(nil), response.Tools...)
	p.skills = append([]string(nil), response.Skills...)
	return p, nil
}

func (p *Process) readLoop(stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var record wireResponse
		if err := json.Unmarshal(line, &record); err != nil {
			p.fail(errors.New("pi runtime produced an invalid JSON protocol record"))
			return
		}
		if record.Type == "event" {
			var event struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(record.Event, &event); err != nil {
				p.fail(errors.New("pi runtime produced an invalid session event"))
				return
			}
			switch event.Type {
			case "agent_start":
				p.busy.Store(true)
			case "agent_settled":
				p.busy.Store(false)
			}
			if p.onEvent != nil {
				p.onEvent(record.Event)
			}
			continue
		}
		if record.Type == "ready" {
			record.OK = true
		}
		if record.ID != "" {
			p.mu.Lock()
			reply := p.pending[record.ID]
			delete(p.pending, record.ID)
			p.mu.Unlock()
			if reply != nil {
				reply <- record
			}
		}
	}
	if err := scanner.Err(); err != nil {
		p.fail(fmt.Errorf("read pi runtime: %w", err))
	}
}

func (p *Process) fail(err error) {
	p.mu.Lock()
	if p.err == nil {
		p.err = err
	}
	p.mu.Unlock()
	_ = p.cmd.Process.Kill()
}

func (p *Process) request(ctx context.Context, command map[string]any) (wireResponse, error) {
	if err := ctx.Err(); err != nil {
		return wireResponse{}, err
	}
	id := strconv.FormatUint(p.seq.Add(1), 10)
	command["id"] = id
	payload, err := json.Marshal(command)
	if err != nil {
		return wireResponse{}, err
	}
	reply := make(chan wireResponse, 1)
	p.mu.Lock()
	select {
	case <-p.done:
		p.mu.Unlock()
		return wireResponse{}, p.Error()
	default:
	}
	p.pending[id] = reply
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
	}()
	p.writeMu.Lock()
	_, err = p.stdin.Write(append(payload, '\n'))
	p.writeMu.Unlock()
	if err != nil {
		return wireResponse{}, fmt.Errorf("write pi command: %w", err)
	}
	var response wireResponse
	select {
	case response = <-reply:
	case <-p.done:
		// stdout is drained before done closes; a final reply can already be
		// buffered when both select cases become ready.
		select {
		case response = <-reply:
		default:
			return wireResponse{}, p.Error()
		}
	case <-ctx.Done():
		return wireResponse{}, ctx.Err()
	}
	if !response.OK {
		message := response.Error
		if message == "" {
			message = "pi command failed"
		}
		return response, errors.New(p.redact(message))
	}
	return response, nil
}

func (p *Process) Prompt(ctx context.Context, message string, systemPrompt ...string) error {
	command := map[string]any{"type": "prompt", "message": message}
	if len(systemPrompt) > 0 {
		command["systemPrompt"] = systemPrompt[0]
	}
	_, err := p.request(ctx, command)
	if ctx.Err() != nil {
		abortCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = p.Abort(abortCtx)
	}
	return err
}

func (p *Process) SetModel(ctx context.Context, modelID, thinking string) error {
	_, err := p.request(ctx, map[string]any{"type": "set_model", "modelId": modelID, "thinkingLevel": thinking})
	return err
}

func (p *Process) SetModels(ctx context.Context, models []Model, modelID, thinking string) error {
	_, err := p.request(ctx, map[string]any{"type": "set_models", "models": models, "modelId": modelID, "thinkingLevel": thinking})
	return err
}

func (p *Process) Abort(ctx context.Context) error {
	_, err := p.request(ctx, map[string]any{"type": "abort"})
	return err
}

func (p *Process) Busy() bool            { return p != nil && p.busy.Load() }
func (p *Process) Done() <-chan struct{} { return p.done }
func (p *Process) Tools() []string       { return append([]string(nil), p.tools...) }
func (p *Process) Skills() []string      { return append([]string(nil), p.skills...) }

func (p *Process) Close() error {
	p.closeOnce.Do(func() {
		p.closing.Store(true)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = p.request(ctx, map[string]any{"type": "shutdown"})
		_ = p.stdin.Close()
		select {
		case <-p.done:
		case <-ctx.Done():
			_ = p.cmd.Process.Kill()
			select {
			case <-p.done:
			case <-time.After(4 * time.Second):
			}
		}
	})
	return nil
}

func (p *Process) Error() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return errors.New(p.errorTextLocked())
}

func (p *Process) errorTextLocked() string {
	message := "pi runtime stopped"
	if p.err != nil {
		message = p.err.Error()
	}
	if diagnostic := strings.TrimSpace(p.stderr.String()); diagnostic != "" {
		message += ": " + diagnostic
	}
	return p.redact(message)
}

func (p *Process) redact(message string) string {
	if p.apiKey != "" {
		message = strings.ReplaceAll(message, p.apiKey, "[redacted]")
	}
	return message
}

func replaceEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, key+"=") {
			out = append(out, entry)
		}
	}
	return append(out, key+"="+value)
}

type tailBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (b *tailBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, data...)
	const limit = 8192
	if len(b.data) > limit {
		b.data = append([]byte(nil), b.data[len(b.data)-limit:]...)
	}
	return len(data), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}
