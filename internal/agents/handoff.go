package agents

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
	"github.com/gofrs/flock"
)

// handoffs is a small durable notification ledger, not an agent activity model.
// Adapters decide which native signals prove cancellation or resumed execution.
type handoffs struct {
	config config.Config
	source config.Source
}

type stopCandidate struct {
	OccurredAt time.Time `json:"occurred_at"`
	DueAt      time.Time `json:"due_at"`
	CWD        string    `json:"cwd,omitempty"`
}
type handoffState struct {
	Format           int             `json:"format"`
	Revision         uint64          `json:"revision"`
	SessionID        string          `json:"session_id"`
	TurnKey          string          `json:"turn_key"`
	NativeTurnID     string          `json:"native_turn_id,omitempty"`
	Sequence         uint64          `json:"sequence"`
	Notified         bool            `json:"notified"`
	NotifiedKind     event.Kind      `json:"notified_kind,omitempty"`
	Cancelled        bool            `json:"cancelled"`
	RequestKind      event.Kind      `json:"request_kind,omitempty"`
	RequestID        string          `json:"request_id,omitempty"`
	Resumes          map[string]bool `json:"resumes,omitempty"`
	Candidate        *stopCandidate  `json:"candidate,omitempty"`
	LastSignalAt     time.Time       `json:"last_signal_at,omitempty"`
	LastResumeAt     time.Time       `json:"last_resume_at,omitempty"`
	CoveredByRequest bool            `json:"covered_by_request,omitempty"`
}

func (h handoffs) key(session, turn string) string {
	return event.SourceEventID(h.source.ID, h.source.Harness, "state", session, turn)
}
func (h handoffs) root() string { return filepath.Join(StateDir(h.config, h.source), "handoffs") }
func (h handoffs) path(key string, pending bool) string {
	dir := "settled"
	if pending {
		dir = "pending"
	}
	return filepath.Join(h.root(), dir, key+".json")
}

func (h handoffs) read(key string) (handoffState, bool, error) {
	var newest handoffState
	found := false
	for _, pending := range []bool{false, true} {
		var state handoffState
		err := store.ReadJSON(h.path(key, pending), &state)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil || state.Format != 1 || state.Revision == 0 || state.SessionID == "" || state.TurnKey == "" || h.key(state.SessionID, state.TurnKey) != key {
			return newest, false, ErrCheckpoint
		}
		if state.Candidate != nil && (state.Candidate.OccurredAt.IsZero() || state.Candidate.DueAt.IsZero()) {
			return newest, false, ErrCheckpoint
		}
		if !found || state.Revision > newest.Revision {
			newest = state
			found = true
		}
	}
	return newest, found, nil
}

func (h handoffs) mutate(ctx context.Context, session, turn, nativeTurn string, fn func(*handoffState) error) error {
	if session == "" || turn == "" {
		return fmt.Errorf("handoff requires a stable session and turn identity")
	}
	key := h.key(session, turn)
	initial := handoffState{Format: 1, SessionID: session, TurnKey: turn, NativeTurnID: nativeTurn}
	return h.mutateKey(ctx, key, &initial, fn)
}

func (h handoffs) mutateKey(ctx context.Context, key string, initial *handoffState, fn func(*handoffState) error) error {
	lockPath := filepath.Join(h.root(), "locks", key+".lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0700); err != nil {
		return err
	}
	lock := flock.New(lockPath)
	locked, err := lock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		_ = lock.Close()
		return err
	}
	if !locked {
		_ = lock.Close()
		return fmt.Errorf("handoff state is busy")
	}
	defer lock.Close()
	state, found, err := h.read(key)
	if err != nil {
		return err
	}
	if !found {
		if initial == nil {
			return nil
		}
		state = *initial
	}
	before, _ := json.Marshal(state)
	if err = fn(&state); err != nil {
		return err
	}
	after, _ := json.Marshal(state)
	if string(before) == string(after) {
		if found {
			if err = os.Remove(h.path(key, state.Candidate == nil)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		return nil
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	state.Revision++
	pending := state.Candidate != nil
	if err = store.WriteJSON(h.path(key, pending), state); err != nil {
		return err
	}
	// A crash between publish and remove leaves two revisions. Readers always
	// choose the newer one; pending scans can never resurrect the older state.
	if err = os.Remove(h.path(key, !pending)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (h handoffs) id(state *handoffState) string {
	if h.source.ID == config.DefaultSourceID && h.source.Harness == "codex" && state.Sequence == 0 {
		return event.ID("codex", state.SessionID, state.NativeTurnID, string(event.TurnCompleted))
	}
	return event.SourceEventID(h.source.ID, h.source.Harness, "handoff", state.SessionID, state.TurnKey, strconv.FormatUint(state.Sequence, 10))
}

func (h handoffs) finish(ctx context.Context, e event.Event, turnKey string, emit func(event.Event) error) error {
	return h.mutate(ctx, e.SessionID, turnKey, e.TurnID, func(state *handoffState) error {
		if state.Notified || state.Cancelled || e.OccurredAt.Before(state.LastResumeAt) {
			return nil
		}
		if state.RequestKind != "" {
			state.Candidate = nil
			state.CoveredByRequest = true
			return nil
		}
		e.ID = h.id(state)
		if err := emit(e); err != nil {
			return err
		}
		kind, err := h.firstKind(ctx, e)
		if err != nil {
			return err
		}
		state.Notified = true
		state.NotifiedKind = kind
		state.LastSignalAt = e.OccurredAt
		state.Candidate = nil
		return nil
	})
}

func (h handoffs) firstKind(ctx context.Context, e event.Event) (event.Kind, error) {
	first, err := (store.Queue{Dir: h.config.StateDir}).Snapshot(ctx, e.ID)
	if err != nil {
		return "", err
	}
	if first != nil {
		if first.Agent != e.Agent || first.SessionID != e.SessionID || first.TurnID != e.TurnID {
			return "", fmt.Errorf("notification identity conflicts with the original snapshot")
		}
		return first.Kind, nil
	}
	return e.Kind, nil
}

func (h handoffs) request(ctx context.Context, e event.Event, turnKey string, emit func(event.Event) error) error {
	return h.mutate(ctx, e.SessionID, turnKey, e.TurnID, func(state *handoffState) error {
		if state.Cancelled {
			return nil
		}
		if err := emit(e); err != nil {
			return err
		}
		state.RequestKind = e.Kind
		state.RequestID = e.RequestID
		state.LastSignalAt = e.OccurredAt
		state.Candidate = nil
		return nil
	})
}

func (h handoffs) stop(ctx context.Context, session, turnKey, nativeTurn, cwd string, now time.Time) error {
	return h.mutate(ctx, session, turnKey, nativeTurn, func(state *handoffState) error {
		if state.Notified || state.Cancelled || state.RequestKind != "" || state.Candidate != nil {
			return nil
		}
		state.Candidate = &stopCandidate{OccurredAt: now.UTC(), DueAt: now.Add(time.Second).UTC(), CWD: cwd}
		state.LastSignalAt = now.UTC()
		return nil
	})
}

func (h handoffs) resume(ctx context.Context, session, turnKey, nativeTurn, identity string, userPrompt bool, at time.Time) error {
	if identity == "" {
		return nil
	}
	return h.mutate(ctx, session, turnKey, nativeTurn, func(state *handoffState) error {
		if state.Resumes[identity] {
			return nil
		}
		if state.Resumes == nil {
			state.Resumes = map[string]bool{}
		}
		state.Resumes[identity] = true
		if !at.After(state.LastSignalAt) || !at.After(state.LastResumeAt) {
			return nil
		}
		state.LastResumeAt = at.UTC()
		// Tool maintenance after a confirmed completion must not open another
		// handoff. Only an earlier neutral/error alert can be followed by resume.
		if state.Notified && state.NotifiedKind == event.AttentionRequired {
			state.Sequence++
			state.Notified = false
			state.NotifiedKind = ""
		}
		state.Candidate = nil
		if userPrompt {
			if state.CoveredByRequest {
				state.Sequence++
				state.CoveredByRequest = false
			}
			state.RequestKind = ""
			state.RequestID = ""
			state.Cancelled = false
		}
		return nil
	})
}

func (h handoffs) resolveApproval(ctx context.Context, session, turnKey, nativeTurn, requestID string) error {
	if requestID == "" {
		return nil
	}
	return h.mutate(ctx, session, turnKey, nativeTurn, func(state *handoffState) error {
		if state.RequestKind == event.ApprovalRequested && state.RequestID == requestID {
			state.RequestKind = ""
			state.RequestID = ""
		}
		return nil
	})
}

func (h handoffs) cancel(ctx context.Context, session, turnKey, nativeTurn string, at time.Time) error {
	return h.mutate(ctx, session, turnKey, nativeTurn, func(state *handoffState) error {
		if at.Before(state.LastResumeAt) {
			return nil
		}
		state.Cancelled = true
		state.Candidate = nil
		state.LastSignalAt = at.UTC()
		return nil
	})
}

func (h handoffs) flush(ctx context.Context, now time.Time, emit func(event.Event) error) error {
	entries, err := os.ReadDir(filepath.Join(h.root(), "pending"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		key := strings.TrimSuffix(entry.Name(), ".json")
		err = h.mutateKey(ctx, key, nil, func(current *handoffState) error {
			candidate := current.Candidate
			if candidate == nil || candidate.DueAt.After(now) {
				return nil
			}
			if current.Notified || current.Cancelled || current.RequestKind != "" {
				current.Candidate = nil
				return nil
			}
			e := event.Event{SchemaVersion: 2, ID: h.id(current), Agent: h.source.Harness, SourceID: h.source.ID, Kind: event.AttentionRequired, Reason: "stop_signal", Source: h.source.Harness + "/hooks", OccurredAt: candidate.OccurredAt, SessionID: current.SessionID, TurnID: current.NativeTurnID, Project: event.Project(candidate.CWD), CWD: candidate.CWD, Detail: "收到停止信号，请回到原会话查看。"}
			if err := emit(e); err != nil {
				return err
			}
			kind, err := h.firstKind(ctx, e)
			if err != nil {
				return err
			}
			current.Notified = true
			current.NotifiedKind = kind
			current.Candidate = nil
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
