package service

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/agents"
	"github.com/TeamSillyBees/Blip/internal/antigravity"
	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
	"golang.org/x/sys/windows"
)

func TestClientRenewsOnlyRecordedAntigravityGrants(t *testing.T) {
	home := t.TempDir()
	source := config.Source{ID: "app", Harness: "antigravity", Enabled: true, Home: home}
	dbPath := filepath.Join(home, antigravity.SummaryFilename)
	sid, err := windows.StringToSid("S-1-5-80-304021-304022-304023-304024-304025")
	if err != nil {
		t.Fatal(err)
	}
	m := installMetadata{Sources: []config.Source{source}, ReadPaths: sourceReadPaths(config.Config{}, source)}
	// Simulate a prior installation that recorded only the main file and WAL.
	m.Grants = []aclChange{{Path: dbPath, SID: sid.String()}, {Path: dbPath + "-wal", SID: sid.String()}}
	for range 2 {
		client, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		if _, err := client.Exec("PRAGMA journal_mode=WAL; CREATE TABLE IF NOT EXISTS t (v); INSERT INTO t VALUES (1)"); err != nil {
			t.Fatal(err)
		}
		if n := allowedSIDEntries(t, dbPath+"-wal", sid.String(), true); n != 0 {
			t.Fatalf("expected a newly created WAL, found %d service ACEs", n)
		}
		for range 2 {
			if err := refreshAntigravityFiles(t.Context(), m, source, sid); err != nil {
				t.Fatal(err)
			}
		}
		for path, want := range map[string]int{home: 0, dbPath: 1, dbPath + "-wal": 1, dbPath + "-shm": 0} {
			if n := allowedSIDEntries(t, path, sid.String(), true); n != want {
				t.Fatalf("%s: %d service ACEs, want %d", path, n, want)
			}
		}
		if err := client.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWindowsServiceAntigravityWaits(t *testing.T) {
	if os.Getenv("BLIP_TEST_WINDOWS_SERVICE") != "1" {
		t.Skip("requires an isolated elevated Windows service test")
	}
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Fatal("SCM integration requires an elevated terminal")
	}
	dir := t.TempDir()
	parent := filepath.Join(dir, ".gemini")
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(parent, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	received := make(chan event.Event, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e event.Event
		if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
			t.Error(err)
			http.Error(w, "invalid event", http.StatusBadRequest)
			return
		}
		received <- e
	}))
	defer server.Close()
	source := config.Source{ID: "app", Harness: "antigravity", Enabled: true, Home: filepath.Join(parent, "antigravity"), EventVersion: 2}
	opts := InstallOptions{ConfigPath: filepath.Join(dir, "blip", "config.json"), StateDir: filepath.Join(dir, "blip", "data"), Executable: filepath.Join(dir, "blip.exe"), Sources: []config.Source{source}}
	c := config.Config{SchemaVersion: 2, StateDir: opts.StateDir, Sources: opts.Sources, Destinations: []config.Destination{{ID: "local", Kind: "webhook", Enabled: true, URL: server.URL}}}
	if err := store.WriteJSON(opts.ConfigPath, c); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "build", "-o", opts.Executable, "../../cmd/blip").CombinedOutput(); err != nil {
		t.Fatalf("build: %v %s", err, out)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if err := Install(ctx, opts); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := Uninstall(ctx, opts.ConfigPath); err != nil {
			t.Error(err)
		}
	})
	var samples map[string][]byte
	if err := store.ReadJSON("../agents/testdata/antigravity/summaries.json", &samples); err != nil {
		t.Fatal(err)
	}
	for run, name := range []string{"PreInvocation", "PreToolUse"} {
		// First start precedes the first client database. On restart, SQLite
		// will create sidecars with fresh ACLs underneath the private parent.
		if err := Start(ctx, opts.ConfigPath); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(source.Home, 0700); err != nil {
			t.Fatal(err)
		}
		client, err := sql.Open("sqlite", filepath.Join(source.Home, antigravity.SummaryFilename))
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		if _, err := client.Exec(`PRAGMA journal_mode=WAL; CREATE TABLE IF NOT EXISTS conversation_summaries
			(conversation_id TEXT PRIMARY KEY, title, workspace_uris DEFAULT '[]', parent_conversation_id DEFAULT '',
			nesting_depth DEFAULT 0, status DEFAULT 'CASCADE_RUN_STATUS_RUNNING', not_fully_idle DEFAULT 1,
			killed DEFAULT 0, step_count DEFAULT 1, last_user_input_step_index DEFAULT -1, last_modified_time DEFAULT CURRENT_TIMESTAMP, raw_summary BLOB)`); err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("chat-%d", run)
		if _, err := client.Exec("INSERT INTO conversation_summaries (conversation_id,title) VALUES (?, 'SCM question')", id); err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(map[string]any{"conversationId": id, "artifactDirectoryPath": filepath.Join(source.Home, "brain", id), "invocationNum": 0, "initialNumSteps": 0, "stepIdx": 0, "toolCall": map[string]string{"name": "ask_question"}})
		hook := exec.CommandContext(ctx, opts.Executable, "--config", opts.ConfigPath, "hook", "--source", source.ID, "--antigravity-event", name)
		hook.Stdin = bytes.NewReader(body)
		if out, err := hook.CombinedOutput(); err != nil {
			t.Fatalf("hook: %v %s", err, out)
		}
		var diag agents.HookDiagnostic
		if err := store.ReadJSON(agents.DiagnosticPath(c, source.ID), &diag); err != nil || diag.ErrorCode != "" {
			t.Fatalf("hook diagnostic: %+v %v", diag, err)
		}
		if _, err := client.Exec("UPDATE conversation_summaries SET raw_summary=?, step_count=2 WHERE conversation_id=?", samples["question"], id); err != nil {
			t.Fatal(err)
		}
		select {
		case e := <-received:
			if e.SessionID != id || e.SessionTitle != "SCM question" || e.Kind != event.InputRequested || len(e.Questions) != 1 || len(e.Questions[0].Options) != 2 {
				t.Fatalf("unexpected user handoff: %+v", e)
			}
		case <-time.After(8 * time.Second):
			log, _ := os.ReadFile(filepath.Join(c.StateDir, "worker.log"))
			status, _ := os.ReadFile(filepath.Join(c.StateDir, "worker.json"))
			t.Fatalf("LocalService missed %s: %s %s", id, log, status)
		}
		if _, err := client.Exec("UPDATE conversation_summaries SET raw_summary=NULL, status='CASCADE_RUN_STATUS_IDLE', not_fully_idle=0 WHERE conversation_id=?", id); err != nil {
			t.Fatal(err)
		}
		if err := Stop(ctx, opts.ConfigPath); err != nil {
			t.Fatal(err)
		}
		if err := client.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
