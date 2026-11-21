package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/store"
	"github.com/ncruces/go-sqlite3"
)

func TestAntigravityHookQueuesRecordedTitle(t *testing.T) {
	s := config.Source{ID: "app", Harness: "antigravity", Enabled: true, Home: t.TempDir(), EventVersion: 2}
	c := config.Config{SchemaVersion: 2, Sources: []config.Source{s}, StateDir: t.TempDir(), Destinations: []config.Destination{{ID: "test", Enabled: true}}}
	ctx := context.Background()
	path := filepath.Join(s.Home, "conversation_summaries.db")
	ask := func(id, request string) {
		t.Helper()
		hook := antigravityHook{Conversation: id, Artifacts: filepath.Join(s.Home, "brain", id), LastUserInput: "private prompt"}
		hook.ToolCall.ID, hook.ToolCall.Name = request, "ask_permission"
		payload, err := json.Marshal(hook)
		if err != nil {
			t.Fatal(err)
		}
		if err := CaptureAntigravity(ctx, c, s.ID, "PreToolUse", bytes.NewReader(payload)); err != nil {
			t.Fatal(err)
		}
	}
	ask("chat", "before-database")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("title lookup created the missing database: %v", err)
	}
	writer, err := sqlite3.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err := writer.Exec(`PRAGMA journal_mode=WAL;
		CREATE TABLE conversation_summaries (conversation_id TEXT PRIMARY KEY, title TEXT);
		INSERT INTO conversation_summaries VALUES ('chat','  第一轮
🙂  '), ('untitled',''), ('other','Other chat');`); err != nil {
		t.Fatal(err)
	}
	ask("chat", "named")
	ask("untitled", "blank")
	ask("absent", "no-row")
	if err := writer.Exec("UPDATE conversation_summaries SET title = '新名称' WHERE conversation_id = 'chat'"); err != nil {
		t.Fatal(err)
	}
	ask("chat", "renamed")
	ask("chat", "named") // Replaying the original hook preserves its captured name.
	if err := writer.Exec("PRAGMA journal_mode=DELETE; BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	ask("chat", "locked")
	if err := writer.Exec("ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	wants := map[string]string{"before-database": "", "named": "第一轮 🙂", "blank": "", "no-row": "", "renamed": "新名称", "locked": ""}
	jobs, err := (store.Queue{Dir: c.StateDir}).Load()
	if err != nil || len(jobs) != len(wants) {
		t.Fatalf("queued %d events: %v", len(jobs), err)
	}
	for _, job := range jobs {
		e := job.Event
		want, ok := wants[e.RequestID]
		if !ok || e.SessionTitle != want {
			t.Fatalf("request %q has session_title %q, want %q", e.RequestID, e.SessionTitle, want)
		}
		if want == "" {
			want = "未命名对话"
		}
		if !strings.HasPrefix(e.Body(), want+"\n") {
			t.Fatalf("request %q rendered the wrong title", e.RequestID)
		}
		if e.RequestID == "locked" && ResolveTitle(ctx, c, e) != "新名称" {
			t.Fatal("delivery could not resolve the title after the database was unlocked")
		}
	}
}
