package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

func testDSH(t *testing.T) (*dshAdapter, func(event.Event) error) {
	t.Helper()
	c := sourceConfig(t)
	s := config.Source{ID: "dsh-desktop", Harness: "dsh", Home: filepath.Join(t.TempDir(), "DSH 中文 $ & ' profile"), Enabled: true, EventVersion: 2}
	c.Sources = []config.Source{s}
	if err := store.WriteFile(filepath.Join(s.Home, "cordis.yml"), []byte("[]\n")); err != nil {
		t.Fatal(err)
	}
	a := newDSH(c, s).(*dshAdapter)
	return a, func(e event.Event) error {
		_, err := (store.Queue{Dir: c.StateDir}).Enqueue(context.Background(), e, []string{"test"})
		return err
	}
}

func TestDSHNativeFixtureReplay(t *testing.T) {
	a, emit := testDSH(t)
	data, err := os.ReadFile("testdata/dsh/native-runtime-hooks.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
			if err := a.ConsumeHook(context.Background(), line, time.Now(), emit); err != nil {
				t.Fatal(err)
			}
			a = newDSH(a.config, a.source).(*dshAdapter)
		}
	}
	counts := map[event.Kind]int{}
	for _, e := range ledgerEvents(t, a.ledger) {
		counts[e.Kind]++
	}
	if counts[event.TurnCompleted] != 4 || counts[event.AttentionRequired] != 2 || counts[event.InputRequested] != 2 || counts[event.ApprovalRequested] != 1 {
		t.Fatal(counts)
	}
}
func TestDSHNativeEnvelope(t *testing.T) {
	for _, tc := range []struct {
		reason, cause string
		root          bool
		kind          event.Kind
		detail        string
	}{
		{"completed", "", true, event.TurnCompleted, ""},
		{"error", "", true, event.AttentionRequired, "error"},
		{"max-tokens", "", true, event.AttentionRequired, "stopped"},
		{"blocked", "", true, event.AttentionRequired, "stopped"},
		{"aborted", "hook", true, event.AttentionRequired, "stopped"},
		{"aborted", "user", true, "", ""},
		{"aborted", "disposed", true, "", ""},
		{"completed", "", false, "", ""},
		{"interrupted", "", true, "", ""},
	} {
		t.Run(tc.reason+tc.cause+string(tc.kind), func(t *testing.T) {
			a, emit := testDSH(t)
			at := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
			raw, _ := json.Marshal(dshHook{Version: 1, Name: "turn/end", SessionID: "s", TurnID: "1", Root: &tc.root, Reason: tc.reason, CancelCause: tc.cause, OccurredAt: at})
			for range 2 {
				if err := a.ConsumeHook(context.Background(), raw, time.Now(), emit); err != nil {
					t.Fatal(err)
				}
				a = newDSH(a.config, a.source).(*dshAdapter)
			}
			events := ledgerEvents(t, a.ledger)
			if tc.kind == "" {
				if len(events) != 0 {
					t.Fatal(events)
				}
				return
			}
			if len(events) != 1 || events[0].Kind != tc.kind || events[0].Reason != tc.detail || !events[0].OccurredAt.Equal(at) {
				t.Fatal(events)
			}
		})
	}
	a, _ := testDSH(t)
	if _, err := a.ParseHook(context.Background(), []byte(`{"hook_event_name":"Stop","session_id":"s"}`), time.Now()); err == nil {
		t.Fatal("lossy Claude bridge accepted as native")
	}
}
func TestDSHRequestsRemainSpecificAcrossReplay(t *testing.T) {
	a, emit := testDSH(t)
	ctx := context.Background()
	root := true
	h := dshHook{Version: 1, Name: "request", SessionID: "s", TurnID: "1", Root: &root, RequestID: "call-1", Kind: "input", Questions: []event.Question{{Text: "Choose", Options: []string{"A", "B"}}}}
	call := func() {
		t.Helper()
		data, _ := json.Marshal(h)
		if err := a.ConsumeHook(ctx, data, time.Now(), emit); err != nil {
			t.Fatal(err)
		}
		a = newDSH(a.config, a.source).(*dshAdapter)
	}
	call()
	h.Name = "turn/end"
	h.Reason = "completed"
	call()
	if got := ledgerEvents(t, a.ledger); len(got) != 1 || got[0].Kind != event.InputRequested {
		t.Fatal(got)
	}
	h.Name = "resolved"
	call()
	h.Name = "request"
	call()
	h.Name = "turn/end"
	call()
	if got := ledgerEvents(t, a.ledger); len(got) != 1 {
		t.Fatal(got)
	}
	h.TurnID = "2"
	call()
	if got := ledgerEvents(t, a.ledger); len(got) != 2 {
		t.Fatal(got)
	}
}

func TestDSHInstallerOwnershipAndRecovery(t *testing.T) {
	a, _ := testDSH(t)
	ctx := context.Background()
	path := a.installer().path
	original := []byte("# keep user choices\n- insert:\n    - id: other\n      name: ./other.mjs\n      config:\n        dynamic: !!js process.env.EXAMPLE\n- override:\n    id: retained\n    disabled: true\n")
	if err := store.WriteFile(path, original); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(t.TempDir(), "程序 $ & '.exe")
	if err := a.Install(ctx, exe); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)
	if err := a.Install(ctx, exe); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if !bytes.Equal(first, second) || a.InstallationStatus() != "configured_unverified" {
		t.Fatal("non-idempotent install")
	}
	// A failed settings publication leaves the ownership journal. Reinstall
	// must accept the old row while changing the executable.
	if err := a.Install(ctx, exe+".new"); err != nil {
		t.Fatal(err)
	}
	if err := a.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !strings.Contains(string(after), "keep user choices") || !strings.Contains(string(after), "!!js process.env.EXAMPLE") || !strings.Contains(string(after), "disabled: true") || strings.Contains(string(after), "blip-") {
		t.Fatal(string(after))
	}
	if err := a.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestDSHInstallerRefusesEditedRowsAndFiles(t *testing.T) {
	for _, target := range []string{"row", "file"} {
		t.Run(target, func(t *testing.T) {
			a, _ := testDSH(t)
			ctx := context.Background()
			if err := a.Install(ctx, filepath.Join(t.TempDir(), "blip.exe")); err != nil {
				t.Fatal(err)
			}
			path := a.installer().path
			if target == "file" {
				m, _ := a.installer().manifest()
				path = m.Files[0]
			}
			before, _ := os.ReadFile(path)
			edited := append([]byte("# user edit\n"), before...)
			if target == "row" {
				edited = bytes.Replace(before, []byte("sourceId: dsh-desktop"), []byte("sourceId: changed"), 1)
			}
			if err := os.WriteFile(path, edited, 0600); err != nil {
				t.Fatal(err)
			}
			if err := a.Uninstall(ctx); err == nil {
				t.Fatal("edited installation removed")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(after, edited) {
				t.Fatal("user edit overwritten")
			}
		})
	}
}
