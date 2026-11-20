package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

type antigravityAdapter struct {
	config          config.Config
	source          config.Source
	registeredEvent string
}

func newAntigravity(c config.Config, s config.Source) Adapter {
	return &antigravityAdapter{config: c, source: s}
}

// Older clients omit hookEventName. Each installed command identifies its
// event independently of stdin and delegates to the same durable queue.
func CaptureAntigravity(ctx context.Context, c config.Config, id, name string, input io.Reader) error {
	return capture(ctx, c, id, input, Registry{"antigravity": func(c config.Config, s config.Source) Adapter {
		return &antigravityAdapter{config: c, source: s, registeredEvent: name}
	}})
}

type antigravityHook struct {
	Name          string   `json:"hookEventName"`
	Conversation  string   `json:"conversationId"`
	Artifacts     string   `json:"artifactDirectoryPath"`
	Transcript    string   `json:"transcriptPath"`
	CWD           string   `json:"cwd"`
	Workspaces    []string `json:"workspacePaths"`
	LastUserInput string   `json:"lastUserInput"`
	Invocation    *int     `json:"invocationNum"`
	InitialSteps  *int     `json:"initialNumSteps"`
	Execution     int      `json:"executionNum"`
	Step          *int     `json:"stepIdx"`
	FullyIdle     bool     `json:"fullyIdle"`
	Termination   string   `json:"terminationReason"`
	ToolCall      struct {
		ID   string          `json:"id"`
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"toolCall"`
}

// Both clients load the same global hooks.json. Match the data directory
// reported by the client before reading or writing any per-conversation state.
func (a *antigravityAdapter) ownsConversation(h antigravityHook) (bool, error) {
	if h.Conversation == "" || h.Conversation == "." || h.Conversation == ".." || strings.ContainsAny(h.Conversation, "/\\:\x00") {
		return false, fmt.Errorf("invalid Antigravity conversationId")
	}
	dir := h.Artifacts
	if dir == "" && h.Transcript != "" {
		logs := filepath.Dir(h.Transcript)
		if filepath.Base(logs) == "logs" && filepath.Base(filepath.Dir(logs)) == ".system_generated" {
			dir = filepath.Dir(filepath.Dir(logs))
		}
	}
	if relative, ok := strings.CutPrefix(filepath.ToSlash(dir), "~/"); ok {
		profile, err := os.UserHomeDir()
		if err != nil {
			return false, err
		}
		dir = filepath.Join(profile, filepath.FromSlash(relative))
	}
	if !filepath.IsAbs(dir) {
		return false, fmt.Errorf("Antigravity hook has no absolute conversation directory")
	}
	match := func(home string) bool {
		rel, err := filepath.Rel(filepath.Join(home, "brain", h.Conversation), dir)
		return err == nil && rel == "."
	}
	if match(a.source.Home) {
		return true, nil
	}
	// Resolve only the configured home. Never open a path supplied on stdin.
	home, err := config.CanonicalHome(a.source.Home)
	return err == nil && match(home), err
}

func (a *antigravityAdapter) ParseHook(ctx context.Context, data []byte, now time.Time) ([]event.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var h antigravityHook
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, fmt.Errorf("invalid Antigravity hook JSON: %w", err)
	}
	if h.Name == "" {
		h.Name = a.registeredEvent
	}
	if h.Name == "" || (a.registeredEvent != "" && h.Name != a.registeredEvent) {
		return nil, fmt.Errorf("unexpected Antigravity hook event")
	}
	owned, err := a.ownsConversation(h)
	if err != nil {
		return nil, err
	}
	if !owned {
		return nil, errForeignHook
	}
	if h.CWD == "" && len(h.Workspaces) > 0 {
		h.CWD = h.Workspaces[0]
	}
	e := event.Event{SchemaVersion: 2, Agent: "antigravity", Source: "antigravity/hooks", SourceID: a.source.ID,
		SessionID: h.Conversation, Project: harnessProject(h.CWD, "Antigravity"), CWD: h.CWD, OccurredAt: now.UTC()}
	marker := filepath.Join(StateDir(a.config, a.source), "invocations", event.ID(h.Conversation)+".json")
	switch h.Name {
	case "PreInvocation":
		// executionNum can be zero on every turn, even for identical user
		// prompts. The first invocation gives us the trajectory step count.
		if h.Invocation != nil && *h.Invocation == 0 && h.InitialSteps != nil && *h.InitialSteps >= 0 {
			return nil, store.WriteJSON(marker, *h.InitialSteps)
		}
		return nil, nil
	case "PreToolUse":
		return h.toolEvent(e)
	case "Stop":
		return h.stopEvent(ctx, e, marker, a.source.Home)
	default:
		return nil, nil
	}
}

func (h *antigravityHook) toolEvent(e event.Event) ([]event.Event, error) {
	switch h.ToolCall.Name {
	case "ask_question":
		e.Kind = event.InputRequested
		// The tool uses "question" for the text, unlike Blip's public "text".
		var input struct {
			Questions []struct {
				Question string   `json:"question"`
				Options  []string `json:"options"`
			} `json:"questions"`
		}
		if err := json.Unmarshal(h.ToolCall.Args, &input); err != nil || len(input.Questions) == 0 {
			return nil, fmt.Errorf("invalid ask_question arguments")
		}
		for _, q := range input.Questions[:min(20, len(input.Questions))] {
			question := event.Question{Text: event.LimitBytes(q.Question, 2000)}
			for _, option := range q.Options[:min(20, len(q.Options))] {
				question.Options = append(question.Options, event.LimitBytes(option, 300))
			}
			e.Questions = append(e.Questions, question)
		}
	case "ask_permission":
		e.Kind, e.Detail = event.ApprovalRequested, h.ToolCall.Name
	case "run_command":
		var command struct {
			BypassSandbox json.RawMessage `json:"BypassSandbox"`
			Summary       string          `json:"toolSummary"`
		}
		if len(h.ToolCall.Args) == 0 {
			return nil, nil
		}
		if err := json.Unmarshal(h.ToolCall.Args, &command); err != nil {
			return nil, fmt.Errorf("invalid run_command arguments: %w", err)
		}
		// Native hooks carry a boolean. Some clients serialize tool arguments as strings.
		if flag := string(command.BypassSandbox); flag != "true" && flag != `"true"` {
			return nil, nil
		}
		e.Kind, e.Detail = event.ApprovalRequested, "申请在沙箱外运行命令。"
		if summary := strings.TrimSpace(command.Summary); summary != "" {
			e.Detail += "\n" + event.LimitBytes(summary, 300)
		}
	default:
		return nil, nil
	}
	e.RequestID = h.ToolCall.ID
	request := e.RequestID
	if request == "" {
		if h.Step == nil || *h.Step < 0 {
			return nil, fmt.Errorf("Antigravity tool hook requires toolCall.id or stepIdx")
		}
		request = "step:" + strconv.Itoa(*h.Step)
	}
	e.ID = event.SourceEventID(e.SourceID, e.Agent, string(e.Kind), h.Conversation, request)
	return []event.Event{e}, nil
}

func (h *antigravityHook) stopEvent(ctx context.Context, e event.Event, marker, home string) ([]event.Event, error) {
	if !h.FullyIdle {
		return nil, nil
	}
	switch strings.TrimPrefix(strings.ToUpper(h.Termination), "EXECUTOR_TERMINATION_REASON_") {
	case "NO_TOOL_CALL", "MODEL_STOP":
		e.Kind = event.TurnCompleted
	case "ERROR":
		e.Kind, e.Reason = event.AttentionRequired, "error"
	case "MAX_STEPS_EXCEEDED", "MAX_INVOCATIONS", "MAX_FORCED_INVOCATIONS", "MAX_TOKEN_BUDGET_EXCEEDED", "TERMINAL_STEP_TYPE", "TERMINAL_CUSTOM_HOOK":
		e.Kind, e.Reason = event.AttentionRequired, "stopped"
	default:
		return nil, nil
	}
	var steps *int
	err := store.ReadJSON(marker, &steps)
	firstStep := 0
	turn := "execution:" + strconv.Itoa(h.Execution) // attached after the first invocation
	if err == nil && steps != nil && *steps >= 0 {
		firstStep = *steps
		turn = "steps:" + strconv.Itoa(*steps)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("invalid Antigravity invocation marker")
	}
	e.ID = event.SourceEventID(e.SourceID, e.Agent, "handoff", h.Conversation, turn, event.ID(h.LastUserInput))
	if e.Kind == event.TurnCompleted {
		e.Detail = antigravityFinalReply(ctx, home, h.Conversation, firstStep)
	}
	return []event.Event{e}, nil
}

func (*antigravityAdapter) Capabilities() []Capability {
	const validation = "Google Antigravity hook contract; agy CLI 1.5.9 on Windows"
	return []Capability{
		{Event: "turn_completed", Support: "native", Evidence: "fully idle Stop with normal termination and final reply from the conversation transcript when available", Validation: validation},
		{Event: "input_requested", Support: "native", Evidence: "PreToolUse ask_question with question text and options; in-tool MCP elicitation is not exposed", Validation: validation},
		{Event: "approval_requested", Support: "signal-only", Evidence: "PreToolUse ask_permission or run_command requesting BypassSandbox; client policy determines whether a prompt appears", Validation: "Antigravity App 2.33.0 command trace; agy CLI 1.5.9 PreToolUse before interactive approval"},
		{Event: "attention_required", Support: "native", Evidence: "fully idle Stop with error, limit or terminal reason", Validation: "Google Stop contract and termination reason fixtures"},
	}
}
