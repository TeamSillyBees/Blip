package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
)

type claudeAdapter struct {
	config config.Config
	source config.Source
	ledger handoffs
}

func newClaude(c config.Config, s config.Source) Adapter {
	return &claudeAdapter{config: c, source: s, ledger: handoffs{config: c, source: s}}
}

// prompt_id is a prompt identity, not a native turn ID. Keep it in adapter
// state instead of presenting an invented turn_id in the public event.
type claudeHook struct {
	Name           string          `json:"hook_event_name"`
	SessionID      string          `json:"session_id"`
	PromptID       string          `json:"prompt_id"`
	AgentID        string          `json:"agent_id"`
	Transcript     string          `json:"transcript_path"`
	CWD            string          `json:"cwd"`
	ToolName       string          `json:"tool_name"`
	ToolUseID      string          `json:"tool_use_id"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ElicitationID  string          `json:"elicitation_id"`
	Message        string          `json:"message"`
	Error          string          `json:"error"`
	Prompt         string          `json:"prompt"`
	StopHookActive bool            `json:"stop_hook_active"`
	LastMessage    string          `json:"last_assistant_message"`
}

func decodeClaudeHook(data []byte) (claudeHook, bool, error) {
	var h claudeHook
	if err := json.Unmarshal(data, &h); err != nil || h.Name == "" {
		return h, false, fmt.Errorf("invalid Claude Code hook JSON")
	}
	switch h.Name {
	case "UserPromptSubmit", "PreToolUse", "PostToolUse", "PostToolUseFailure", "PermissionRequest", "Stop", "StopFailure", "Elicitation", "ElicitationResult":
	default:
		return h, false, nil
	}
	if h.SessionID == "" {
		return h, false, fmt.Errorf("Claude Code hook requires session_id")
	}
	if h.AgentID != "" || strings.Contains(strings.ReplaceAll(h.Transcript, "\\", "/"), "/subagents/") {
		return h, false, nil
	}
	return h, true, nil
}

func (a *claudeAdapter) hookEvent(h claudeHook, turnKey string, now time.Time) *event.Event {
	e := event.Event{SchemaVersion: 2, SourceID: a.source.ID, Agent: "claude-code", Source: "claude-code/hooks", SessionID: h.SessionID, OccurredAt: now.UTC(), CWD: h.CWD, Project: harnessProject(h.CWD, "Claude Code")}
	switch h.Name {
	case "PermissionRequest":
		e.Kind, e.RequestID = event.ApprovalRequested, h.ToolUseID
		e.Detail = event.LimitBytes(h.ToolName, 200)
	case "PreToolUse":
		if h.ToolName != "AskUserQuestion" {
			return nil
		}
		e.Kind, e.RequestID = event.InputRequested, h.ToolUseID
		var input struct {
			Questions []struct {
				Question string `json:"question"`
				Options  []struct {
					Label string `json:"label"`
				} `json:"options"`
			} `json:"questions"`
		}
		if json.Unmarshal(h.ToolInput, &input) == nil {
			for _, q := range input.Questions {
				if len(e.Questions) >= 20 {
					break
				}
				question := event.Question{Text: event.LimitBytes(q.Question, 2000)}
				for _, o := range q.Options {
					if len(question.Options) < 20 {
						question.Options = append(question.Options, event.LimitBytes(o.Label, 300))
					}
				}
				e.Questions = append(e.Questions, question)
			}
		}
	case "Elicitation":
		e.Kind, e.RequestID = event.InputRequested, h.ElicitationID
		e.Detail = event.LimitBytes(h.Message, 2000)
	case "StopFailure":
		e.Kind, e.Reason = event.AttentionRequired, "error"
		e.Detail = event.LimitBytes(h.Error, 200)
	default:
		return nil
	}
	identity := e.RequestID
	if identity == "" {
		// Missing request IDs have content-level, per-prompt deduplication only.
		// Never expose this digest as a native request_id.
		identity = event.ID(h.Name, h.ToolName, string(h.ToolInput), h.Message)
	}
	e.ID = event.SourceEventID(a.source.ID, "claude-code", string(e.Kind), h.SessionID, turnKey, identity)
	return &e
}

func (a *claudeAdapter) ParseHook(ctx context.Context, data []byte, now time.Time) ([]event.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h, relevant, err := decodeClaudeHook(data)
	if err != nil || !relevant {
		return nil, err
	}
	e := a.hookEvent(h, "prompt:"+h.PromptID, now)
	if e == nil {
		return nil, nil
	}
	return []event.Event{*e}, nil
}

func (a *claudeAdapter) ConsumeHook(ctx context.Context, data []byte, now time.Time, emit func(event.Event) error) error {
	h, relevant, err := decodeClaudeHook(data)
	if err != nil || !relevant {
		return err
	}
	turn := "prompt:" + h.PromptID
	if h.PromptID == "" {
		turn, err = a.legacyTurn(ctx, h, now)
		if err != nil {
			return err
		}
	}
	switch h.Name {
	case "UserPromptSubmit":
		return a.ledger.resume(ctx, h.SessionID, turn, "", event.ID("prompt", turn), true, now)
	case "Stop":
		// A later Stop can itself prove a hook-induced continuation even when
		// the intervening model response called no tools.
		if h.StopHookActive && h.LastMessage != "" {
			if err := a.ledger.resume(ctx, h.SessionID, turn, "", event.ID("stop-continuation", h.LastMessage), false, now); err != nil {
				return err
			}
		}
		return a.ledger.stop(ctx, h.SessionID, turn, "", h.CWD, now)
	case "PostToolUse", "PostToolUseFailure":
		if h.ToolUseID == "" {
			return nil
		}
		if err := a.ledger.resolveRequest(ctx, h.SessionID, turn, "", h.ToolUseID, true); err != nil {
			return err
		}
		return a.ledger.resume(ctx, h.SessionID, turn, "", event.ID("resolved-tool", h.ToolUseID), false, now)
	case "ElicitationResult":
		if h.ElicitationID == "" {
			return nil
		}
		if err := a.ledger.resolveRequest(ctx, h.SessionID, turn, "", h.ElicitationID, true); err != nil {
			return err
		}
		return a.ledger.resume(ctx, h.SessionID, turn, "", event.ID("resolved-elicitation", h.ElicitationID), false, now)
	case "PreToolUse":
		if h.ToolUseID != "" {
			if err := a.ledger.resume(ctx, h.SessionID, turn, "", event.ID("tool", h.ToolUseID), false, now); err != nil {
				return err
			}
		}
	}
	e := a.hookEvent(h, turn, now)
	if e == nil {
		return nil
	}
	if e.Kind == event.AttentionRequired {
		return a.ledger.finish(ctx, *e, turn, emit)
	}
	return a.ledger.request(ctx, *e, turn, emit)
}

func (a *claudeAdapter) Poll(ctx context.Context, emit func(event.Event) error) error {
	return a.ledger.flush(ctx, time.Now(), emit)
}

func (a *claudeAdapter) installer() jsonHookInstaller {
	return jsonHookInstaller{config: a.config, source: a.source, path: filepath.Join(a.source.Home, "settings.json")}
}
func (a *claudeAdapter) Install(ctx context.Context, executable string) error {
	executable, err := filepath.Abs(executable)
	if err != nil {
		return err
	}
	configPath, err := filepath.Abs(a.config.Path)
	if err != nil {
		return err
	}
	for _, value := range []string{executable, configPath} {
		if strings.ContainsAny(value, "\x00\r\n") || strings.Contains(value, "${") {
			return fmt.Errorf("hook paths contain unsupported control characters or path placeholders")
		}
	}
	hook := map[string]any{"type": "command", "command": executable, "args": []string{"--config", configPath, "hook", "--source", a.source.ID}, "timeout": 5, "onFailure": "continue"}
	entries := []jsonHookEntry{}
	for _, name := range []string{"UserPromptSubmit", "PreToolUse", "PostToolUse", "PostToolUseFailure", "PermissionRequest", "Stop", "StopFailure", "Elicitation", "ElicitationResult"} {
		entries = append(entries, jsonHookEntry{Event: name, Hook: hook})
	}
	return a.installer().edit(ctx, entries, true)
}
func (a *claudeAdapter) Uninstall(ctx context.Context) error {
	return a.installer().edit(ctx, nil, false)
}
func (a *claudeAdapter) InstallationStatus() string { return a.installer().status() }
func (a *claudeAdapter) Capabilities() []Capability {
	return []Capability{
		{Event: "turn_completed", Support: "unavailable", Evidence: "Stop may be continued by other hooks; reported as a stop signal", Validation: "no final-result collector"},
		{Event: "attention_required", Support: "signal-only", Evidence: "Stop bounded to 1s; StopFailure reports API error; no Stop on user interrupt", Validation: "Stop: CLI 2.1.318 Windows with loopback model; API error: protocol fixtures"},
		{Event: "input_requested", Support: "native", Evidence: "AskUserQuestion and MCP Elicitation", Validation: "official protocol fixtures; request ID may be absent"},
		{Event: "approval_requested", Support: "native", Evidence: "PermissionRequest", Validation: "official protocol fixtures; request may already be handled at delivery"},
	}
}

func harnessProject(cwd, name string) string {
	if cwd == "" {
		return name
	}
	return event.Project(cwd)
}
