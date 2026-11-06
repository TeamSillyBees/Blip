package agents

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

func TestHookCheckpointFaultPausesSourceUntilExactStateRestored(t *testing.T) {
	a, _ := testHermes(t)
	ctx := context.Background()
	payload := `{"hook_event_name":"on_session_end","session_id":"broken","extra":{"turn_id":"t","completed":true}}`
	path := filepath.Join(StateDir(a.config, a.source), "sessions", event.ID("broken")+".json")
	if err := store.WriteFile(path, []byte("broken")); err != nil {
		t.Fatal(err)
	}
	if err := Capture(ctx, a.config, a.source.ID, strings.NewReader(payload)); !errors.Is(err, ErrCheckpoint) {
		t.Fatal(err)
	}
	status := Inspect(a.config, a.source)
	if status.State != "paused" || status.ErrorCode != "checkpoint_invalid" {
		t.Fatal(status)
	}
	other := strings.ReplaceAll(payload, "broken", "healthy")
	if err := Capture(ctx, a.config, a.source.ID, strings.NewReader(other)); !errors.Is(err, ErrCheckpoint) {
		t.Fatal("source did not pause", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := Capture(ctx, a.config, a.source.ID, strings.NewReader(other)); !errors.Is(err, ErrCheckpoint) {
		t.Fatal("deleted state accepted as recovery", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deleted checkpoint recreated")
	}
	if err := store.WriteJSON(path, hermesSession{Format: 1, SessionID: "broken", Sequence: 1}); err != nil {
		t.Fatal(err)
	}
	if err := Capture(ctx, a.config, a.source.ID, strings.NewReader(other)); err != nil {
		t.Fatal(err)
	}
	if status = Inspect(a.config, a.source); status.LastHookError != "" || status.ErrorCode != "" {
		t.Fatal(status)
	}
}

func TestHookDiagnosticPreservesLatestEventAndTimeout(t *testing.T) {
	c := sourceConfig(t)
	ctx := context.Background()
	now := time.Now()
	if err := saveHookDiagnostic(ctx, c, "one", HookDiagnostic{ReceivedAt: now, LastEventAt: &now}); err != nil {
		t.Fatal(err)
	}
	if err := saveHookDiagnostic(ctx, c, "one", HookDiagnostic{ReceivedAt: now.Add(time.Second), ErrorCode: "hook_failed"}); err != nil {
		t.Fatal(err)
	}
	if err := saveHookDiagnostic(ctx, c, "one", HookDiagnostic{ReceivedAt: now.Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	var got HookDiagnostic
	if err := store.ReadJSON(DiagnosticPath(c, "one"), &got); err != nil || got.ErrorCode != "hook_failed" || got.LastEventAt == nil || !got.LastEventAt.Equal(now) {
		t.Fatal(got, err)
	}
	reader, writer := io.Pipe()
	defer writer.Close()
	bounded, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := Capture(bounded, c, "two", reader); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err := store.ReadJSON(DiagnosticPath(c, "two"), &got); err != nil || got.ErrorCode != "hook_timeout" {
		t.Fatal(got, err)
	}
}
