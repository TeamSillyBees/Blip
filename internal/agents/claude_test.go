package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

func TestClaudeNativeFixtureAndToolFreeContinuation(t *testing.T) {
	a, emit := testClaude(t)
	ctx, now := context.Background(), time.Now().UTC()
	data, err := os.ReadFile("testdata/claude-code/native-hooks.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for n, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		if err := a.ConsumeHook(ctx, line, now.Add(time.Duration(n)*time.Millisecond), emit); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.ledger.flush(ctx, now.Add(2*time.Second), emit); err != nil {
		t.Fatal(err)
	}
	if len(ledgerEvents(t, a.ledger)) != 1 {
		t.Fatal("native fixture lost Stop")
	}
	continued := []byte(`{"session_id":"test-session","prompt_id":"test-prompt","hook_event_name":"Stop","stop_hook_active":true,"last_assistant_message":"Continued after another hook, without tools."}`)
	if err := a.ConsumeHook(ctx, continued, now.Add(3*time.Second), emit); err != nil {
		t.Fatal(err)
	}
	if err := a.ledger.flush(ctx, now.Add(4*time.Second), emit); err != nil {
		t.Fatal(err)
	}
	if err := a.ConsumeHook(ctx, continued, now.Add(5*time.Second), emit); err != nil {
		t.Fatal(err)
	}
	if err := a.ledger.flush(ctx, now.Add(6*time.Second), emit); err != nil {
		t.Fatal(err)
	}
	if len(ledgerEvents(t, a.ledger)) != 2 {
		t.Fatal("tool-free continuation missing or duplicated")
	}
}

func testClaude(t *testing.T) (*claudeAdapter, func(event.Event) error) {
	t.Helper()
	c := sourceConfig(t)
	s := config.Source{ID: "claude-main", Harness: "claude-code", Home: filepath.Join(t.TempDir(), "Claude 配置"), EventVersion: 2, Enabled: true}
	c.Sources = []config.Source{s}
	a := newClaude(c, s).(*claudeAdapter)
	return a, func(e event.Event) error {
		_, err := (store.Queue{Dir: c.StateDir}).Enqueue(context.Background(), e, []string{"test"})
		return err
	}
}

func TestClaudeStopResumeAndErrorAcrossRestart(t *testing.T) {
	a, emit := testClaude(t)
	ctx, now := context.Background(), time.Now().UTC()
	stop := []byte(`{"hook_event_name":"Stop","session_id":"s","prompt_id":"p","cwd":"D:/项目","last_assistant_message":"Which task next?"}`)
	if err := a.ConsumeHook(ctx, stop, now, emit); err != nil {
		t.Fatal(err)
	}
	a = newClaude(a.config, a.source).(*claudeAdapter)
	if err := a.ledger.flush(ctx, now.Add(time.Second), emit); err != nil {
		t.Fatal(err)
	}
	if err := a.ConsumeHook(ctx, stop, now.Add(2*time.Second), emit); err != nil {
		t.Fatal(err)
	}
	if err := a.ConsumeHook(ctx, []byte(`{"hook_event_name":"StopFailure","session_id":"s","prompt_id":"p","error":"rate_limit"}`), now.Add(3*time.Second), emit); err != nil {
		t.Fatal(err)
	}
	if events := ledgerEvents(t, a.ledger); len(events) != 1 || events[0].Kind != event.AttentionRequired || events[0].Reason != "stop_signal" || events[0].TurnID != "" || events[0].Project != "项目" {
		t.Fatalf("bad initial handoff: %+v", events)
	}
	if err := a.ConsumeHook(ctx, []byte(`{"hook_event_name":"PreToolUse","session_id":"s","prompt_id":"p","tool_name":"Read","tool_use_id":"tool-2"}`), now.Add(4*time.Second), emit); err != nil {
		t.Fatal(err)
	}
	if err := a.ConsumeHook(ctx, []byte(`{"hook_event_name":"StopFailure","session_id":"s","prompt_id":"p","error":"rate_limit"}`), now.Add(5*time.Second), emit); err != nil {
		t.Fatal(err)
	}
	events := ledgerEvents(t, a.ledger)
	if len(events) != 2 {
		t.Fatalf("resumed handoff missing: %+v", events)
	}
	found := false
	for _, e := range events {
		if e.Reason == "error" && e.Detail == "rate_limit" && e.Project == "Claude Code" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing API error reminder")
	}
}

func TestClaudeRequestsSuppressStopAndResolveSynchronously(t *testing.T) {
	for _, pair := range [][2]string{
		{`{"hook_event_name":"PreToolUse","tool_name":"AskUserQuestion","tool_use_id":"q","tool_input":{"questions":[{"question":"选择？","options":[{"label":"继续"},{"label":"结束"}]}]}}`, `{"hook_event_name":"PostToolUse","tool_use_id":"q"}`},
		{`{"hook_event_name":"PermissionRequest","tool_name":"Bash","tool_use_id":"q"}`, `{"hook_event_name":"PostToolUseFailure","tool_use_id":"q"}`},
		{`{"hook_event_name":"Elicitation","elicitation_id":"q","message":"需要你回复"}`, `{"hook_event_name":"ElicitationResult","elicitation_id":"q"}`},
	} {
		t.Run(pair[0], func(t *testing.T) {
			a, emit := testClaude(t)
			ctx, now := context.Background(), time.Now().UTC()
			consume := func(raw string, offset time.Duration) {
				t.Helper()
				var h map[string]any
				_ = json.Unmarshal([]byte(raw), &h)
				h["session_id"], h["prompt_id"] = "s", "p"
				data, _ := json.Marshal(h)
				if err := a.ConsumeHook(ctx, data, now.Add(offset), emit); err != nil {
					t.Fatal(err)
				}
			}
			consume(pair[0], 0)
			consume(pair[0], time.Millisecond)
			consume(`{"hook_event_name":"Stop"}`, 2*time.Millisecond)
			if err := a.ledger.flush(ctx, now.Add(2*time.Second), emit); err != nil {
				t.Fatal(err)
			}
			if events := ledgerEvents(t, a.ledger); len(events) != 1 || events[0].RequestID != "q" {
				t.Fatalf("request/stop duplicated: %+v", events)
			}
			consume(pair[1], 3*time.Second)
			consume(`{"hook_event_name":"Stop"}`, 4*time.Second)
			if err := a.ledger.flush(ctx, now.Add(5*time.Second), emit); err != nil {
				t.Fatal(err)
			}
			if len(ledgerEvents(t, a.ledger)) != 2 {
				t.Fatal("resolved request hid next handoff")
			}
		})
	}
}

func TestClaudeFiltersNonHandoffsAndSubagents(t *testing.T) {
	a, emit := testClaude(t)
	for _, raw := range []string{
		`{"hook_event_name":"Notification","session_id":"s","notification_type":"idle_prompt"}`,
		`{"hook_event_name":"SessionEnd","session_id":"s","reason":"other"}`,
		`{"hook_event_name":"SubagentStop","session_id":"s","agent_id":"a"}`,
		`{"hook_event_name":"Stop","session_id":"s","agent_id":"a"}`,
		`{"hook_event_name":"Stop","session_id":"s","transcript_path":"C:\\p\\subagents\\a.jsonl"}`,
		`{"hook_event_name":"TaskCompleted","session_id":"s","task_id":"a"}`,
	} {
		if err := a.ConsumeHook(context.Background(), []byte(raw), time.Now(), emit); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.ledger.flush(context.Background(), time.Now().Add(2*time.Second), emit); err != nil {
		t.Fatal(err)
	}
	if len(ledgerEvents(t, a.ledger)) != 0 {
		t.Fatal("non-root or idle event notified")
	}
	if err := a.ConsumeHook(context.Background(), []byte(`{"hook_event_name":"Stop"}`), time.Now(), emit); err == nil {
		t.Fatal("accepted missing session identity")
	}
}

func TestClaudeLegacyPromptSequenceSurvivesRestart(t *testing.T) {
	a, emit := testClaude(t)
	ctx, now := context.Background(), time.Now().UTC()
	prompt := []byte(`{"hook_event_name":"UserPromptSubmit","session_id":"s","prompt":"continue"}`)
	stop := []byte(`{"hook_event_name":"Stop","session_id":"s"}`)
	for n := range 2 {
		at := now.Add(time.Duration(n) * 3 * time.Second)
		if err := a.ConsumeHook(ctx, prompt, at, emit); err != nil {
			t.Fatal(err)
		}
		if err := a.ConsumeHook(ctx, prompt, at.Add(time.Millisecond), emit); err != nil {
			t.Fatal(err)
		}
		a = newClaude(a.config, a.source).(*claudeAdapter)
		if err := a.ConsumeHook(ctx, stop, at.Add(2*time.Millisecond), emit); err != nil {
			t.Fatal(err)
		}
		if err := a.ledger.flush(ctx, at.Add(2*time.Second), emit); err != nil {
			t.Fatal(err)
		}
	}
	if events := ledgerEvents(t, a.ledger); len(events) != 2 || events[0].ID == events[1].ID {
		t.Fatalf("legacy turns collapsed: %+v", events)
	}
}

func TestClaudeInstallPreservesOtherHooksAndDetectsEdits(t *testing.T) {
	a, _ := testClaude(t)
	ctx := context.Background()
	path := filepath.Join(a.source.Home, "settings.json")
	original := []byte(`{"permissions":{"deny":["Bash(rm *)"]},"hooks":{"Stop":[{"matcher":"","hooks":[{"type":"command","command":"my-own-hook"}]}]}}`)
	if err := store.WriteFile(path, original); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), "中文 $ & ' app.exe")
	if err := a.Install(ctx, executable); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)
	if err := a.Install(ctx, executable); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) || a.InstallationStatus() != "configured_unverified" {
		t.Fatal("install not idempotent")
	}
	root, _, _, _ := readHookSettings(path)
	groups := root["hooks"].(map[string]any)["Stop"].([]any)
	owned := groups[1].(map[string]any)["hooks"].([]any)[0].(map[string]any)
	owned["timeout"] = float64(99)
	if err := store.WriteJSON(path, root); err != nil {
		t.Fatal(err)
	}
	if err := a.Uninstall(ctx); err == nil {
		t.Fatal("silently removed edited hook")
	}
	if a.InstallationStatus() != "missing_configuration" {
		t.Fatal("doctor missed modified hook")
	}
	if err := store.WriteFile(path, first); err != nil {
		t.Fatal(err)
	}
	if err := a.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	root, _, _, _ = readHookSettings(path)
	var expected map[string]any
	_ = json.Unmarshal(original, &expected)
	if !jsonEqual(root, expected) {
		t.Fatalf("uninstall changed user settings: %+v", root)
	}
	if a.InstallationStatus() != "not_installed" {
		t.Fatal("manifest retained after uninstall")
	}
}

func TestClaudeInstallRefusesDisabledOrCorruptSettings(t *testing.T) {
	for _, raw := range []string{`{"disableAllHooks":true}`, `{"allowManagedHooksOnly":true}`, `null`, `{"hooks":[]}`, `{broken`} {
		t.Run(raw, func(t *testing.T) {
			a, _ := testClaude(t)
			path := filepath.Join(a.source.Home, "settings.json")
			if err := store.WriteFile(path, []byte(raw)); err != nil {
				t.Fatal(err)
			}
			if err := a.Install(context.Background(), filepath.Join(t.TempDir(), "blip.exe")); err == nil {
				t.Fatal("accepted disabled or corrupt settings")
			}
			after, _ := os.ReadFile(path)
			if string(after) != raw {
				t.Fatal("settings overwritten")
			}
		})
	}
}
