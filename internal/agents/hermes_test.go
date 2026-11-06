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

func TestHermesNativeFixtureReplay(t *testing.T) {
	a, emit := testHermes(t)
	data, err := os.ReadFile("testdata/hermes/native-shell-hooks.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	ctx, now := context.Background(), time.Now()
	for round := range 2 {
		for n, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
			if err := a.ConsumeHook(ctx, line, now.Add(time.Duration(round*100+n)*time.Millisecond), emit); err != nil {
				t.Fatal(err)
			}
			a = newHermes(a.config, a.source).(*hermesAdapter)
		}
	}
	counts := map[event.Kind]int{}
	for _, e := range ledgerEvents(t, a.ledger) {
		counts[e.Kind]++
		if e.SessionID != "test-session" {
			t.Fatal("subagent emitted after restart")
		}
	}
	if counts[event.TurnCompleted] != 2 || counts[event.InputRequested] != 1 || counts[event.ApprovalRequested] != 1 || counts[event.AttentionRequired] != 1 {
		t.Fatalf("replay changed notifications: %v", counts)
	}
}

func testHermes(t *testing.T) (*hermesAdapter, func(event.Event) error) {
	t.Helper()
	c := sourceConfig(t)
	s := config.Source{ID: "hermes-work", Harness: "hermes", Home: filepath.Join(t.TempDir(), "Hermes 配置"), EventVersion: 2, Enabled: true}
	c.Sources = []config.Source{s}
	a := newHermes(c, s).(*hermesAdapter)
	return a, func(e event.Event) error {
		_, err := (store.Queue{Dir: c.StateDir}).Enqueue(context.Background(), e, []string{"test"})
		return err
	}
}
func TestHermesOutcomeEnvelopeAndReplay(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
		kind        event.Kind
		reason      string
	}{
		{"completed", `"turn_id":"t","completed":true,"failed":false,"interrupted":false,"turn_exit_reason":"completed"`, event.TurnCompleted, ""},
		{"failed", `"turn_id":"t","completed":false,"failed":true,"interrupted":false,"turn_exit_reason":"api_error"`, event.AttentionRequired, "error"},
		{"unknown", `"turn_id":"t","completed":false,"failed":false,"interrupted":false,"turn_exit_reason":"budget_exhausted"`, event.AttentionRequired, "stopped"},
		{"cancelled", `"turn_id":"t","completed":false,"failed":true,"interrupted":true`, "", ""},
		{"ordinary-exit", `"platform":"cli"`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, emit := testHermes(t)
			payload := []byte(`{"hook_event_name":"on_session_end","session_id":"s","profile":"work","cwd":"D:/项目","extra":{` + tc.extra + `}}`)
			for range 2 {
				if err := a.ConsumeHook(context.Background(), payload, time.Now(), emit); err != nil {
					t.Fatal(err)
				}
				a = newHermes(a.config, a.source).(*hermesAdapter)
			}
			events := ledgerEvents(t, a.ledger)
			if tc.kind == "" {
				if len(events) != 0 {
					t.Fatalf("unexpected reminder: %+v", events)
				}
				return
			}
			if len(events) != 1 || events[0].Kind != tc.kind || events[0].Reason != tc.reason || events[0].TurnID != "t" || events[0].Project != "项目" {
				t.Fatalf("wrong outcome: %+v", events)
			}
		})
	}
}
func TestHermesActualHumanWaitAndQuestions(t *testing.T) {
	a, emit := testHermes(t)
	ctx := context.Background()
	now := time.Now().UTC()
	call := func(raw string) {
		t.Helper()
		if err := a.ConsumeHook(ctx, []byte(raw), now, emit); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Millisecond)
	}
	call(`{"hook_event_name":"pre_approval_request","session_id":"s","extra":{"surface":"smart","turn_id":"t"}}`)
	if len(ledgerEvents(t, a.ledger)) != 0 {
		t.Fatal("automatic approval emitted human reminder")
	}
	call(`{"hook_event_name":"pre_tool_call","session_id":"s","tool_name":"clarify","tool_input":{"questions":[{"question":"选择？","choices":["继续","结束"]}]},"extra":{"turn_id":"t","tool_call_id":"tool-q"}}`)
	call(`{"hook_event_name":"on_human_input_request","session_id":"s","extra":{"kind":"clarify","request_id":"q","prompt":"选择？"}}`)
	call(`{"hook_event_name":"on_human_input_request","session_id":"s","extra":{"kind":"clarify","request_id":"q","prompt":"选择？"}}`)
	call(`{"hook_event_name":"on_session_end","session_id":"s","extra":{"turn_id":"t","completed":true}}`)
	events := ledgerEvents(t, a.ledger)
	if len(events) != 1 || events[0].Kind != event.InputRequested || len(events[0].Questions) != 1 || len(events[0].Questions[0].Options) != 2 || events[0].TurnID != "t" {
		t.Fatalf("missing question/duplicate stop: %+v", events)
	}
	a = newHermes(a.config, a.source).(*hermesAdapter)
	call(`{"hook_event_name":"on_human_input_resolved","session_id":"s","extra":{"kind":"clarify","request_id":"q","outcome":"answered"}}`)
	call(`{"hook_event_name":"on_session_end","session_id":"s","extra":{"turn_id":"t","completed":true}}`)
	if len(ledgerEvents(t, a.ledger)) != 2 {
		t.Fatal("resolved wait swallowed end")
	}
}
func TestHermesNativeApprovalAndAnonymousResolution(t *testing.T) {
	for _, id := range []string{"r", ""} {
		t.Run(id, func(t *testing.T) {
			a, emit := testHermes(t)
			ctx := context.Background()
			now := time.Now()
			for _, name := range []string{"on_human_input_request", "on_human_input_resolved"} {
				payload, _ := json.Marshal(map[string]any{"hook_event_name": name, "session_id": "s", "extra": map[string]any{"kind": "approval", "request_id": id, "prompt": "confirm command"}})
				if err := a.ConsumeHook(ctx, payload, now, emit); err != nil {
					t.Fatal(err)
				}
			}
			if err := a.ConsumeHook(ctx, []byte(`{"hook_event_name":"on_session_end","session_id":"s","extra":{"completed":true}}`), now, emit); err != nil {
				t.Fatal(err)
			}
			events := ledgerEvents(t, a.ledger)
			if len(events) != 2 {
				t.Fatalf("wait not resolved: %+v", events)
			}
			found := false
			for _, e := range events {
				if e.Kind == event.ApprovalRequested && e.RequestID == id {
					found = true
				}
			}
			if !found {
				t.Fatal("approval identity changed")
			}
		})
	}
}
func TestHermesChildLineageAndSessionIsolation(t *testing.T) {
	a, emit := testHermes(t)
	ctx := context.Background()
	for _, raw := range []string{
		`{"hook_event_name":"subagent_start","session_id":"parent","extra":{"child_session_id":"child"}}`,
		`{"hook_event_name":"on_session_end","session_id":"child","extra":{"turn_id":"t","completed":true}}`,
		`{"hook_event_name":"on_human_input_request","session_id":"child","extra":{"kind":"approval","request_id":"q"}}`,
		`{"hook_event_name":"on_session_end","session_id":"parent","extra":{"turn_id":"t","completed":true}}`,
	} {
		if err := a.ConsumeHook(ctx, []byte(raw), time.Now(), emit); err != nil {
			t.Fatal(err)
		}
		a = newHermes(a.config, a.source).(*hermesAdapter)
	}
	events := ledgerEvents(t, a.ledger)
	if len(events) != 1 || events[0].SessionID != "parent" {
		t.Fatalf("child leaked into root reminders: %+v", events)
	}
	if err := a.ConsumeHook(ctx, []byte(`{"hook_event_name":"on_session_end","extra":{"turn_id":"t","completed":true}}`), time.Now(), emit); err == nil {
		t.Fatal("missing session admitted")
	}
}
func TestHermesInstallPreservesYAMLAndOwnership(t *testing.T) {
	a, _ := testHermes(t)
	ctx := context.Background()
	path := filepath.Join(a.source.Home, "config.yaml")
	original := []byte("# Keep this profile comment\nmodel: custom/model # keep inline comment\nhooks_auto_accept: false\nhooks:\n  outbound:\n    - url: https://example.invalid/events\n  on_session_end:\n    - command: user-hook\n      timeout: 7\n")
	if err := store.WriteFile(path, original); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), "中文 $ & ' blip.exe")
	if err := a.Install(ctx, executable); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)
	if !strings.Contains(string(first), "# keep inline comment") || !strings.Contains(string(first), "hooks_auto_accept: false") {
		t.Fatal("lost comments or changed trust")
	}
	if err := a.Install(ctx, executable); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) || a.InstallationStatus() != "configured_unverified" {
		t.Fatal("install not idempotent")
	}
	if err := a.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	doc, _, _, err := readHookYAML(path)
	if err != nil {
		t.Fatal(err)
	}
	var after map[string]any
	_ = doc.Decode(&after)
	if after["hooks_auto_accept"] != false || after["model"] != "custom/model" {
		t.Fatalf("user settings changed: %+v", after)
	}
	hooks := after["hooks"].(map[string]any)
	if len(hooks) != 2 || len(hooks["on_session_end"].([]any)) != 1 {
		t.Fatalf("other hooks lost: %+v", hooks)
	}
	if a.InstallationStatus() != "not_installed" {
		t.Fatal("ownership not removed")
	}
}
func TestHermesRefusesMalformedOrMergedHookSettings(t *testing.T) {
	for _, raw := range []string{"hooks: []\n", "hooks:\n  on_session_end: value\n", "model: a\nmodel: b\n", "defaults: &defaults {hooks: {}}\n<<: *defaults\n", "hooks: {}\n---\nmodel: a\n"} {
		t.Run(raw, func(t *testing.T) {
			a, _ := testHermes(t)
			path := filepath.Join(a.source.Home, "config.yaml")
			_ = store.WriteFile(path, []byte(raw))
			if err := a.Install(context.Background(), filepath.Join(t.TempDir(), "blip.exe")); err == nil {
				t.Fatal("unsafe YAML accepted")
			}
			after, _ := os.ReadFile(path)
			if string(after) != raw {
				t.Fatal("unsafe YAML overwritten")
			}
		})
	}
}
