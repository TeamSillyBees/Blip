package agents

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
)

type hermesAdapter struct {
	config config.Config
	source config.Source
	ledger handoffs
}

func newHermes(c config.Config, s config.Source) Adapter {
	return &hermesAdapter{config: c, source: s, ledger: handoffs{config: c, source: s}}
}

// The shell bridge promotes session_id and args, but keeps outcome, native
// turn/request IDs and child identities under extra. Plugin kwargs are not the
// wire format and must never be parsed as a Claude-compatible payload.
type hermesHook struct {
	Name      string          `json:"hook_event_name"`
	SessionID string          `json:"session_id"`
	CWD       string          `json:"cwd"`
	Profile   string          `json:"profile"`
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	Extra     struct {
		TurnID         string `json:"turn_id"`
		ToolCallID     string `json:"tool_call_id"`
		RequestID      string `json:"request_id"`
		Kind           string `json:"kind"`
		Prompt         string `json:"prompt"`
		ChildSessionID string `json:"child_session_id"`
		Completed      *bool  `json:"completed"`
		Failed         *bool  `json:"failed"`
		Interrupted    *bool  `json:"interrupted"`
		ExitReason     string `json:"turn_exit_reason"`
	} `json:"extra"`
}

func decodeHermesHook(data []byte) (hermesHook, bool, error) {
	var h hermesHook
	if json.Unmarshal(data, &h) != nil || h.Name == "" {
		return h, false, fmt.Errorf("invalid Hermes shell hook JSON")
	}
	switch h.Name {
	case "on_session_end", "pre_tool_call", "post_tool_call", "on_human_input_request", "on_human_input_resolved", "subagent_start":
	default:
		return h, false, nil
	}
	if h.SessionID == "" {
		return h, false, fmt.Errorf("Hermes hook requires a native session_id")
	}
	return h, true, nil
}

func (a *hermesAdapter) hookEvent(h hermesHook, now time.Time) *event.Event {
	e := event.Event{SchemaVersion: 2, SourceID: a.source.ID, Agent: "hermes", Source: "hermes/shell-hooks", SessionID: h.SessionID, TurnID: h.Extra.TurnID, OccurredAt: now.UTC(), CWD: h.CWD, Project: harnessProject(h.CWD, "Hermes")}
	switch h.Name {
	case "on_human_input_request":
		e.Kind = event.InputRequested
		if h.Extra.Kind == "approval" {
			e.Kind = event.ApprovalRequested
		}
		e.RequestID = h.Extra.RequestID
		e.Detail = event.LimitBytes(h.Extra.Prompt, 2000)
	case "on_session_end":
		if h.Extra.TurnID == "" && h.Extra.Completed == nil && h.Extra.Failed == nil && h.Extra.Interrupted == nil && h.Extra.ExitReason == "" {
			return nil
		}
		if hermesInterrupted(h) {
			return nil
		}
		switch {
		case h.Extra.Failed != nil && *h.Extra.Failed:
			e.Kind, e.Reason = event.AttentionRequired, "error"
		case h.Extra.Completed != nil && *h.Extra.Completed:
			e.Kind = event.TurnCompleted
		default:
			e.Kind, e.Reason = event.AttentionRequired, "stopped"
		}
		if e.Kind == event.AttentionRequired {
			e.Detail = event.LimitBytes(h.Extra.ExitReason, 200)
		}
	default:
		return nil
	}
	identity := e.RequestID
	if identity == "" {
		identity = event.ID(h.Extra.Kind, h.Extra.Prompt)
	}
	e.ID = event.SourceEventID(a.source.ID, "hermes", string(e.Kind), h.SessionID, e.TurnID, identity)
	return &e
}

func hermesInterrupted(h hermesHook) bool {
	if h.Extra.Interrupted != nil && *h.Extra.Interrupted {
		return true
	}
	switch h.Extra.ExitReason {
	case "interrupted", "cancelled", "canceled", "user_interrupt":
		return true
	}
	return false
}

func (a *hermesAdapter) ParseHook(ctx context.Context, data []byte, now time.Time) ([]event.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	h, relevant, err := decodeHermesHook(data)
	if err != nil || !relevant {
		return nil, err
	}
	e := a.hookEvent(h, now)
	if e == nil {
		return nil, nil
	}
	return []event.Event{*e}, nil
}

func (a *hermesAdapter) ConsumeHook(ctx context.Context, data []byte, now time.Time, emit func(event.Event) error) error {
	h, relevant, err := decodeHermesHook(data)
	if err != nil || !relevant {
		return err
	}
	if h.Name == "subagent_start" {
		if h.Extra.ChildSessionID == "" || h.Extra.ChildSessionID == h.SessionID {
			return nil
		}
		return a.withSession(ctx, h.Extra.ChildSessionID, func(s *hermesSession) error { s.Child = true; return nil })
	}
	return a.withSession(ctx, h.SessionID, func(s *hermesSession) error {
		if s.Child {
			return nil
		}
		turn, native := s.TurnKey, s.NativeTurn
		if h.Extra.TurnID != "" {
			turn, native = "turn:"+h.Extra.TurnID, h.Extra.TurnID
		}
		if turn == "" {
			turn = s.legacyKey()
		}
		switch h.Name {
		case "pre_tool_call":
			if h.Extra.TurnID == "" && s.Ended {
				s.Sequence++
				turn = s.legacyKey()
			}
			s.TurnKey, s.NativeTurn, s.Ended = turn, native, false
			s.Questions = nil
			if h.ToolName == "clarify" {
				s.Questions = hermesQuestions(h.ToolInput)
			}
			if h.Extra.ToolCallID == "" {
				return nil
			}
			return a.ledger.resume(ctx, h.SessionID, turn, native, event.ID("tool", h.Extra.ToolCallID), false, now)
		case "on_human_input_request":
			e := a.hookEvent(h, now)
			e.TurnID = native
			var texts []string
			for _, question := range s.Questions {
				texts = append(texts, question.Text)
			}
			if h.Extra.Kind == "clarify" && strings.Join(texts, "\n") == h.Extra.Prompt {
				e.Questions = s.Questions
			}
			request := e.RequestID
			if request == "" {
				request = event.ID(turn, h.Extra.Kind, h.Extra.Prompt)
			}
			if s.Resolved[request] {
				return nil
			}
			e.ID = event.SourceEventID(a.source.ID, "hermes", "request", h.SessionID, request)
			if err := a.ledger.request(ctx, *e, turn, emit); err != nil {
				return err
			}
			if s.Pending == nil {
				s.Pending = map[string]hermesRequest{}
			}
			requestIdentity := e.RequestID
			if requestIdentity == "" {
				requestIdentity = "event:" + e.ID
			}
			s.Pending[request] = hermesRequest{TurnKey: turn, NativeTurn: native, RequestIdentity: requestIdentity}
			return nil
		case "on_human_input_resolved":
			request := h.Extra.RequestID
			if request == "" {
				request = event.ID(turn, h.Extra.Kind, h.Extra.Prompt)
			}
			if s.Resolved == nil {
				s.Resolved = map[string]bool{}
			}
			s.Resolved[request] = true
			if pending, exists := s.Pending[request]; exists {
				if err := a.ledger.resolveRequest(ctx, h.SessionID, pending.TurnKey, pending.NativeTurn, pending.RequestIdentity, true); err != nil {
					return err
				}
				delete(s.Pending, request)
			}
			return nil
		case "post_tool_call":
			// Native human-input-resolved, not arbitrary tool completion, closes a wait.
			return nil
		case "on_session_end":
			e := a.hookEvent(h, now)
			if e == nil {
				if hermesInterrupted(h) {
					s.Ended, s.LastEnd = true, now.UTC()
					return a.ledger.cancel(ctx, h.SessionID, turn, native, now)
				}
				return nil
			}
			if len(s.Pending) > 0 {
				return nil
			}
			// Very old finalizers without turn_id have only best-effort dedup.
			if native == "" && s.Ended && now.Sub(s.LastEnd) >= 2*time.Second {
				s.Sequence++
				turn = s.legacyKey()
			}
			e.TurnID = native
			if err := a.ledger.finish(ctx, *e, turn, emit); err != nil {
				return err
			}
			if s.NativeTurn == "" || s.NativeTurn == native || s.Ended {
				s.Ended, s.LastEnd = true, now.UTC()
				s.TurnKey, s.NativeTurn = turn, native
			}
			return nil
		}
		return nil
	})
}

func hermesQuestions(data []byte) []event.Question {
	var input struct {
		Questions []struct {
			Question string   `json:"question"`
			Choices  []string `json:"choices"`
		} `json:"questions"`
	}
	if json.Unmarshal(data, &input) != nil {
		return nil
	}
	questions := []event.Question{}
	for _, q := range input.Questions {
		if len(questions) >= 5 {
			break
		}
		question := event.Question{Text: event.LimitBytes(q.Question, 2000)}
		for _, choice := range q.Choices {
			if len(question.Options) < 20 {
				question.Options = append(question.Options, event.LimitBytes(choice, 300))
			}
		}
		questions = append(questions, question)
	}
	return questions
}

func (a *hermesAdapter) installer() yamlHookInstaller {
	return yamlHookInstaller{config: a.config, source: a.source, path: filepath.Join(a.source.Home, "config.yaml")}
}
func (a *hermesAdapter) Install(ctx context.Context, executable string) error {
	command, err := hermesCommand(executable, a.config.Path, a.source.ID, runtime.GOOS == "windows")
	if err != nil {
		return err
	}
	entries := []jsonHookEntry{}
	for _, name := range []string{"on_session_end", "pre_tool_call", "on_human_input_request", "on_human_input_resolved", "subagent_start"} {
		entries = append(entries, jsonHookEntry{Event: name, Hook: map[string]any{"command": command, "timeout": 5, "fail_closed": false}})
	}
	return a.installer().edit(ctx, entries, true)
}
func (a *hermesAdapter) Uninstall(ctx context.Context) error {
	return a.installer().edit(ctx, nil, false)
}
func (a *hermesAdapter) InstallationStatus() string { return a.installer().status() }
func (a *hermesAdapter) Capabilities() []Capability {
	return []Capability{
		{Event: "turn_completed", Support: "native", Evidence: "on_session_end extra.completed and turn_id", Validation: "Hermes source 4d043038; shell protocol fixtures"},
		{Event: "attention_required", Support: "native", Evidence: "on_session_end failed/turn_exit_reason; interrupted excluded", Validation: "ordinary process exit ignored; shell protocol fixtures"},
		{Event: "input_requested", Support: "native", Evidence: "on_human_input_request; clarify options from pre_tool_call", Validation: "native request identity required for precise dedup"},
		{Event: "approval_requested", Support: "native", Evidence: "on_human_input_request kind=approval, actual human wait", Validation: "pre_approval_request not used: may refer to automatic approval"},
	}
}

func hermesCommand(executable, configPath, id string, windows bool) (string, error) {
	values := []string{executable, configPath}
	for n, value := range values {
		absolute, err := filepath.Abs(value)
		if err != nil {
			return "", err
		}
		values[n] = absolute
		if strings.ContainsAny(absolute, "\x00\r\n\"") {
			return "", fmt.Errorf("Hermes hook paths contain unsupported quotes or control characters")
		}
	}
	quote := func(value string) string {
		if windows {
			return "\"" + value + "\""
		}
		return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
	}
	return quote(values[0]) + " --config " + quote(values[1]) + " hook --source " + id, nil
}
