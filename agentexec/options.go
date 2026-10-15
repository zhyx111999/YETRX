package agentexec

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/tool"
)

// LocalToolSpec is supplied by trusted host configuration, never by model input.
// An absent spec means host execution. Kind "sdk" selects DefaultTools by name.
type LocalToolSpec struct {
	Kind string
	Data json.RawMessage
}

// The constructor context lasts for the worker process lifetime. Constructed
// tools receive the individual Prompt/tool deadline context in Call.
type LocalToolConstructor func(context.Context, json.RawMessage) (tool.CoreTool, error)
type toolSpec struct {
	Name, Description, Prompt string
	Schema                    map[string]any
	Raw                       bool
	Local                     *LocalToolSpec
}
type initRequest struct {
	Options       json.RawMessage
	Tools         []toolSpec
	TranscriptDir string
	Unlocked      []string
	TodoItems     []tool.Todo
	ObserveLocal  bool
	Todos         bool
	HasTranscript bool
	Unlock        bool
	Hooks         bool
	HookFirer     bool
	CanUse        bool
	Compactor     bool
	ContextView   bool
	Reminder      bool
	NewID         bool
	Warn          bool
}

// Every Options field must be explicitly classified. New SDK fields fail closed.
const plainFields = "SystemPrompt AppendSystemPrompt DynamicBoundary DeferredTools AllowedTools DisallowedTools PermissionMode Compaction EnableWebFetch WebFetchProxy WebFetchCACert WebFetchInsecureTLS EnableWebSearch WebSearchBackend BraveSearchAPIKey TavilySearchAPIKey DeepSeekSearchBaseURL DeepSeekSearchAPIKey DeepSeekSearchModel WebSearchProxy WebSearchCACert WebSearchInsecureTLS MaxTurns MaxDuration Settlement MaxTokens Temperature MaxConcurrency WorkingDir ToolOutputDir MaxToolOutputChars BashEnv SessionID TokenBudget EscalateMaxTokens DisableBackgroundTasks NonStreaming"
const brokerFields = "Provider Tools UnlockSet CanUseTool Hooks Compactor OnWarn Skills Todos AskUser SystemReminderFunc Transcript Deps"
const unsupportedFields = "Plan Memory"

func optionClasses() map[string]string {
	out := map[string]string{}
	for kind, fields := range map[string]string{"plain": plainFields, "broker": brokerFields, "unsupported": unsupportedFields} {
		for _, field := range strings.Fields(fields) {
			out[field] = kind
		}
	}
	return out
}
func prepare(opts agentcore.Options, local func(tool.CoreTool) (*LocalToolSpec, error)) (initRequest, map[string]tool.CoreTool, error) {
	var init initRequest
	plain := map[string]any{}
	value := reflect.ValueOf(opts)
	typ := value.Type()
	classes := optionClasses()
	var unsupported []string
	for i := 0; i < value.NumField(); i++ {
		name, field := typ.Field(i).Name, value.Field(i)
		switch classes[name] {
		case "plain":
			plain[name] = field.Interface()
		case "broker":
		case "unsupported":
			if !field.IsZero() {
				unsupported = append(unsupported, name)
			}
		default:
			unsupported = append(unsupported, name+" (unclassified SDK field)")
		}
	}
	depsType := reflect.TypeOf(opts.Deps)
	for i := 0; i < depsType.NumField(); i++ {
		name := depsType.Field(i).Name
		if name != "CallModel" && name != "CallModelSync" && name != "NewID" {
			unsupported = append(unsupported, "Deps."+name+" (unclassified SDK field)")
		}
	}
	if len(unsupported) > 0 {
		return init, nil, fmt.Errorf("agentexec: unsupported Options fields: %s", strings.Join(unsupported, ", "))
	}
	if opts.Provider == nil {
		return init, nil, fmt.Errorf("agentexec: Provider is required")
	}
	if opts.Transcript != nil {
		init.HasTranscript = true
		init.TranscriptDir = filepath.Dir(opts.Transcript.MainPath("agentexec-root-probe"))
	}
	init.Todos = opts.Todos != nil
	if init.Todos {
		init.TodoItems = opts.Todos.List()
	}
	init.Unlock = opts.UnlockSet != nil
	if init.Unlock {
		init.Unlocked = opts.UnlockSet.List()
	}
	init.Hooks = opts.Hooks != nil
	_, init.HookFirer = opts.Hooks.(hookFirer)
	init.CanUse, init.Compactor = opts.CanUseTool != nil, opts.Compactor != nil
	_, init.ContextView = opts.Compactor.(harness.ContextView)
	init.Reminder, init.NewID, init.Warn = opts.SystemReminderFunc != nil, opts.Deps.NewID != nil, opts.OnWarn != nil
	tools := append([]tool.CoreTool{}, opts.Tools...)
	if opts.Tools == nil {
		tools = tool.DefaultTools()
	}
	// These retain their original shared host stores and callbacks.
	if opts.Skills != nil {
		tools = append(tools, opts.Skills.Tool())
	}
	if opts.Todos != nil {
		tools = append(tools, opts.Todos.Tool())
	}
	if opts.AskUser != nil {
		tools = append(tools, tool.NewAskUserQuestion(opts.AskUser))
	}
	host := map[string]tool.CoreTool{}
	for _, t := range tools {
		spec := toolSpec{Name: t.Name(), Description: t.Description(), Prompt: t.Prompt(), Schema: t.InputSchema()}
		if raw, ok := t.(interface{ AcceptsRawInput() bool }); ok {
			spec.Raw = raw.AcceptsRawInput()
		}
		if local != nil {
			var err error
			spec.Local, err = local(t)
			if err != nil {
				return init, nil, err
			}
		}
		if spec.Local != nil && (t.Name() == "TodoWrite" || t.Name() == "Skill" || t.Name() == "AskUserQuestion") {
			return init, nil, fmt.Errorf("agentexec: %s must retain its host store/callback", t.Name())
		}
		init.Tools = append(init.Tools, spec)
		host[t.Name()] = t
	}
	var err error
	init.Options, err = json.Marshal(plain)
	return init, host, err
}

type hookFirer interface {
	FireSessionStart(context.Context)
	FireUserPromptSubmit(context.Context, string) string
}
