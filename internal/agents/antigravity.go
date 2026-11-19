package agents

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/TeamSillyBees/Blip/internal/antigravity"
	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
	"github.com/ncruces/go-sqlite3"
	"google.golang.org/protobuf/proto"
)

const antigravityCheckpoint = "antigravity-summaries"

type antigravityAdapter struct {
	source     config.Source
	database   string
	checkpoint string
	db         *antigravity.SummaryDB
	file       os.FileInfo
	version    int64
}

func newAntigravity(c config.Config, s config.Source) Adapter {
	return &antigravityAdapter{source: s, database: filepath.Join(s.Home, antigravity.SummaryFilename), checkpoint: filepath.Join(StateDir(c, s), "summaries.json")}
}

func (a *antigravityAdapter) Capabilities() []Capability {
	const validation = "agy 1.5.9 local model on Windows; Antigravity 2.32.2 summary schema"
	return []Capability{
		{Event: "turn_completed", Support: "derived", Evidence: "fully idle root conversation with NO_TOOL_CALL termination", Validation: validation},
		{Event: "input_requested", Support: "derived", Evidence: "waiting ask_question or MCP elicitation in the local summary", Validation: validation},
		{Event: "approval_requested", Support: "derived", Evidence: "waiting step with an explicit permission interaction", Validation: validation},
		{Event: "attention_required", Support: "derived", Evidence: "idle after an executor error, limit or terminal step", Validation: "native enum and protocol fixtures"},
	}
}

// All surfaces publish summaries locally. There is no hook to install or interpret.
func (*antigravityAdapter) ParseHook(context.Context, []byte, time.Time) ([]event.Event, error) {
	return nil, nil
}

func (a *antigravityAdapter) ReadPaths() []ReadPath {
	return []ReadPath{{Path: a.database, Optional: true}, {Path: a.database + "-wal", Optional: true}, {Path: a.database + "-shm", Optional: true}}
}

func (a *antigravityAdapter) Close() error {
	if a.db == nil {
		return nil
	}
	err := a.db.Close()
	a.db, a.file, a.version = nil, nil, 0
	return err
}

type antigravityTurn struct {
	InputAt  time.Time `json:"input_at"`
	Settled  bool      `json:"settled,omitempty"`
	Requests []string  `json:"requests,omitempty"`
}

type antigravityCursor struct {
	Format   int                        `json:"format"`
	Since    time.Time                  `json:"since"`
	Sessions map[string]antigravityTurn `json:"sessions"`
}

func readAntigravityCursor(path string) (antigravityCursor, error) {
	var cursor antigravityCursor
	err := store.ReadJSON(path, &cursor)
	if errors.Is(err, os.ErrNotExist) {
		return cursor, err
	}
	if err == nil && cursor.Format == 1 && !cursor.Since.IsZero() && cursor.Sessions != nil {
		for id, turn := range cursor.Sessions {
			if id == "" || turn.InputAt.IsZero() {
				return cursor, ErrCheckpoint
			}
			for _, seen := range turn.Requests {
				if _, err := hex.DecodeString(seen); len(seen) != 64 || err != nil {
					return cursor, ErrCheckpoint
				}
			}
		}
		return cursor, nil
	}
	return cursor, ErrCheckpoint
}

func (a *antigravityAdapter) Poll(ctx context.Context, emit func(event.Event) error) error {
	// Establish the start time even when the client has yet to create its DB.
	// An already-finished history must not flood the queue on first attachment.
	if _, err := os.Stat(a.checkpoint); errors.Is(err, os.ErrNotExist) {
		cursor := antigravityCursor{Format: 1, Since: time.Now().UTC(), Sessions: map[string]antigravityTurn{}}
		if err := store.WriteJSON(a.checkpoint, cursor); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	info, err := os.Stat(a.database)
	if err != nil {
		_ = a.Close()
		return fmt.Errorf("open Antigravity conversation_summaries.db: %w", err)
	}
	if a.db == nil || !os.SameFile(info, a.file) {
		if err := a.Close(); err != nil {
			return err
		}
		a.db, err = antigravity.OpenSummaryDB(ctx, a.database)
		if err != nil {
			return err
		}
		a.file = info
	}
	// data_version is comparable only on the same connection. Read it before
	// the SELECT so a concurrent commit always schedules another snapshot.
	var version int64
	if err := a.db.QueryRowContext(ctx, "PRAGMA data_version").Scan(&version); err != nil {
		_ = a.Close()
		return err
	}
	if version == a.version {
		return nil
	}
	cursor, err := readAntigravityCursor(a.checkpoint)
	if err != nil {
		return checkpointFailed(antigravityCheckpoint, event.ID(antigravityCheckpoint), "")
	}
	if err := a.collect(ctx, cursor, emit); err != nil {
		return err
	}
	a.version = version
	return nil
}

func (a *antigravityAdapter) collect(ctx context.Context, cursor antigravityCursor, emit func(event.Event) error) error {
	const query = `SELECT coalesce(conversation_id, ''), +last_user_input_time, +last_modified_time, coalesce(status, ''),
		coalesce(not_fully_idle, 0) != 0, coalesce(title, ''), coalesce(workspace_uris, ''), raw_summary FROM conversation_summaries
		WHERE parent_conversation_id = '' AND nesting_depth = 0 AND killed = 0 AND raw_summary IS NOT NULL`
	rows, err := a.db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	changed := false
	for rows.Next() {
		var row antigravity.Row
		var input, modified any
		var raw sql.RawBytes
		if err := rows.Scan(&row.ConversationID, &input, &modified, &row.Status, &row.NotFullyIdle, &row.Title, &row.WorkspaceURIs, &raw); err != nil {
			return err
		}
		id := row.ConversationID
		inputAt, err := summaryTime(input)
		if err != nil {
			return fmt.Errorf("invalid last_user_input_time: %w", err)
		}
		if inputAt.IsZero() {
			continue // a new conversation without a user turn
		}
		turn := cursor.Sessions[id]
		if inputAt.Before(turn.InputAt) {
			continue
		}
		if !inputAt.Equal(turn.InputAt) {
			turn = antigravityTurn{InputAt: inputAt.UTC()}
		}
		status, busy := row.Status, row.NotFullyIdle
		if turn.Settled && status == "CASCADE_RUN_STATUS_IDLE" && !busy {
			continue // skip already settled history without decoding its summaries
		}
		row.UserInputAt = inputAt
		row.ModifiedAt, err = summaryTime(modified)
		if err != nil {
			return fmt.Errorf("invalid last_modified_time: %w", err)
		}
		var summary antigravity.Summary
		if err := (proto.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, &summary); err != nil {
			return fmt.Errorf("invalid Antigravity raw_summary: %w", err)
		}
		observed := inputAt.Equal(cursor.Sessions[id].InputAt)
		if !observed && (status == "CASCADE_RUN_STATUS_RUNNING" || busy) {
			cursor.Sessions[id] = turn
			changed = true
		}
		for _, e := range summary.Events(row, a.source.ID) {
			request := e.Kind == event.InputRequested || e.Kind == event.ApprovalRequested
			if request && slices.Contains(turn.Requests, e.ID) || !request && turn.Settled {
				continue
			}
			if request || observed || !e.OccurredAt.Before(cursor.Since) {
				if err := emit(e); err != nil {
					return err
				}
			}
			if request {
				turn.Requests = append(turn.Requests, e.ID)
			} else {
				turn.Settled = true
			}
			cursor.Sessions[id] = turn
			changed = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if changed {
		return store.WriteJSON(a.checkpoint, cursor)
	}
	return nil
}

// Preserve the timestamp formats and precision used in existing checkpoints
// and event IDs, regardless of the column's declared type in the client DB.
func summaryTime(v any) (time.Time, error) {
	if v == nil {
		return time.Time{}, nil
	}
	if b, ok := v.([]byte); ok {
		v = string(b)
	}
	return sqlite3.TimeFormatAuto.Decode(v)
}
