package agents

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/TeamSillyBees/Blip/internal/codex"
	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
	"github.com/gofrs/flock"
)

// Only a typed adapter fault can name a recoverable checkpoint. No arbitrary
// paths are accepted from input or from a persisted fault record.
type checkpointFault struct {
	Format    int    `json:"format"`
	Kind      string `json:"kind"`
	Key       string `json:"key"`
	SessionID string `json:"session_id,omitempty"`
}

func (f *checkpointFault) Error() string { return ErrCheckpoint.Error() }
func (f *checkpointFault) Unwrap() error { return ErrCheckpoint }
func faultPath(c config.Config, id string) string {
	return filepath.Join(c.StateDir, "sources", id, "checkpoint-fault.json")
}
func checkpointFailed(kind, key, session string) error {
	return &checkpointFault{Format: 1, Kind: kind, Key: key, SessionID: session}
}
func faultLock(ctx context.Context, c config.Config, id string) (*flock.Flock, error) {
	path := faultPath(c, id)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	lock := flock.New(path + ".lock")
	ok, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil || !ok {
		_ = lock.Close()
		if err == nil {
			err = context.DeadlineExceeded
		}
		return nil, err
	}
	return lock, nil
}

// RecordCheckpointFault persists adapter identity only, never a raw callback.
func RecordCheckpointFault(ctx context.Context, c config.Config, s config.Source, err error) error {
	var fault *checkpointFault
	if !errors.As(err, &fault) {
		return nil
	}
	lock, lockErr := faultLock(ctx, c, s.ID)
	if lockErr != nil {
		return lockErr
	}
	defer lock.Close()
	// Keep the first unresolved incident; a concurrent failure cannot hide it.
	if _, statErr := os.Stat(faultPath(c, s.ID)); statErr == nil {
		return nil
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return statErr
	}
	return store.WriteJSON(faultPath(c, s.ID), fault)
}

// CheckSourceCheckpoint pauses every callback for a source after a detected
// state fault. It clears the latch only after the exact checkpoint is restored
// and validates again. Deleting the checkpoint is never treated as recovery.
func CheckSourceCheckpoint(ctx context.Context, c config.Config, s config.Source) error {
	path := faultPath(c, s.ID)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	lock, err := faultLock(ctx, c, s.ID)
	if err != nil {
		return err
	}
	defer lock.Close()
	var f checkpointFault
	if err = store.ReadJSON(path, &f); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	_, keyErr := hex.DecodeString(f.Key)
	if err != nil || f.Format != 1 || len(f.Key) != 64 || keyErr != nil {
		return ErrCheckpoint
	}
	root := StateDir(c, s)
	valid := false
	switch f.Kind {
	case "antigravity-summaries":
		// A fault from the retired SQLite collector cannot block native hooks.
		valid = s.Harness == "antigravity" && f.Key == event.ID(f.Kind)
	case "codex-rollout":
		valid = s.Harness == "codex" && f.Key == event.ID("codex-rollout") && codex.ValidateCheckpoint(filepath.Join(root, "rollout-cursors.json")) == nil
	case "handoff":
		_, found, readErr := (handoffs{config: c, source: s}).read(f.Key)
		valid = found && readErr == nil
	case "hermes-session":
		var state hermesSession
		valid = s.Harness == "hermes" && f.SessionID != "" && f.Key == event.ID(f.SessionID) && store.ReadJSON(filepath.Join(root, "sessions", f.Key+".json"), &state) == nil && state.Format == 1 && state.SessionID == f.SessionID && state.Sequence > 0
	case "claude-prompt":
		var state legacyPromptState
		valid = s.Harness == "claude-code" && f.SessionID != "" && f.Key == event.ID(f.SessionID) && store.ReadJSON(filepath.Join(root, "legacy-prompts", f.Key+".json"), &state) == nil && state.Format == 1 && state.SessionID == f.SessionID && state.Sequence > 0
	}
	if !valid {
		return ErrCheckpoint
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return os.Remove(path)
}

func hasCheckpointFault(c config.Config, id string) bool {
	_, err := os.Stat(faultPath(c, id))
	return err == nil || !errors.Is(err, os.ErrNotExist)
}
