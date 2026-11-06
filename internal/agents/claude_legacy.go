package agents

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
	"github.com/gofrs/flock"
)

type legacyPromptState struct {
	Format       int       `json:"format"`
	SessionID    string    `json:"session_id"`
	Sequence     uint64    `json:"sequence"`
	PromptDigest string    `json:"prompt_digest,omitempty"`
	PromptAt     time.Time `json:"prompt_at,omitempty"`
}

// Older payloads lack prompt_id. Advance a durable sequence on observed user
// prompts, merging identical prompt callbacks for two seconds only. This is
// explicitly best effort, not a fabricated native turn or exact replay ID.
func (a *claudeAdapter) legacyTurn(ctx context.Context, hook claudeHook, now time.Time) (string, error) {
	dir := filepath.Join(StateDir(a.config, a.source), "legacy-prompts")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, event.ID(hook.SessionID)+".json")
	lock := flock.New(path + ".lock")
	ok, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil || !ok {
		_ = lock.Close()
		if err == nil {
			err = context.DeadlineExceeded
		}
		return "", err
	}
	defer lock.Close()
	state := legacyPromptState{Format: 1, SessionID: hook.SessionID, Sequence: 1}
	err = store.ReadJSON(path, &state)
	missing := errors.Is(err, os.ErrNotExist)
	if (err != nil && !missing) || state.Format != 1 || state.SessionID != hook.SessionID || state.Sequence == 0 {
		return "", ErrCheckpoint
	}
	changed := missing
	if hook.Name == "UserPromptSubmit" {
		digest := event.ID(hook.Prompt)
		if state.PromptDigest != digest || now.Sub(state.PromptAt) >= 2*time.Second {
			if !missing {
				state.Sequence++
			}
			state.PromptDigest, state.PromptAt = digest, now.UTC()
			changed = true
		}
	}
	if changed {
		if err := store.WriteJSON(path, state); err != nil {
			return "", err
		}
	}
	return "legacy:" + strconv.FormatUint(state.Sequence, 10), nil
}
