package agents

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/antigravity"
	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
	"github.com/ncruces/go-sqlite3/driver"
)

func TestAntigravitySummaryLifecycle(t *testing.T) {
	ctx := context.Background()
	c := sourceConfig(t)
	s := config.Source{ID: "agy", Harness: "antigravity", Enabled: true, Home: filepath.Join(t.TempDir(), "中文 # % & agy")}
	c.Sources = []config.Source{s}
	if err := os.MkdirAll(s.Home, 0700); err != nil {
		t.Fatal(err)
	}
	open := func() *antigravityAdapter {
		a, err := Builtins().Open(c, s)
		if err != nil {
			t.Fatal(err)
		}
		adapter := a.(*antigravityAdapter)
		t.Cleanup(func() { _ = adapter.Close() })
		return adapter
	}
	a := open()
	var events []event.Event
	emit := func(e event.Event) error {
		if err := e.Validate(); err != nil {
			return err
		}
		events = append(events, e)
		return nil
	}
	if err := a.Poll(ctx, emit); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing database: %v", err)
	}
	if _, err := os.Stat(a.database); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("reader created a database")
	}
	db, err := driver.Open(a.database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`PRAGMA journal_mode=WAL;
		CREATE TABLE conversation_summaries (conversation_id TEXT PRIMARY KEY, title TEXT, workspace_uris TEXT,
		status TEXT, not_fully_idle INTEGER, killed INTEGER, parent_conversation_id TEXT, nesting_depth INTEGER,
		last_user_input_time DATETIME, last_modified_time DATETIME, raw_summary BLOB)`)
	if err != nil {
		t.Fatal(err)
	}
	var samples map[string][]byte
	if err := store.ReadJSON("testdata/antigravity/summaries.json", &samples); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	put := func(id, sample, status string, turn, modified time.Time) {
		t.Helper()
		const layout = "2006-01-02 15:04:05.999999999-07:00"
		_, err := db.Exec(`INSERT OR REPLACE INTO conversation_summaries VALUES (?, 'Blip demo', '["file:///C:/work/My%20Project"]', ?, 0, 0, '', 0, ?, ?, ?)`, id, status, turn.Format(layout), modified.Format(layout), samples[sample])
		if err != nil {
			t.Fatal(err)
		}
	}
	const idle, running = "CASCADE_RUN_STATUS_IDLE", "CASCADE_RUN_STATUS_RUNNING"
	old := now.Add(-24 * time.Hour)
	put("history", "completed", idle, old, old)
	put("child", "permission", running, now, now)
	if _, err := db.Exec("UPDATE conversation_summaries SET parent_conversation_id='root', nesting_depth=1 WHERE conversation_id='child'"); err != nil {
		t.Fatal(err)
	}
	put("root", "permission", running, now, now)
	// The checkpoint must not consume a change until durable enqueue succeeds.
	busy := errors.New("queue busy")
	if err := a.Poll(ctx, func(event.Event) error { return busy }); !errors.Is(err, busy) {
		t.Fatalf("failed enqueue: %v", err)
	}
	poll := func() {
		t.Helper()
		if err := a.Poll(ctx, emit); err != nil {
			t.Fatal(err)
		}
	}
	poll()
	if len(events) != 1 || events[0].Kind != event.ApprovalRequested || events[0].Detail != "run_command" || events[0].RequestID != "call-123" || events[0].SessionID != "root" || events[0].SourceID != "agy" || events[0].Project != "My Project" || events[0].SessionTitle != "Blip demo" {
		t.Fatalf("approval: %+v", events)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a = open()
	poll()
	put("root", "question", running, now, now.Add(time.Second))
	poll()
	if len(events) != 2 || events[1].Kind != event.InputRequested || len(events[1].Questions) != 1 || events[1].Questions[0].Text != "Test input?" || !slices.Equal(events[1].Questions[0].Options, []string{"Yes", "No"}) {
		t.Fatalf("question: %+v", events)
	}
	put("root", "completed", idle, now, now.Add(2*time.Second))
	poll()
	poll()
	put("root", "completed", idle, now.Add(time.Minute), now.Add(2*time.Minute))
	poll()
	if len(events) != 4 || events[2].Kind != event.TurnCompleted || events[3].Kind != event.TurnCompleted || events[2].ID == events[3].ID {
		t.Fatalf("separate completed turns: %+v", events)
	}
	// Force the next poll to reread the persisted checkpoint.
	_ = a.Close()
	if err := os.WriteFile(a.checkpoint, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.Poll(ctx, emit); !errors.Is(err, ErrCheckpoint) {
		t.Fatalf("corrupt checkpoint: %v", err)
	} else if err := RecordCheckpointFault(ctx, c, s, err); err != nil {
		t.Fatal(err)
	}
	if err := CheckSourceCheckpoint(ctx, c, s); !errors.Is(err, ErrCheckpoint) {
		t.Fatalf("corrupt checkpoint should pause this source: %v", err)
	}
	if err := os.Remove(a.checkpoint); err != nil {
		t.Fatal(err)
	}
	if err := CheckSourceCheckpoint(ctx, c, s); !errors.Is(err, ErrCheckpoint) {
		t.Fatalf("deleting the checkpoint cleared the fault: %v", err)
	}
}

func TestAntigravityWaitingInteraction(t *testing.T) {
	now := time.Now()
	row := antigravity.Row{ConversationID: "session", UserInputAt: now, ModifiedAt: now, Status: "CASCADE_RUN_STATUS_RUNNING"}
	for _, test := range []struct {
		name string
		step *antigravity.Step
		want event.Kind
	}{
		{"automatic tool", &antigravity.Step{Status: 2}, ""},
		{"waiting without interaction", &antigravity.Step{Status: 9}, ""},
		{"resolved permission", &antigravity.Step{Status: 3, RequestedInteraction: &antigravity.Interaction{Permission: &antigravity.Approval{}}}, ""},
		{"MCP elicitation", &antigravity.Step{Status: 9, RequestedInteraction: &antigravity.Interaction{Elicitation: &antigravity.Elicitation{Message: "Choose an account"}}}, event.InputRequested},
		{"blocking review", &antigravity.Step{Status: 9, NotifyUser: &antigravity.NotifyUser{IsBlocking: true, NotificationContent: "Review plan"}}, event.InputRequested},
		{"automatic review", &antigravity.Step{Status: 9, NotifyUser: &antigravity.NotifyUser{IsBlocking: true, ShouldAutoProceed: true}}, ""},
		{"file permission", &antigravity.Step{Status: 9, RequestedInteraction: &antigravity.Interaction{FilePermission: &antigravity.Approval{}}}, event.ApprovalRequested},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := antigravity.Summary{WaitingSteps: []*antigravity.WaitingStep{{Step: test.step, StepIndex: 2}}}
			got := s.Events(row, "app")
			if test.want == "" {
				if len(got) != 0 {
					t.Fatalf("unexpected event: %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].Kind != test.want || got[0].RequestID != "" || got[0].ID == s.Events(row, "cli")[0].ID {
				t.Fatalf("wrong event or cross-source identity: %+v", got)
			}
		})
	}
}

func TestAntigravityOnlyReportsHandoffs(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		status            string
		busy, interrupted bool
		reason            uint32
		want              event.Kind
	}{
		{"CASCADE_RUN_STATUS_RUNNING", false, false, 4, ""},
		{"CASCADE_RUN_STATUS_CANCELING", false, false, 4, ""},
		{"CASCADE_RUN_STATUS_IDLE", true, false, 4, ""},
		{"CASCADE_RUN_STATUS_IDLE", false, true, 4, ""},
		{"CASCADE_RUN_STATUS_IDLE", false, false, 2, ""},
		{"CASCADE_RUN_STATUS_IDLE", false, false, 0, ""},
		{"CASCADE_RUN_STATUS_IDLE", false, false, 12, ""},
		{"CASCADE_RUN_STATUS_IDLE", false, false, 1, event.AttentionRequired},
		{"CASCADE_RUN_STATUS_IDLE", false, false, 11, event.AttentionRequired},
	} {
		s := antigravity.Summary{Interrupted: test.interrupted, LatestExecutorTerminationReason: test.reason}
		r := antigravity.Row{ConversationID: "root", Status: test.status, NotFullyIdle: test.busy, UserInputAt: now, ModifiedAt: now}
		got := s.Events(r, "app")
		if test.want == "" && len(got) != 0 || test.want != "" && (len(got) != 1 || got[0].Kind != test.want || got[0].Validate() != nil) {
			t.Errorf("%+v: %+v", test, got)
		}
	}
}
