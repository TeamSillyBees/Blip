package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/TeamSillyBees/Blip/internal/codex"
	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
)

type codexAdapter struct {
	config    config.Config
	source    config.Source
	collector codex.Collector
	ledger    handoffs
}

func newCodex(c config.Config, s config.Source) Adapter {
	ledger := handoffs{config: c, source: s}
	c.CodexHome = s.Home
	c.StateDir = StateDir(c, s)
	return &codexAdapter{config: c, source: s, ledger: ledger, collector: codex.Collector{SessionsDir: filepath.Join(s.Home, "sessions"), StatePath: filepath.Join(c.StateDir, "rollout-cursors.json")}}
}

func (a *codexAdapter) normalize(e event.Event) event.Event {
	if a.source.EventVersion == 1 {
		return e
	}
	e.SchemaVersion, e.SourceID = 2, a.source.ID
	if a.source.ID != config.DefaultSourceID {
		// Retain the parser's replay identity while namespacing different homes.
		e.ID = event.SourceEventID(a.source.ID, "codex", string(e.Kind), e.ID)
	}
	return e
}

func (a *codexAdapter) ParseHook(ctx context.Context, data []byte, now time.Time) ([]event.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e, err := codex.ParseHook(bytes.NewReader(data), now)
	if err != nil || e == nil {
		return nil, err
	}
	return []event.Event{a.normalize(*e)}, nil
}

func (a *codexAdapter) ConsumeHook(ctx context.Context, data []byte, now time.Time, emit func(event.Event) error) error {
	if a.source.EventVersion == 1 {
		events, err := a.ParseHook(ctx, data, now)
		if err != nil {
			return err
		}
		for _, e := range events {
			if err = emit(e); err != nil {
				return err
			}
		}
		return nil
	}
	var hook codex.Hook
	if json.Unmarshal(data, &hook) != nil {
		return fmt.Errorf("invalid Codex hook JSON")
	}
	switch hook.Name {
	case "Stop", "Interrupt", "UserPromptSubmit", "PreToolUse", "PostToolUse", "PermissionRequest":
	default:
		return nil
	}
	if hook.SessionID == "" || hook.TurnID == "" {
		return fmt.Errorf("Codex hook requires session_id and turn_id")
	}
	if hook.AgentID != "" || codex.IsSubagentTranscript(hook.TranscriptPath) {
		return nil
	}
	session, turn := hook.SessionID, hook.TurnID
	switch hook.Name {
	case "Stop":
		return a.ledger.stop(ctx, session, turn, turn, hook.CWD, now)
	case "Interrupt":
		return a.ledger.cancel(ctx, session, turn, turn, now)
	case "UserPromptSubmit":
		return a.ledger.resume(ctx, session, turn, turn, event.ID("prompt", hook.Prompt), true, now)
	case "PostToolUse":
		return a.ledger.resolveApproval(ctx, session, turn, turn, hook.ToolUseID)
	case "PreToolUse":
		if hook.ToolUseID != "" {
			if err := a.ledger.resume(ctx, session, turn, turn, event.ID("tool", hook.ToolUseID), false, now); err != nil {
				return err
			}
		}
	}
	events, err := a.ParseHook(ctx, data, now)
	if err != nil {
		return err
	}
	for _, e := range events {
		if err = a.ledger.request(ctx, e, turn, emit); err != nil {
			return err
		}
	}
	return nil
}

func (a *codexAdapter) Poll(ctx context.Context, emit func(event.Event) error) error {
	if a.source.EventVersion == 2 {
		a.collector.Observe = func(ctx context.Context, signal codex.Lifecycle) error {
			session, turn := signal.SessionID, signal.TurnID
			switch signal.Type {
			case "task_started", "turn_started":
				return a.ledger.resume(ctx, session, turn, turn, event.ID("rollout-start", signal.OccurredAt.Format(time.RFC3339Nano)), true, signal.OccurredAt)
			case "turn_aborted":
				return a.ledger.cancel(ctx, session, turn, turn, signal.OccurredAt)
			case "turn_failed", "task_failed":
				e := event.Event{SchemaVersion: 2, Agent: "codex", SourceID: a.source.ID, Source: "codex/rollout", Kind: event.AttentionRequired, Reason: "error", SessionID: session, TurnID: turn, OccurredAt: signal.OccurredAt, Project: event.Project(signal.CWD), CWD: signal.CWD, Detail: "本轮因异常停止，请查看原会话中的原因。"}
				e.ReceivedAt, e.SignalAt = time.Now().UTC(), signal.OccurredAt
				return a.ledger.finish(ctx, e, turn, emit)
			}
			return nil
		}
	}
	err := a.collector.Poll(ctx, func(e event.Event) error {
		e.ReceivedAt, e.SignalAt = time.Now().UTC(), e.OccurredAt
		e = a.normalize(e)
		if a.source.EventVersion == 2 {
			if e.Kind == event.InputRequested {
				return a.ledger.request(ctx, e, e.TurnID, emit)
			}
			return a.ledger.finish(ctx, e, e.TurnID, emit)
		}
		return emit(e)
	})
	if errors.Is(err, codex.ErrCheckpoint) {
		return errors.Join(checkpointFailed("codex-rollout", event.ID("codex-rollout"), ""), err)
	}
	if err != nil {
		return err
	}
	if a.source.EventVersion == 2 {
		return a.ledger.flush(ctx, time.Now(), emit)
	}
	return nil
}
func (a *codexAdapter) SessionTitle(ctx context.Context, id string) string {
	return codex.SessionTitle(ctx, a.source.Home, id)
}
func (a *codexAdapter) Install(ctx context.Context, executable string) error {
	id := a.source.ID
	if id == config.DefaultSourceID {
		id = ""
	}
	return codex.InstallSourceVersion(ctx, a.config, executable, id, a.source.EventVersion)
}
func (a *codexAdapter) Uninstall(ctx context.Context) error { return codex.Uninstall(ctx, a.config) }
func (a *codexAdapter) InstallationStatus() string {
	return codex.InstallationStatus(a.config, a.source.EventVersion)
}
func (a *codexAdapter) ReadPaths() []ReadPath {
	return []ReadPath{{Path: filepath.Join(a.source.Home, "sessions"), Directory: true}, {Path: filepath.Join(a.source.Home, "session_index.jsonl"), Optional: true}}
}
func (a *codexAdapter) Capabilities() []Capability {
	capabilities := []Capability{
		{Event: "turn_completed", Support: "derived", Evidence: "Codex rollout task_complete with root session and cancellation checks", Validation: "fixtures; see docs/compatibility.md"},
		{Event: "input_requested", Support: "derived", Evidence: "PreToolUse plus rollout request_user_input(_async), deduplicated by native call ID", Validation: "desktop rollout fixture; see docs/compatibility.md"},
		{Event: "approval_requested", Support: "native", Evidence: "PermissionRequest", Validation: "fixtures; delivery may follow approval"},
		{Event: "attention_required", Support: "unavailable", Evidence: "stop/error correlation not enabled", Validation: "not yet implemented"},
	}
	if a.source.EventVersion == 2 {
		capabilities[3] = Capability{Event: "attention_required", Support: "signal-only", Evidence: "Stop bounded to 1s; Interrupt suppression; rollout failure records", Validation: "protocol fixtures; cancellation requires observed signal"}
	}
	return capabilities
}
