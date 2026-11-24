package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TeamSillyBees/Blip/internal/event"
)

const antigravityTranscriptTail = 1 << 20

// A slow transcript must leave time for the Stop hook to enqueue the notification.
func antigravityFinalReply(ctx context.Context, home, conversation string, firstStep int) string {
	budget := 250 * time.Millisecond
	if deadline, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(deadline)/2)
	}
	return antigravityLookup(ctx, budget, func(ctx context.Context) string {
		return readAntigravityReply(ctx, home, conversation, firstStep)
	})
}

// Opening client files can block in the OS even after the lookup context expires.
func antigravityLookup[T any](ctx context.Context, budget time.Duration, read func(context.Context) T) T {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	result := make(chan T, 1)
	go func() { result <- read(ctx) }()
	select {
	case text := <-result:
		return text
	case <-ctx.Done():
		var zero T
		return zero
	}
}

// Open the standard log relative to the bound conversation, never an arbitrary path from stdin.
func openAntigravityTranscript(home, conversation string) (*os.File, error) {
	root, err := os.OpenRoot(home)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	chat := filepath.Join("brain", conversation)
	info, err := root.Lstat(chat)
	if err != nil || !info.IsDir() {
		return nil, os.ErrNotExist
	}
	dir, err := root.OpenRoot(chat)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	const log = ".system_generated/logs/transcript.jsonl"
	info, err = dir.Stat(log)
	if err != nil || !info.Mode().IsRegular() {
		return nil, os.ErrNotExist
	}
	return dir.Open(log)
}

func readAntigravityReply(ctx context.Context, home, conversation string, firstStep int) string {
	f, err := openAntigravityTranscript(home, conversation)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || ctx.Err() != nil {
		return ""
	}
	size := info.Size()
	start := max(int64(0), size-antigravityTranscriptTail-1)
	data := make([]byte, size-start)
	if _, err := f.ReadAt(data, start); err != nil {
		return ""
	}
	if start > 0 {
		// The first row of a tail can start in the middle of a JSON string.
		_, data, _ = bytes.Cut(data, []byte{'\n'})
	}
	for len(data) > 0 && ctx.Err() == nil {
		data = bytes.TrimSpace(data)
		line := bytes.LastIndexByte(data, '\n') + 1
		var step struct {
			Index     *int       `json:"step_index"`
			Source    string     `json:"source"`
			Type      string     `json:"type"`
			Status    string     `json:"status"`
			Content   string     `json:"content"`
			ToolCalls []struct{} `json:"tool_calls"`
		}
		if json.Unmarshal(data[line:], &step) != nil || step.Index == nil || *step.Index < firstStep || step.Type == "USER_INPUT" {
			return ""
		}
		if step.Type == "PLANNER_RESPONSE" {
			// A later tool call or empty response supersedes earlier commentary.
			if step.Source == "MODEL" && step.Status == "DONE" && len(step.ToolCalls) == 0 {
				return event.LimitBytes(strings.TrimSpace(step.Content), 4000)
			}
			return ""
		}
		data = data[:line]
	}
	return ""
}
