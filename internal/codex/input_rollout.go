package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/TeamSillyBees/Blip/internal/event"
)

// inputTurn is optional in old checkpoints. A non-nil empty ID records an
// observed turn boundary, so input after completion cannot reuse an old turn.
type inputTurn struct {
	ID  string `json:"id"`
	CWD string `json:"cwd,omitempty"`
}

func inputBoundary(line record) (*inputTurn, string) {
	if line.Type != "turn_context" && line.Type != "event_msg" {
		return nil, ""
	}
	var payload struct {
		Type       string `json:"type"`
		TurnID     string `json:"turn_id"`
		RootTurnID string `json:"root_turn_id"`
		CWD        string `json:"cwd"`
	}
	if json.Unmarshal(line.Payload, &payload) != nil || payload.TurnID == "" || (payload.RootTurnID != "" && payload.RootTurnID != payload.TurnID) {
		return nil, ""
	}
	if line.Type == "turn_context" {
		return &inputTurn{ID: payload.TurnID, CWD: payload.CWD}, ""
	}
	switch payload.Type {
	case "task_started", "turn_started":
		return &inputTurn{ID: payload.TurnID, CWD: payload.CWD}, ""
	case "task_complete", "turn_aborted", "turn_failed", "task_failed":
		return &inputTurn{}, payload.TurnID
	}
	return nil, ""
}

func observeInputTurn(line record, position *cursor) {
	boundary, ended := inputBoundary(line)
	if boundary == nil {
		return
	}
	if ended != "" && position.InputTurn != nil && position.InputTurn.ID != "" && ended != position.InputTurn.ID {
		return // A late record for another turn cannot close the active turn.
	}
	position.InputTurn = boundary
}

// restoreInputTurn looks backwards only when a fresh/legacy checkpoint skipped
// the active turn's context. It does not rewind the durable cursor or replay
// historical notifications. Memory is bounded by the maximum record size, and
// cancellation leaves the request unconsumed so a later poll can retry.
func restoreInputTurn(ctx context.Context, file io.ReaderAt, offset int64) (*inputTurn, error) {
	var suffix []byte
	oversized := false
	for offset > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		start := offset - 64*1024
		if start < 0 {
			start = 0
		}
		chunk := make([]byte, int(offset-start))
		if _, err := file.ReadAt(chunk, start); err != nil {
			return nil, fmt.Errorf("cannot restore Codex input turn: %w", err)
		}
		for len(chunk) > 0 {
			separator := bytes.LastIndexByte(chunk, '\n')
			part := chunk[separator+1:]
			if !oversized && len(part)+len(suffix) <= maxRecordBytes {
				joined := make([]byte, 0, len(part)+len(suffix))
				joined = append(joined, part...)
				joined = append(joined, suffix...)
				suffix = joined
			} else {
				suffix, oversized = nil, true
			}
			if separator < 0 && start != 0 {
				break
			}
			var line record
			if !oversized && json.Unmarshal(suffix, &line) == nil {
				if boundary, _ := inputBoundary(line); boundary != nil {
					return boundary, nil
				}
			}
			suffix, oversized = nil, false
			if separator < 0 {
				break
			}
			chunk = chunk[:separator]
		}
		offset = start
	}
	return &inputTurn{}, nil
}

func rolloutInput(ctx context.Context, file io.ReaderAt, line record, position *cursor) (*event.Event, error) {
	if line.Type != "response_item" || line.Timestamp.IsZero() || !position.Metadata.IsRoot() {
		return nil, nil
	}
	var call struct {
		Type      string          `json:"type"`
		Name      string          `json:"name"`
		CallID    string          `json:"call_id"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if json.Unmarshal(line.Payload, &call) != nil || call.Type != "function_call" || call.CallID == "" || !inputTool.MatchString(call.Name) {
		return nil, nil
	}
	if position.InputTurn == nil {
		var err error
		position.InputTurn, err = restoreInputTurn(ctx, file, position.Offset)
		if err != nil {
			return nil, err
		}
	}
	turn := position.InputTurn
	if turn.ID == "" || position.Aborted[turn.ID] {
		return nil, nil
	}
	cwd := turn.CWD
	if cwd == "" {
		cwd = position.Metadata.CWD
	}
	data, err := json.Marshal(Hook{Name: "PreToolUse", SessionID: position.Metadata.ID, TurnID: turn.ID, CWD: cwd, ToolName: call.Name, ToolUseID: call.CallID, Input: call.Arguments})
	if err != nil {
		return nil, nil
	}
	// The same parser preserves the hook's native request ID, text limits and
	// notification ID. Whichever path arrives first owns the durable snapshot.
	notification, err := ParseHook(bytes.NewReader(data), line.Timestamp)
	if err != nil || notification == nil || len(notification.Questions) == 0 {
		return nil, nil // Unknown/malformed records must not guess a question.
	}
	notification.Source = "codex/rollout"
	return notification, nil
}
