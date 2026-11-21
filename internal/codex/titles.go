package codex

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/TeamSillyBees/Blip/internal/event"
)

const (
	titleChunkBytes  = 64 * 1024
	maxTitleRowBytes = 64 * 1024
	titleLookupTime  = 450 * time.Millisecond
)

// SessionTitle returns the latest recorded conversation name from Codex's local
// session index. It does not infer names from message text or session IDs. Each
// call reads the current file so later renames and replacements are reflected.
// Missing, unreadable, or time-limited lookups return an empty name.
func SessionTitle(ctx context.Context, codexHome, sessionID string) string {
	if codexHome == "" || sessionID == "" || ctx.Err() != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, titleLookupTime)
	defer cancel()
	file, err := os.Open(filepath.Join(codexHome, "session_index.jsonl"))
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return ""
	}
	return readSessionTitle(ctx, file, info.Size(), sessionID)
}

// Read backwards from a captured size. Seeing a newline first establishes a
// complete record; any unterminated append at the end is skipped. Reversing one
// bounded row at a time avoids loading the index or retaining unrelated titles.
func readSessionTitle(ctx context.Context, file io.ReaderAt, size int64, sessionID string) string {
	chunk := make([]byte, titleChunkBytes)
	row := make([]byte, 0, maxTitleRowBytes)
	terminated, oversized := false, false
	for offset := size; offset > 0; {
		if ctx.Err() != nil {
			return ""
		}
		length := int64(len(chunk))
		if length > offset {
			length = offset
		}
		offset -= length
		n, err := file.ReadAt(chunk[:length], offset)
		if err != nil || n != int(length) {
			return "" // A concurrent truncation makes this snapshot unreliable.
		}
		for i := n - 1; i >= 0; i-- {
			if chunk[i] != '\n' {
				if terminated && !oversized {
					if len(row) == maxTitleRowBytes {
						oversized = true
						row = row[:0]
					} else {
						row = append(row, chunk[i])
					}
				}
				continue
			}
			if ctx.Err() != nil {
				return ""
			}
			if terminated && !oversized {
				if title, found := titleFromReverseRow(row, sessionID); found {
					return title
				}
			}
			terminated, oversized = true, false
			row = row[:0]
		}
	}
	if ctx.Err() == nil && terminated && !oversized {
		if title, found := titleFromReverseRow(row, sessionID); found {
			return title
		}
	}
	return ""
}

func titleFromReverseRow(row []byte, sessionID string) (string, bool) {
	if len(row) == 0 {
		return "", false
	}
	for left, right := 0, len(row)-1; left < right; left, right = left+1, right-1 {
		row[left], row[right] = row[right], row[left]
	}
	if !utf8.Valid(row) {
		return "", false
	}
	var entry struct {
		ID         string  `json:"id"`
		ThreadName *string `json:"thread_name"`
	}
	if json.Unmarshal(row, &entry) != nil || entry.ID != sessionID || entry.ThreadName == nil {
		return "", false
	}
	// An explicitly empty latest name must not resurrect an older name.
	return event.NormalizeSessionTitle(*entry.ThreadName), true
}
