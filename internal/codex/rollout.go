package codex

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

const maxRecordBytes = 2 * 1024 * 1024

var ErrCheckpoint = errors.New("invalid rollout checkpoint; preserve it for diagnosis")

type Metadata struct {
	ID           string          `json:"id"`
	SessionID    string          `json:"session_id"`
	CWD          string          `json:"cwd"`
	ThreadSource string          `json:"thread_source"`
	Source       json.RawMessage `json:"source"`
}

func (m Metadata) IsSubagent() bool {
	if m.ThreadSource == "subagent" {
		return true
	}
	var source map[string]json.RawMessage
	if json.Unmarshal(m.Source, &source) == nil {
		_, ok := source["subagent"]
		if ok {
			return true
		}
	}
	return m.SessionID != "" && m.ID != m.SessionID
}

func (m Metadata) IsRoot() bool {
	if m.ID == "" || m.IsSubagent() {
		return false
	}
	if m.ThreadSource == "user" {
		return true
	}
	var source string
	if json.Unmarshal(m.Source, &source) != nil {
		return false
	}
	switch source {
	case "cli", "vscode", "exec", "appServer", "app-server":
		return m.ThreadSource == ""
	}
	return false
}

type record struct {
	Timestamp time.Time       `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}
type cursor struct {
	Offset     int64           `json:"offset"`
	Metadata   Metadata        `json:"metadata"`
	Aborted    map[string]bool `json:"aborted,omitempty"`
	HeaderHash string          `json:"header_hash,omitempty"`
	AnchorHash string          `json:"anchor_hash,omitempty"`
}
type checkpoint struct {
	Initialized bool               `json:"initialized"`
	BaselineAt  time.Time          `json:"baseline_at"`
	Files       map[string]*cursor `json:"files"`
}
type Collector struct {
	SessionsDir string
	StatePath   string
	state       checkpoint
	loaded      bool
}

func readMetadata(path string) (Metadata, error) {
	var metadata Metadata
	file, err := os.Open(path)
	if err != nil {
		return metadata, err
	}
	defer file.Close()
	metadata, _, err = readHeader(file)
	return metadata, err
}

func readHeader(reader io.Reader) (Metadata, string, error) {
	var metadata Metadata
	data, _, err := boundedLine(bufio.NewReader(reader))
	if err != nil {
		return metadata, "", err
	}
	var line record
	if json.Unmarshal(data, &line) != nil || line.Type != "session_meta" || json.Unmarshal(line.Payload, &metadata) != nil {
		return metadata, "", fmt.Errorf("unrecognized Codex session metadata")
	}
	return metadata, hashBytes(data), nil
}

// boundedLine consumes complete oversized records without allocating their full contents.
// EOF means an incomplete final line; callers must not advance their durable cursor.
func boundedLine(reader *bufio.Reader) ([]byte, int64, error) {
	var data []byte
	var consumed int64
	oversized := false
	for {
		part, err := reader.ReadSlice('\n')
		consumed += int64(len(part))
		if len(data)+len(part) <= maxRecordBytes && !oversized {
			data = append(data, part...)
		} else {
			data = nil
			oversized = true
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return data, consumed, err
	}
}

func baselineOffset(file io.ReaderAt, size int64) (int64, error) {
	start := size - 64*1024
	if start < 0 {
		start = 0
	}
	// Bound the baseline to the observed size. A concurrently appended completion
	// belongs to the next read and must not be swallowed by a read through EOF.
	data, err := io.ReadAll(io.NewSectionReader(file, start, size-start))
	if err != nil {
		return 0, err
	}
	for i := len(data) - 1; i >= 0; i-- {
		if data[i] == '\n' {
			return start + int64(i) + 1, nil
		}
	}
	return 0, nil
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func anchorHash(file io.ReaderAt, offset int64) (string, error) {
	if offset == 0 {
		return "", nil
	}
	start := offset - 512
	if start < 0 {
		start = 0
	}
	data := make([]byte, offset-start)
	if _, err := file.ReadAt(data, start); err != nil {
		return "", err
	}
	return hashBytes(data), nil
}

func validCheckpoint(state checkpoint) bool {
	if !state.Initialized || state.Files == nil || state.BaselineAt.IsZero() {
		return false
	}
	for path, position := range state.Files {
		if path == "" || position == nil || position.Offset < 0 {
			return false
		}
		for _, fingerprint := range []string{position.HeaderHash, position.AnchorHash} {
			if fingerprint == "" {
				continue
			} // Existing v0.1 checkpoints acquire fingerprints on their next read.
			if len(fingerprint) != sha256.Size*2 {
				return false
			}
			if _, err := hex.DecodeString(fingerprint); err != nil {
				return false
			}
		}
	}
	return true
}

func (c *Collector) Poll(ctx context.Context, emit func(event.Event) error) error {
	if !c.loaded {
		err := store.ReadJSON(c.StatePath, &c.state)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return ErrCheckpoint
		}
		if err == nil && !validCheckpoint(c.state) {
			return ErrCheckpoint
		}
		if errors.Is(err, os.ErrNotExist) {
			c.state.BaselineAt = time.Now().UTC()
		}
		if c.state.Files == nil {
			c.state.Files = map[string]*cursor{}
		}
		c.loaded = true
	}
	initial := !c.state.Initialized
	var failures []error
	walkErr := filepath.WalkDir(c.SessionsDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if errors.Is(walkErr, os.ErrNotExist) {
			return nil
		}
		if walkErr != nil {
			failures = append(failures, fmt.Errorf("cannot scan a Codex session directory"))
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "rollout-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
			return nil
		}
		if err := c.scanFile(ctx, path, initial, emit); err != nil {
			failures = append(failures, err)
		}
		return nil
	})
	if walkErr != nil {
		failures = append(failures, walkErr)
	}
	if initial && ctx.Err() != nil {
		c.loaded = false
		c.state = checkpoint{}
		return ctx.Err()
	}
	c.state.Initialized = true
	if err := store.WriteJSON(c.StatePath, c.state); err != nil {
		failures = append(failures, fmt.Errorf("save rollout checkpoint: %w", err))
	}
	return errors.Join(failures...)
}

func (c *Collector) scanFile(ctx context.Context, path string, initial bool, emit func(event.Event) error) error {
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("cannot open Codex session file")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("cannot inspect Codex session file")
	}
	metadata, header, err := readHeader(file)
	if err != nil {
		return nil
	} // A new or replaced file may still be writing its first line.
	position, exists := c.state.Files[path]
	reset := false
	if exists {
		reset = info.Size() < position.Offset || metadata.ID != position.Metadata.ID || metadata.SessionID != position.Metadata.SessionID || (position.HeaderHash != "" && header != position.HeaderHash)
		if !reset && position.AnchorHash != "" {
			anchor, err := anchorHash(file, position.Offset)
			if err != nil {
				return fmt.Errorf("cannot validate Codex session read position")
			}
			reset = anchor != position.AnchorHash
		}
	}
	if !exists || reset {
		position = &cursor{Metadata: metadata, Aborted: map[string]bool{}}
		// Files changed while the initial scan is underway may already contain
		// post-baseline events. Read them with the timestamp filter instead of
		// jumping past them; older, unchanged files can take the fast EOF path.
		if !exists && initial && info.ModTime().Before(c.state.BaselineAt) {
			position.Offset, err = baselineOffset(file, info.Size())
			if err != nil {
				return err
			}
		}
		c.state.Files[path] = position
	}
	position.Metadata, position.HeaderHash = metadata, header
	if !metadata.IsRoot() {
		return nil
	}
	readErr := c.read(ctx, file, position, emit)
	anchor, err := anchorHash(file, position.Offset)
	if err != nil {
		return errors.Join(readErr, fmt.Errorf("cannot save Codex session read position"))
	}
	position.AnchorHash = anchor
	return readErr
}

func (c *Collector) read(ctx context.Context, file *os.File, position *cursor, emit func(event.Event) error) error {
	var err error
	if _, err = file.Seek(position.Offset, io.SeekStart); err != nil {
		return err
	}
	reader := bufio.NewReader(file)
	if position.Aborted == nil {
		position.Aborted = map[string]bool{}
	}
	for i := 0; i < 2000; i++ {
		if err = ctx.Err(); err != nil {
			return err
		}
		data, consumed, readErr := boundedLine(reader)
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("cannot read Codex session record")
		}
		var line record
		if json.Unmarshal(data, &line) == nil {
			if line.Type == "session_meta" {
				var metadata Metadata
				if json.Unmarshal(line.Payload, &metadata) == nil {
					position.Metadata = metadata
				}
			}
			notification := completeEvent(line, position)
			if notification != nil && notification.OccurredAt.After(c.state.BaselineAt) {
				if err = emit(*notification); err != nil {
					return err
				}
			}
		}
		position.Offset += consumed
	}
	return nil
}

func completeEvent(line record, position *cursor) *event.Event {
	if line.Type != "event_msg" {
		return nil
	}
	var payload struct {
		Type             string          `json:"type"`
		TurnID           string          `json:"turn_id"`
		RootTurnID       string          `json:"root_turn_id"`
		LastAgentMessage string          `json:"last_agent_message"`
		Status           string          `json:"status"`
		Error            json.RawMessage `json:"error"`
	}
	if json.Unmarshal(line.Payload, &payload) != nil || payload.TurnID == "" {
		return nil
	}
	switch payload.Type {
	case "turn_aborted", "turn_failed", "task_failed":
		position.Aborted[payload.TurnID] = true
		return nil
	case "task_complete":
	default:
		return nil
	}
	if position.Aborted[payload.TurnID] || !position.Metadata.IsRoot() || (payload.RootTurnID != "" && payload.RootTurnID != payload.TurnID) || line.Timestamp.IsZero() {
		return nil
	}
	if payload.Status != "" && payload.Status != "completed" {
		return nil
	}
	if len(payload.Error) > 0 && string(payload.Error) != "null" {
		return nil
	}
	notification := &event.Event{SchemaVersion: 1, ID: event.ID("codex", position.Metadata.ID, payload.TurnID, string(event.TurnCompleted)), Agent: "codex", Kind: event.TurnCompleted, Source: "codex/rollout", OccurredAt: line.Timestamp.UTC(), SessionID: position.Metadata.ID, TurnID: payload.TurnID, Project: event.Project(position.Metadata.CWD), CWD: position.Metadata.CWD, Detail: event.LimitBytes(payload.LastAgentMessage, 4000)}
	return notification
}
