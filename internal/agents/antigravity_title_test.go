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
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

func TestAntigravityHookQueuesRecordedTitle(t *testing.T) {
	s := config.Source{ID: "app", Harness: "antigravity", Enabled: true, Home: t.TempDir(), EventVersion: 2}
	c := config.Config{SchemaVersion: 2, Sources: []config.Source{s}, StateDir: t.TempDir(), Destinations: []config.Destination{{ID: "test", Enabled: true}}}
	ctx := context.Background()
	path := filepath.Join(s.Home, "conversation_summaries.db")
	for _, id := range []string{"chat", "untitled", "absent", "child", "unknown"} {
		line := `{"step_index":0,"type":"USER_INPUT","source":"USER_EXPLICIT"}`
		if id == "child" {
			line = `{"step_index":0,"type":"SYSTEM_MESSAGE","source":"SYSTEM"}` + "\n" + line
		}
		if id == "unknown" {
			line = `{"step_index":12,"type":"USER_INPUT","source":"USER_EXPLICIT"}`
		}
		if err := store.WriteFile(filepath.Join(s.Home, "brain", id, ".system_generated", "logs", "transcript.jsonl"), []byte(line+"\n")); err != nil {
			t.Fatal(err)
		}
	}
	wants := map[string]string{}
	stop := func(id, turn, want string, notify bool) {
		t.Helper()
		hook := antigravityHook{Conversation: id, Artifacts: filepath.Join(s.Home, "brain", id), LastUserInput: turn, CWD: filepath.Join(s.Home, "project"), FullyIdle: true, Termination: "NO_TOOL_CALL"}
		payload, _ := json.Marshal(hook)
		if err := CaptureAntigravity(ctx, c, s.ID, "Stop", bytes.NewReader(payload), nil); err != nil {
			t.Fatal(err)
		}
		if notify {
			key := event.SourceEventID("app", "antigravity", "handoff", id, "execution:0", event.ID(turn))
			wants[key] = want
		}
	}
	stop("chat", "before-db", "", true)
	stop("child", "never promote system-origin workers", "", false)
	stop("unknown", "later input alone is insufficient", "", false)
	stop("missing", "no local evidence", "", false)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lookup created missing database: %v", err)
	}
	db := summaryFixture(t, s.Home)
	execSummary(t, db, "INSERT INTO conversation_summaries (conversation_id,title) VALUES ('chat','  第一轮\n🙂  '),('untitled',''),('other','Other chat')")
	stop("chat", "named", "第一轮 🙂", true)
	stop("untitled", "blank", "", true)
	stop("absent", "no row", "", true)
	execSummary(t, db, "UPDATE conversation_summaries SET title='新名称' WHERE conversation_id='chat'")
	stop("chat", "renamed", "新名称", true)
	stop("chat", "named", "第一轮 🙂", true)
	execSummary(t, db, "PRAGMA journal_mode=DELETE; BEGIN EXCLUSIVE")
	stop("chat", "locked", "", true)
	execSummary(t, db, "ROLLBACK")
	jobs, err := (store.Queue{Dir: c.StateDir}).Load()
	if err != nil || len(jobs) != len(wants) {
		t.Fatalf("queued %d, want %d: %v", len(jobs), len(wants), err)
	}
	for _, job := range jobs {
		e := job.Event
		want, ok := wants[e.ID]
		if !ok || e.SessionTitle != want {
			t.Fatalf("wrong captured title: %+v, want %q", e, want)
		}
		if want == "" {
			want = "project 对话"
		}
		if !strings.HasPrefix(e.Body(), want+"\n") {
			t.Fatalf("wrong title in body: %s", e.Body())
		}
		if e.SessionID == "chat" && e.SessionTitle == "" && ResolveTitle(ctx, c, e) != "新名称" {
			t.Fatal("delivery could not resolve latest title")
		}
	}
}
