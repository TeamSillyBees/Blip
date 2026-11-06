package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSessionTitleUsesLatestPhysicalRename(t *testing.T) {
	home := t.TempDir()
	data := titleRow(t, "session", "Original name", "2026-11-06T12:00:00Z") +
		titleRow(t, "other", "Unrelated name", "2026-11-06T12:10:00Z") +
		titleRow(t, "session", "Renamed conversation", "2026-11-06T11:00:00Z")
	writeTitleIndex(t, home, data)
	if got := SessionTitle(context.Background(), home, "session"); got != "Renamed conversation" {
		t.Fatalf("did not use the last physical name entry: %q", got)
	}
}

func TestSessionTitleIgnoresIncompleteAndInvalidRows(t *testing.T) {
	home := t.TempDir()
	valid := titleRow(t, "session", "Complete name", "")
	for _, suffix := range []struct{ name, data string }{
		{"partial JSON", `{"id":"session","thread_name":"unfinished`},
		{"JSON without newline", strings.TrimSuffix(titleRow(t, "session", "Uncommitted name", ""), "\n")},
		{"malformed complete row", "{broken JSON}\n"},
		{"wrong title type", "{\"id\":\"session\",\"thread_name\":3}\n"},
		{"missing title", "{\"id\":\"session\"}\n"},
		{"null title", "{\"id\":\"session\",\"thread_name\":null}\n"},
		{"invalid UTF-8", "{\"id\":\"session\",\"thread_name\":\"\xff\"}\n"},
		{"oversized complete row", titleRow(t, "session", strings.Repeat("x", maxTitleRowBytes+1), "")},
		{"oversized partial row", strings.Repeat("x", maxTitleRowBytes*2)},
	} {
		t.Run(suffix.name, func(t *testing.T) {
			writeTitleIndex(t, home, valid+suffix.data)
			if got := SessionTitle(context.Background(), home, "session"); got != "Complete name" {
				t.Fatalf("incomplete or invalid row replaced the last complete name: %q", got)
			}
		})
	}
}

func TestSessionTitleDoesNotInventMissingNames(t *testing.T) {
	home := t.TempDir()
	if got := SessionTitle(context.Background(), home, "session"); got != "" {
		t.Fatal("missing index supplied a name")
	}
	for _, data := range []string{
		"",
		titleRow(t, "other", "Different conversation", ""),
		titleRow(t, "session", "Old name", "") + titleRow(t, "session", "", ""),
		titleRow(t, "session", "Old name", "") + titleRow(t, "session", " \t\n\x00 ", ""),
		"{\"id\":\"session\",\"title\":\"Message preview\",\"first_user_message\":\"Do something\"}\n",
	} {
		writeTitleIndex(t, home, data)
		if got := SessionTitle(context.Background(), home, "session"); got != "" {
			t.Fatalf("missing/empty recorded name produced invented or stale text: %q", got)
		}
	}
	if got := SessionTitle(context.Background(), "", "session"); got != "" {
		t.Fatal("empty home supplied a name")
	}
	if got := SessionTitle(context.Background(), home, ""); got != "" {
		t.Fatal("empty session ID supplied a name")
	}
}

func TestSessionTitleReflectsReplacedIndex(t *testing.T) {
	home := t.TempDir()
	writeTitleIndex(t, home, titleRow(t, "session", "Before replacement", ""))
	if got := SessionTitle(context.Background(), home, "session"); got != "Before replacement" {
		t.Fatal("initial name missing")
	}
	path := filepath.Join(home, "session_index.jsonl")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	writeTitleIndex(t, home, titleRow(t, "session", "After replacement", ""))
	if got := SessionTitle(context.Background(), home, "session"); got != "After replacement" {
		t.Fatalf("lookup kept a stale name after replacement: %q", got)
	}
}

func TestSessionTitleNormalizesAndBoundsUnicode(t *testing.T) {
	home := t.TempDir()
	writeTitleIndex(t, home, titleRow(t, "session", "  实验室\t聊天\n名称\x00\x1f  💻  ", ""))
	if got := SessionTitle(context.Background(), home, "session"); got != "实验室 聊天 名称 💻" {
		t.Fatalf("title is not a single normalized line: %q", got)
	}
	writeTitleIndex(t, home, titleRow(t, "session", strings.Repeat("中文💻", 200), ""))
	got := SessionTitle(context.Background(), home, "session")
	if !utf8.ValidString(got) || len(got) > maxTitleBytes || !strings.HasSuffix(got, "…") {
		t.Fatal("long Unicode title was not bounded safely")
	}
}

func TestSessionTitleAcrossReverseChunkBoundaries(t *testing.T) {
	for _, tailSize := range []int{titleChunkBytes - 1, titleChunkBytes, titleChunkBytes + 1, titleChunkBytes*2 + 7} {
		t.Run(strconv.Itoa(tailSize), func(t *testing.T) {
			home := t.TempDir()
			// Blank rows make the matching row straddle each relevant chunk
			// alignment, including the start of the file and CRLF records.
			data := strings.TrimSuffix(titleRow(t, "session", "Boundary 名称", ""), "\n") + "\r\n" + strings.Repeat("\n", tailSize)
			writeTitleIndex(t, home, data)
			if got := SessionTitle(context.Background(), home, "session"); got != "Boundary 名称" {
				t.Fatalf("name crossing a chunk boundary was lost: %q", got)
			}
		})
	}
}

func TestSessionTitleHasNoTotalIndexSizeCutoff(t *testing.T) {
	home := t.TempDir()
	// An oversized record is ignored individually without discarding older
	// valid names merely because they are farther than a fixed scan limit.
	data := titleRow(t, "session", "Older conversation", "") + strings.Repeat("x", 9*1024*1024) + "\n"
	writeTitleIndex(t, home, data)
	if got := SessionTitle(context.Background(), home, "session"); got != "Older conversation" {
		t.Fatalf("older name was lost behind a large index record: %q", got)
	}
}

func TestSessionTitleChecksCancellationDuringScan(t *testing.T) {
	home := t.TempDir()
	writeTitleIndex(t, home, titleRow(t, "session", "Available name", ""))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := SessionTitle(ctx, home, "session"); got != "" {
		t.Fatal("cancelled lookup supplied a name")
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	data := []byte(titleRow(t, "session", "Older name", "") + strings.Repeat("\n", titleChunkBytes*2))
	reader := &cancelTitleReader{Reader: bytes.NewReader(data), cancel: cancel}
	if got := readSessionTitle(ctx, reader, int64(len(data)), "session"); got != "" || reader.calls != 1 {
		t.Fatalf("lookup continued reading after cancellation: calls=%d", reader.calls)
	}
}

type cancelTitleReader struct {
	*bytes.Reader
	cancel context.CancelFunc
	calls  int
}

func (r *cancelTitleReader) ReadAt(data []byte, offset int64) (int, error) {
	r.calls++
	n, err := r.Reader.ReadAt(data, offset)
	r.cancel()
	return n, err
}

func titleRow(t *testing.T, id, name, updatedAt string) string {
	t.Helper()
	data, err := json.Marshal(map[string]string{"id": id, "thread_name": name, "updated_at": updatedAt})
	if err != nil {
		t.Fatal(err)
	}
	return string(data) + "\n"
}

func writeTitleIndex(t *testing.T, home, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, "session_index.jsonl"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
