package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/TeamSillyBees/Blip/internal/codex"
	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

type codexAdapter struct {
	config    config.Config
	source    config.Source
	collector codex.Collector
}

func newCodex(c config.Config, s config.Source) Adapter {
	c.CodexHome = s.Home
	c.StateDir = StateDir(c, s)
	return &codexAdapter{config: c, source: s, collector: codex.Collector{SessionsDir: filepath.Join(s.Home, "sessions"), StatePath: filepath.Join(c.StateDir, "rollout-cursors.json")}}
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

func (a *codexAdapter) Poll(ctx context.Context, emit func(event.Event) error) error {
	err := a.collector.Poll(ctx, func(e event.Event) error { return emit(a.normalize(e)) })
	if errors.Is(err, codex.ErrCheckpoint) {
		return errors.Join(ErrCheckpoint, err)
	}
	return err
}
func (a *codexAdapter) SessionTitle(ctx context.Context, id string) string {
	return codex.SessionTitle(ctx, a.source.Home, id)
}
func (a *codexAdapter) Install(ctx context.Context, executable string) error {
	id := a.source.ID
	if id == config.DefaultSourceID {
		id = ""
	}
	return codex.InstallSource(ctx, a.config, executable, id)
}
func (a *codexAdapter) Uninstall(ctx context.Context) error { return codex.Uninstall(ctx, a.config) }
func (a *codexAdapter) InstallationStatus() string {
	var manifest struct {
		CodexHome string   `json:"codex_home"`
		Commands  []string `json:"commands"`
	}
	err := store.ReadJSON(filepath.Join(a.config.StateDir, "installation.json"), &manifest)
	if errors.Is(err, os.ErrNotExist) {
		return "not_installed"
	}
	if err != nil || len(manifest.Commands) == 0 {
		return "invalid_manifest"
	}
	data, err := os.ReadFile(filepath.Join(a.source.Home, "hooks.json"))
	if err != nil {
		return "missing_configuration"
	}
	var root struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
				Windows string `json:"commandWindows"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if json.Unmarshal(data, &root) != nil {
		return "invalid_configuration"
	}
	owned := map[string]bool{}
	for _, command := range manifest.Commands {
		owned[command] = true
	}
	for _, name := range []string{"PreToolUse", "PermissionRequest"} {
		found := false
		for _, group := range root.Hooks[name] {
			for _, hook := range group.Hooks {
				if owned[hook.Command] && owned[hook.Windows] {
					found = true
				}
			}
		}
		if !found {
			return "missing_configuration"
		}
	}
	return "configured_unverified"
}
func (a *codexAdapter) ReadPaths() []ReadPath {
	return []ReadPath{{Path: filepath.Join(a.source.Home, "sessions"), Directory: true}, {Path: filepath.Join(a.source.Home, "session_index.jsonl"), Optional: true}}
}
func (a *codexAdapter) Capabilities() []Capability {
	return []Capability{
		{Event: "turn_completed", Support: "derived", Evidence: "Codex rollout task_complete with root session and cancellation checks", Validation: "fixtures; see docs/compatibility.md"},
		{Event: "input_requested", Support: "native", Evidence: "PreToolUse request_user_input(_async)", Validation: "fixtures; see docs/compatibility.md"},
		{Event: "approval_requested", Support: "native", Evidence: "PermissionRequest", Validation: "fixtures; delivery may follow approval"},
		{Event: "attention_required", Support: "unavailable", Evidence: "stop/error correlation not enabled", Validation: "not yet implemented"},
	}
}
