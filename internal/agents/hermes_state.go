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

type hermesRequest struct {
	TurnKey         string `json:"turn_key"`
	NativeTurn      string `json:"native_turn,omitempty"`
	RequestIdentity string `json:"request_identity"`
}
type hermesSession struct {
	Format     int                      `json:"format"`
	SessionID  string                   `json:"session_id"`
	Child      bool                     `json:"child,omitempty"`
	TurnKey    string                   `json:"turn_key,omitempty"`
	NativeTurn string                   `json:"native_turn,omitempty"`
	Sequence   uint64                   `json:"sequence"`
	Ended      bool                     `json:"ended,omitempty"`
	LastEnd    time.Time                `json:"last_end,omitempty"`
	Pending    map[string]hermesRequest `json:"pending,omitempty"`
	Resolved   map[string]bool          `json:"resolved,omitempty"`
	Questions  []event.Question         `json:"questions,omitempty"`
}

func (s *hermesSession) legacyKey() string { return "legacy:" + strconv.FormatUint(s.Sequence, 10) }
func (a *hermesAdapter) withSession(ctx context.Context, session string, fn func(*hermesSession) error) error {
	dir := filepath.Join(StateDir(a.config, a.source), "sessions")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dir, event.ID(session)+".json")
	lock := flock.New(path + ".lock")
	ok, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil || !ok {
		_ = lock.Close()
		if err == nil {
			err = context.DeadlineExceeded
		}
		return err
	}
	defer lock.Close()
	s := hermesSession{Format: 1, SessionID: session, Sequence: 1}
	err = store.ReadJSON(path, &s)
	if (err != nil && !errors.Is(err, os.ErrNotExist)) || s.Format != 1 || s.SessionID != session || s.Sequence == 0 {
		return checkpointFailed("hermes-session", event.ID(session), session)
	}
	if err := fn(&s); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return store.WriteJSON(path, s)
}
