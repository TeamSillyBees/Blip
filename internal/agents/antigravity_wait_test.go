package agents

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/antigravity"
	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
	"google.golang.org/protobuf/proto"
)

func summaryFixture(t *testing.T, home string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(home, antigravity.SummaryFilename))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	execSummary(t, db, `PRAGMA journal_mode=WAL;
		CREATE TABLE conversation_summaries (conversation_id TEXT PRIMARY KEY, title TEXT DEFAULT '', workspace_uris TEXT DEFAULT '[]',
		parent_conversation_id TEXT DEFAULT '', nesting_depth INTEGER DEFAULT 0, status TEXT DEFAULT 'CASCADE_RUN_STATUS_RUNNING',
		not_fully_idle INTEGER DEFAULT 1, killed INTEGER DEFAULT 0, step_count INTEGER DEFAULT 0,
		last_user_input_step_index INTEGER DEFAULT -1, last_modified_time DATETIME DEFAULT CURRENT_TIMESTAMP, raw_summary BLOB)`)
	return db
}

func execSummary(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatal(err)
	}
}

func waitingSample(t *testing.T, name, call string, idx int) []byte {
	t.Helper()
	var samples map[string][]byte
	if err := store.ReadJSON("testdata/antigravity/summaries.json", &samples); err != nil {
		t.Fatal(err)
	}
	var s antigravity.Summary
	if err := proto.Unmarshal(samples[name], &s); err != nil {
		t.Fatal(err)
	}
	for _, w := range s.WaitingSteps {
		w.StepIndex = uint32(idx)
		w.Step.Metadata.ToolCall.Id = call
	}
	b, err := proto.Marshal(&s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAntigravityTeamworkHandoff(t *testing.T) {
	home := t.TempDir()
	s := config.Source{ID: "app", Harness: "antigravity", Home: home, Enabled: true, EventVersion: 2}
	c := config.Config{StateDir: t.TempDir()}
	a := newAntigravity(c, s).(*antigravityAdapter)
	t.Cleanup(func() { a.Close() })
	db := summaryFixture(t, home)
	execSummary(t, db, `INSERT INTO conversation_summaries (conversation_id,parent_conversation_id,nesting_depth,title,workspace_uris) VALUES
		('root','',0,'Node Detection And Filtering Optimization','["file:///C:/projects/SubMeld"]'),
		('relay','root',1,'','[]'),('interviewer','relay',2,'','[]'),('lead','relay',2,'','[]'),
		('track','lead',3,'','[]'),('self','track',4,'','[]'),('explorer','self',5,'A named worker','[]'),
		('worker','self',5,'','[]'),('orphan','not-yet-present',1,'','[]'),('depth-only','',2,'','[]'),
		('cycle','cycle',1,'','[]'),('other','',0,'Independent task','["file:///D:/other"]')`)
	parse := func(id, name, reason string, idle bool, first int) []event.Event {
		t.Helper()
		index := 0
		h := antigravityHook{Conversation: id, Artifacts: filepath.Join(home, "brain", id), Name: name, Termination: reason, FullyIdle: idle, Invocation: &index, InitialSteps: &first}
		b, _ := json.Marshal(h)
		events, err := a.ParseHook(context.Background(), b, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return events
	}
	// The interviewer hands off to the relay. Only the ensuing root question
	// and review halt are user-visible. Survey reports and worker errors stay local.
	for _, id := range []string{"interviewer", "relay", "lead", "track", "self", "explorer", "worker", "orphan", "depth-only", "cycle"} {
		for _, reason := range []string{"NO_TOOL_CALL", "ERROR", "MAX_INVOCATIONS"} {
			if got := parse(id, "Stop", reason, true, 0); len(got) != 0 {
				t.Fatalf("internal stop %s: %+v", id, got)
			}
		}
	}
	var handoffs []string
	for _, first := range []int{1, 9} {
		parse("root", "PreInvocation", "", false, first)
		if got := parse("root", "Stop", "NO_TOOL_CALL", false, 0); len(got) != 0 {
			t.Fatalf("background work: %+v", got)
		}
		got := parse("root", "Stop", "NO_TOOL_CALL", true, 0)
		if len(got) != 1 || got[0].SessionTitle != "Node Detection And Filtering Optimization" {
			t.Fatalf("root handoff: %+v", got)
		}
		handoffs = append(handoffs, got[0].ID)
	}
	if handoffs[0] == handoffs[1] {
		t.Fatal("collapsed independent root handoffs")
	}
	for _, id := range []string{"explorer", "worker", "orphan", "depth-only", "cycle"} {
		parse(id, "PreInvocation", "", false, 1)
		execSummary(t, db, "UPDATE conversation_summaries SET raw_summary=? WHERE conversation_id=?", waitingSample(t, "permission", "same-call", 2), id)
	}
	parse("other", "PreInvocation", "", false, 1)
	execSummary(t, db, "UPDATE conversation_summaries SET raw_summary=? WHERE conversation_id='other'", waitingSample(t, "question", "ask", 2))
	var events []event.Event
	queue := store.Queue{Dir: c.StateDir}
	emit := func(e event.Event) error {
		if err := e.Validate(); err != nil {
			return err
		}
		events = append(events, e)
		_, err := queue.Enqueue(t.Context(), e, []string{"local"})
		return err
	}
	// A failed enqueue leaves the wait eligible. A restarted worker deduplicates
	// the same request even if a sibling uses the same tool call ID.
	if err := a.Poll(t.Context(), func(event.Event) error { return os.ErrPermission }); err == nil {
		t.Fatal("expected enqueue failure")
	}
	for i := 0; i < 2; i++ {
		if err := a.Poll(t.Context(), emit); err != nil {
			t.Fatal(err)
		}
		if err := a.Close(); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := queue.Load()
	if err != nil || len(jobs) != 3 {
		t.Fatalf("waiting jobs %d, events %+v: %v", len(jobs), events, err)
	}
	for _, j := range jobs {
		e := j.Event
		switch e.Kind {
		case event.InputRequested:
			if e.SessionID != "other" || e.SessionTitle != "Independent task" || !slices.Equal(e.Questions[0].Options, []string{"Yes", "No"}) {
				t.Fatalf("independent root: %+v", e)
			}
		case event.ApprovalRequested:
			if e.SessionID != "root" || e.SessionTitle != "Node Detection And Filtering Optimization" || e.Project != "SubMeld" || e.RequestID != "same-call" {
				t.Fatalf("bubbled approval: %+v", e)
			}
		default:
			t.Fatalf("unexpected notification: %+v", e)
		}
	}
	execSummary(t, db, "UPDATE conversation_summaries SET killed=1 WHERE conversation_id='relay'")
	events = nil
	if err := a.Poll(t.Context(), emit); err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.SessionID == "root" {
			t.Fatalf("requested approval under a cancelled ancestor: %+v", e)
		}
	}
	execSummary(t, db, "INSERT INTO conversation_summaries (conversation_id,title) VALUES ('not-yet-present','Late parent')")
	if err := a.Poll(t.Context(), emit); err != nil {
		t.Fatal(err)
	}
	jobs, err = queue.Load()
	if err != nil || len(jobs) != 4 {
		t.Fatalf("late parent was missed: %v %v", jobs, err)
	}
}
func TestAntigravityOnlyReportsOutstandingInteractions(t *testing.T) {
	dbHome := t.TempDir()
	db := summaryFixture(t, dbHome)
	execSummary(t, db, "INSERT INTO conversation_summaries (conversation_id,title) VALUES ('session','Root')")
	a := newAntigravity(config.Config{StateDir: t.TempDir()}, config.Source{ID: "app", Home: dbHome}).(*antigravityAdapter)
	t.Cleanup(func() { a.Close() })
	h := antigravityHook{Conversation: "session", Artifacts: filepath.Join(dbHome, "brain", "session"), Name: "PreToolUse"}
	h.ToolCall.Name, h.ToolCall.ID = "run_command", "command"
	h.ToolCall.Args = []byte(`{"BypassSandbox":true}`)
	if err := store.WriteJSON(filepath.Join(StateDir(a.config, a.source), "invocations", event.ID("session")+".json"), 1); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(h)
	got, err := a.ParseHook(t.Context(), data, time.Now())
	if err != nil || len(got) != 0 {
		t.Fatalf("possible bypass emitted an early approval: %+v %v", got, err)
	}
	emit := func(e event.Event) error { got = append(got, e); return nil }
	poll := func(want int) {
		t.Helper()
		got = nil
		if err := a.Poll(t.Context(), emit); err != nil || len(got) != want {
			t.Fatalf("poll returned %+v: %v", got, err)
		}
	}
	// This callback attached after the first invocation, with only a tool ID.
	execSummary(t, db, "UPDATE conversation_summaries SET last_user_input_step_index=10, status='CASCADE_RUN_STATUS_IDLE', not_fully_idle=0, last_modified_time='2020-01-01'")
	poll(0)
	approve := &antigravity.Interaction{FilePermission: &antigravity.Approval{}}
	step := &antigravity.Step{Status: 3, RequestedInteraction: approve}
	set := func() {
		t.Helper()
		raw, err := proto.Marshal(&antigravity.Summary{WaitingSteps: []*antigravity.WaitingStep{{Step: step, StepIndex: 12}}})
		if err != nil {
			t.Fatal(err)
		}
		execSummary(t, db, "UPDATE conversation_summaries SET raw_summary=?", raw)
	}
	set()
	poll(0) // already resolved by policy or by the user
	step.Status = 9
	execSummary(t, db, "UPDATE conversation_summaries SET status='CASCADE_RUN_STATUS_RUNNING',not_fully_idle=1")
	set()
	poll(1)
	if got[0].Kind != event.ApprovalRequested {
		t.Fatalf("file approval: %+v", got)
	}
	execSummary(t, db, "UPDATE conversation_summaries SET title='Renamed'")
	poll(0) // unrelated commits while the same question is on screen
	step.RequestedInteraction = nil
	step.NotifyUser = &antigravity.NotifyUser{IsBlocking: true, ShouldAutoProceed: true}
	set()
	poll(0)
	step.NotifyUser.ShouldAutoProceed = false
	step.NotifyUser.NotificationContent = "Review plan"
	set()
	poll(1)
	if got[0].Kind != event.InputRequested || got[0].Questions[0].Text != "Review plan" {
		t.Fatalf("blocking review: %+v", got)
	}
	step.NotifyUser = nil
	step.RequestedInteraction = &antigravity.Interaction{Elicitation: &antigravity.Elicitation{Message: "Pick an account"}}
	step.Metadata = &antigravity.StepMetadata{ToolCall: &antigravity.ToolCall{Id: "next-request"}}
	set()
	poll(1)
	if got[0].Questions[0].Text != "Pick an account" {
		t.Fatalf("MCP question: %+v", got)
	}
	execSummary(t, db, "UPDATE conversation_summaries SET status='CASCADE_RUN_STATUS_IDLE', not_fully_idle=0, step_count=14, last_modified_time=datetime('now','+1 second'), raw_summary=NULL")
	poll(0)
	files, err := os.ReadDir(a.watches())
	if err != nil || len(files) != 0 {
		t.Fatalf("settled watch: %+v %v", files, err)
	}
}
