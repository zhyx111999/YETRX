package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/config"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/llmrec"
	"github.com/Autumn-27/artex/pibridge"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/transcript"
)

type piHumanTurnKey struct{}

type piConversation struct {
	ready          chan struct{}
	mu             sync.Mutex
	runMu          sync.Mutex
	process        *pibridge.Process
	relay          *pibridge.Relay
	err            error
	modelID        string
	thinking       string
	modelSignature string
	budgetMu       sync.Mutex
	turnsLeft      int
	cancelBudget   context.CancelCauseFunc
}

func (p *piConversation) currentProcess() *pibridge.Process {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.process
}

func (p *piConversation) close() {
	p.mu.Lock()
	process, relay := p.process, p.relay
	p.mu.Unlock()
	if process != nil {
		_ = process.Close()
	}
	if relay != nil {
		_ = relay.Close()
	}
}

// piProvider carries the same session attribution as native conversations while
// retaining the existing provider's credentials, recording and failover policy.
type piProvider struct {
	provider  llm.Provider
	sessionID string
}

func (p piProvider) context(ctx context.Context) context.Context {
	ctx = transcript.WithSessionID(ctx, p.sessionID)
	ctx = llmrec.WithWorker(ctx, "agent")
	return agent.WithRunInfo(ctx, agent.RunInfo{SessionID: p.sessionID})
}

func (p piProvider) Stream(ctx context.Context, request llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return p.provider.Stream(p.context(ctx), request)
}

func (p piProvider) Complete(ctx context.Context, request llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return p.provider.Complete(p.context(ctx), request)
}

func piModel(id, name, sessionID string, provider llm.Provider, cfg agent.Config) pibridge.Model {
	maxTokens := cfg.MaxTokens
	if maxTokens <= 0 {
		maxTokens = min(32768, cfg.CompactionWindow()/4)
	}
	return pibridge.Model{ID: id, Name: name, ContextWindow: cfg.CompactionWindow(), MaxTokens: maxTokens,
		NonStreaming: !cfg.Stream, Provider: piProvider{provider: provider, sessionID: sessionID}}
}

// piModels uses the same binding -> conversation pin -> global precedence as
// resolveChatAgent. Other configured profiles remain selectable by subagents.
func (s *Server) piModels(c *db.Conversation) ([]pibridge.Model, string, string, error) {
	sessionID := s.convBusyKey(c.ID)
	var models []pibridge.Model
	configs := map[string]agent.Config{}
	s.cfgMu.Lock()
	global, globalConfig, on := s.llmProv, s.llmCfg, s.llmOn
	s.cfgMu.Unlock()
	selected := ""
	if on && global != nil {
		models = append(models, piModel("global", "默认模型 · "+globalConfig.Model, sessionID, global, globalConfig))
		configs["global"] = globalConfig
		selected = "global"
	}
	profiles, err := s.m.pg.ListProfiles()
	if err != nil {
		return nil, "", "", fmt.Errorf("读取 Agent 模型配置: %w", err)
	}
	for _, profile := range profiles {
		provider, cfg, ok := s.providerForProfile(profile.ID)
		if !ok {
			continue
		}
		id := "profile-" + strconv.FormatInt(profile.ID, 10)
		provider = s.poolForBinding(profile.ID, provider, cfg)
		models = append(models, piModel(id, profile.Name+" · "+cfg.Model, sessionID, provider, cfg))
		configs[id] = cfg
	}
	if id := s.effectiveProfileForAgent(c.AgentKey, c.LLMProfileID); id != nil {
		candidate := "profile-" + strconv.FormatInt(*id, 10)
		if _, ok := configs[candidate]; ok {
			selected = candidate
		}
	}
	if selected == "" {
		return nil, "", "", errors.New(s.chatUnavailableReason())
	}
	cfg := configs[selected]
	thinking := "off"
	if cfg.ThinkingType != "disabled" {
		switch cfg.ReasoningEffort {
		case "minimal", "low", "medium", "high", "xhigh", "max":
			thinking = cfg.ReasoningEffort
		default:
			if cfg.ThinkingType == "enabled" {
				thinking = "medium"
			}
		}
	}
	return models, selected, thinking, nil
}

func piModelSignature(models []pibridge.Model) string {
	encoded, _ := json.Marshal(models) // Provider and NonStreaming are explicitly private.
	return string(encoded)
}

func piRuntimeScript() string {
	if path := strings.TrimSpace(os.Getenv("YETRX_PI_BRIDGE")); path != "" {
		absolute, err := filepath.Abs(path)
		if err == nil {
			return absolute
		}
		return path
	}
	absolute, _ := filepath.Abs(filepath.Join(config.BaseDir(), "runtime", "pi", "bridge.mjs"))
	return absolute
}

func (s *Server) piProcess(id int64) *pibridge.Process {
	s.piMu.Lock()
	entry := s.piSessions[id]
	s.piMu.Unlock()
	if entry == nil {
		return nil
	}
	return entry.currentProcess()
}

func (s *Server) piBusy(id int64) bool {
	process := s.piProcess(id)
	return process != nil && process.Busy()
}

func (s *Server) closePiConversation(id int64) {
	s.piMu.Lock()
	entry := s.piSessions[id]
	delete(s.piSessions, id)
	s.piMu.Unlock()
	if entry != nil {
		<-entry.ready
		entry.close()
	}
}

func (s *Server) closePiConversations() {
	s.piMu.Lock()
	entries := s.piSessions
	s.piSessions = make(map[int64]*piConversation)
	s.piMu.Unlock()
	var wait sync.WaitGroup
	for _, entry := range entries {
		wait.Add(1)
		go func(entry *piConversation) {
			defer wait.Done()
			<-entry.ready
			entry.close()
		}(entry)
	}
	wait.Wait()
}

func (s *Server) getPiConversation(ctx context.Context, c *db.Conversation, models []pibridge.Model, modelID, thinking string) (*piConversation, error) {
	s.piOnce.Do(func() {
		go func() {
			<-s.ctx.Done()
			s.closePiConversations()
		}()
	})
	s.piMu.Lock()
	if s.piSessions == nil {
		s.piSessions = make(map[int64]*piConversation)
	}
	if existing := s.piSessions[c.ID]; existing != nil {
		s.piMu.Unlock()
		select {
		case <-existing.ready:
			return existing, existing.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	entry := &piConversation{ready: make(chan struct{})}
	s.piSessions[c.ID] = entry
	s.piMu.Unlock()
	defer close(entry.ready)
	fail := func(err error) (*piConversation, error) {
		entry.err = err
		entry.close()
		s.piMu.Lock()
		if s.piSessions[c.ID] == entry {
			delete(s.piSessions, c.ID)
		}
		s.piMu.Unlock()
		return nil, err
	}
	if _, err := os.Stat(piRuntimeScript()); err != nil {
		return fail(errors.New("Agent 运行时尚未安装，请部署 runtime/pi 并安装其依赖"))
	}
	relay, err := pibridge.NewRelay(models)
	if err != nil {
		return fail(err)
	}
	entry.mu.Lock()
	entry.relay = relay
	entry.mu.Unlock()
	dataDir, err := filepath.Abs(s.m.dir)
	if err != nil {
		return fail(err)
	}
	sessionID := s.convBusyKey(c.ID)
	sessionFile := filepath.Join(dataDir, "pi", "sessions", sessionID+".jsonl")
	var history []json.RawMessage
	if _, err := os.Stat(sessionFile); errors.Is(err, os.ErrNotExist) {
		history, err = s.piConversationHistory(c.ID, ctx.Value(piHumanTurnKey{}) == true, modelID)
		if err != nil {
			return fail(fmt.Errorf("恢复既有对话历史: %w", err))
		}
	}
	recorder := pibridge.NewEventRecorder(func(activity db.Activity) {
		if _, err := s.m.pg.AppendConvActivity(c.ID, activity); err != nil {
			log.Printf("[pi conv %d] save activity: %v", c.ID, err)
		}
		_ = s.m.pg.TouchConversation(c.ID)
	})
	process, err := pibridge.StartProcess(s.ctx, pibridge.ProcessOptions{
		NodeBinary: os.Getenv("YETRX_PI_NODE"), Script: piRuntimeScript(),
		CWD:      filepath.Join(dataDir, "sessions", sessionID),
		AgentDir: filepath.Join(dataDir, "pi", "agents", sessionID), SessionFile: sessionFile,
		BaseURL: relay.BaseURL(), APIKey: relay.APIKey(), Models: models,
		ModelID: modelID, ThinkingLevel: thinking, History: history,
		SystemPrompt: agent.PiSystemPrompt(filepath.Join(dataDir, "sessions", sessionID)),
		Tools:        []string{"grep", "find", "ls"},
		OnEvent: func(event json.RawMessage) {
			recorder.Handle(event)
			var metadata struct {
				Type    string `json:"type"`
				Message struct {
					StopReason string `json:"stopReason"`
				} `json:"message"`
			}
			if json.Unmarshal(event, &metadata) == nil && metadata.Type == "turn_end" {
				entry.budgetMu.Lock()
				if entry.turnsLeft > 0 && entry.cancelBudget != nil {
					entry.turnsLeft--
					if entry.turnsLeft == 0 && metadata.Message.StopReason == "toolUse" {
						entry.cancelBudget(errors.New("已达到本轮步数上限"))
					}
				}
				entry.budgetMu.Unlock()
			}
		},
	})
	if err != nil {
		return fail(err)
	}
	entry.mu.Lock()
	entry.process = process
	entry.mu.Unlock()
	entry.modelID, entry.thinking, entry.modelSignature = modelID, thinking, piModelSignature(models)
	go func() {
		<-process.Done()
		_ = relay.Close()
		s.piMu.Lock()
		if s.piSessions[c.ID] == entry {
			delete(s.piSessions, c.ID)
		}
		s.piMu.Unlock()
	}()
	return entry, nil
}

func (s *Server) runPiConversation(ctx context.Context, c *db.Conversation, message string) error {
	models, modelID, thinking, err := s.piModels(c)
	if err != nil {
		return err
	}
	entry, err := s.getPiConversation(ctx, c, models, modelID, thinking)
	if err != nil {
		return err
	}
	entry.runMu.Lock()
	defer entry.runMu.Unlock()
	process := entry.currentProcess()
	// A workflow result may start a turn after HTTP admission. pi queues the
	// accepted prompt behind it, preserving the already-saved user message.
	if err := entry.relay.UpdateModels(models); err != nil {
		return err
	}
	if signature := piModelSignature(models); signature != entry.modelSignature {
		if err := process.SetModels(ctx, models, modelID, thinking); err != nil {
			return err
		}
		entry.modelSignature, entry.modelID, entry.thinking = signature, modelID, thinking
	} else if entry.modelID != modelID || entry.thinking != thinking {
		if err := process.SetModel(ctx, modelID, thinking); err != nil {
			return err
		}
		entry.modelID, entry.thinking = modelID, thinking
	}
	if seconds := s.agentRunSeconds(c.AgentKey); seconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
		defer cancel()
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	entry.budgetMu.Lock()
	entry.turnsLeft, entry.cancelBudget = s.agentMaxTurns(c.AgentKey), cancel
	entry.budgetMu.Unlock()
	defer func() {
		entry.budgetMu.Lock()
		entry.turnsLeft, entry.cancelBudget = 0, nil
		entry.budgetMu.Unlock()
	}()
	workspace, err := filepath.Abs(filepath.Join(s.m.dir, "sessions", s.convBusyKey(c.ID)))
	if err != nil {
		return err
	}
	if err := process.Prompt(runCtx, message, agent.PiSystemPrompt(workspace)); err != nil {
		if cause := context.Cause(runCtx); cause != nil {
			return cause
		}
		return err
	}
	return nil
}
