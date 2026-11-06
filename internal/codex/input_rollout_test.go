package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

func inputRecord(kind string, payload any) string {
	data, _ := json.Marshal(map[string]any{"timestamp": time.Now().UTC(), "type": kind, "payload": payload})
	return string(data) + "\n"
}

func inputContext(turn string) string {
	return inputRecord("turn_context", map[string]any{"turn_id": turn, "cwd": `C:\项目\Blip`})
}

func inputCall(name, id string) string {
	return inputRecord("response_item", map[string]any{"type": "function_call", "name": name, "call_id": id,
		"arguments": `{"questions":[{"title":"请选择通知渠道","options":["Gotify","飞书"]}]}`})
}

func TestDesktopInputRolloutFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "desktop-input-0.167.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rollout-desktop.jsonl"), data, 0600); err != nil {
		t.Fatal(err)
	}
	c := Collector{SessionsDir: dir, StatePath: filepath.Join(dir, "cursor.json"), loaded: true,
		state: checkpoint{Initialized: true, BaselineAt: time.Unix(1, 0), Files: map[string]*cursor{}}}
	var events []event.Event
	if err := c.Poll(context.Background(), func(e event.Event) error { events = append(events, e); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].SessionID != "desktop-root" || events[0].TurnID != "native-turn" || events[0].RequestID != "call-desktop-input" || events[0].Questions[0].Options[1] != "飞书" {
		t.Fatalf("desktop request not captured: %+v", events)
	}
}

func TestRolloutInputRecoversActiveTurnFromBaselineAndLegacyCursor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout-active.jsonl")
	if err := os.WriteFile(path, []byte(meta("root", "user")+inputContext("active")+inputCall("request_user_input_async", "historical")), 0600); err != nil {
		t.Fatal(err)
	}
	c := Collector{SessionsDir: dir, StatePath: filepath.Join(dir, "cursor.json")}
	var events []event.Event
	emit := func(e event.Event) error { events = append(events, e); return nil }
	if err := c.Poll(context.Background(), emit); err != nil || len(events) != 0 {
		t.Fatalf("baseline replayed old input: %+v %v", events, err)
	}
	appendFile(t, path, inputCall("request_user_input_async", "new"))
	if err := c.Poll(context.Background(), emit); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].TurnID != "active" || events[0].Kind != event.InputRequested || events[0].Source != "codex/rollout" || events[0].Questions[0].Options[1] != "飞书" {
		t.Fatalf("active request was lost: %+v", events)
	}
	// Simulate a pre-upgrade checkpoint. Its byte offset must survive intact.
	c.state.Files[path].InputTurn = nil
	if err := store.WriteJSON(c.StatePath, c.state); err != nil {
		t.Fatal(err)
	}
	c = Collector{SessionsDir: dir, StatePath: c.StatePath}
	appendFile(t, path, inputCall("functions.request_user_input", "legacy-upgrade"))
	if err := c.Poll(context.Background(), emit); err != nil || len(events) != 2 || events[1].TurnID != "active" {
		t.Fatalf("legacy upgrade lost identity or replayed history: %+v %v", events, err)
	}
	c = Collector{SessionsDir: dir, StatePath: c.StatePath}
	if err := c.Poll(context.Background(), emit); err != nil || len(events) != 2 {
		t.Fatalf("restart repeated input: %+v %v", events, err)
	}
}

func TestRolloutInputAndHookShareOneDurableIdentity(t *testing.T) {
	for _, hookFirst := range []bool{true, false} {
		dir := t.TempDir()
		c := Collector{SessionsDir: dir, StatePath: filepath.Join(dir, "cursor.json")}
		q := store.Queue{Dir: filepath.Join(dir, "queue")}
		emit := func(e event.Event) error { _, err := q.Enqueue(context.Background(), e, []string{"one"}); return err }
		if err := c.Poll(context.Background(), emit); err != nil {
			t.Fatal(err)
		}
		hook, err := ParseHook(strings.NewReader(`{"hook_event_name":"PreToolUse","session_id":"root","turn_id":"active","tool_name":"request_user_input_async","tool_use_id":"same-call","tool_input":{"questions":[{"title":"请选择通知渠道","options":["Gotify","飞书"]}]}}`), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if hookFirst {
			if err := emit(*hook); err != nil {
				t.Fatal(err)
			}
		}
		path := filepath.Join(dir, "rollout-root.jsonl")
		if err := os.WriteFile(path, []byte(meta("root", "user")+inputContext("active")+inputCall("request_user_input_async", "same-call")), 0600); err != nil {
			t.Fatal(err)
		}
		if err := c.Poll(context.Background(), emit); err != nil {
			t.Fatal(err)
		}
		if err := emit(*hook); err != nil {
			t.Fatal(err)
		}
		jobs, err := q.Load()
		if err != nil || len(jobs) != 1 || jobs[0].Event.ID != hook.ID {
			t.Fatalf("hook/rollout double delivery: %+v %v", jobs, err)
		}
		wantSource := "codex/rollout"
		if hookFirst {
			wantSource = "codex/hooks"
		}
		if jobs[0].Event.Source != wantSource {
			t.Fatal("late path overwrote original snapshot")
		}
	}
}

func TestRolloutInputRequiresRootNativeTurnAndStructuredQuestion(t *testing.T) {
	for _, test := range []struct{ name, source, body string }{
		{"missing turn", "user", inputCall("request_user_input_async", "one")},
		{"subagent", "subagent", inputContext("t") + inputCall("request_user_input_async", "one")},
		{"unrelated tool", "user", inputContext("t") + inputCall("exec", "one")},
		{"ordinary text", "user", inputContext("t") + inputRecord("response_item", map[string]any{"type": "message", "content": "选哪个？"})},
		{"no call ID", "user", inputContext("t") + inputCall("request_user_input_async", "")},
		{"completed", "user", inputContext("t") + completion("t", "task_complete") + inputCall("request_user_input_async", "one")},
		{"cancelled", "user", inputContext("t") + completion("t", "turn_aborted") + inputCall("request_user_input_async", "one")},
		{"malformed arguments", "user", inputContext("t") + inputRecord("response_item", map[string]any{"type": "function_call", "name": "request_user_input_async", "call_id": "one", "arguments": "{broken"})},
		{"empty questions", "user", inputContext("t") + inputRecord("response_item", map[string]any{"type": "function_call", "name": "request_user_input_async", "call_id": "one", "arguments": `{"questions":[]}`})},
		{"tool output", "user", inputContext("t") + inputRecord("response_item", map[string]any{"type": "function_call_output", "name": "request_user_input_async", "call_id": "one", "output": `{"accepted":true}`})},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			c := Collector{SessionsDir: dir, StatePath: filepath.Join(dir, "cursor.json")}
			emit := func(e event.Event) error {
				if e.Kind == event.InputRequested {
					t.Fatalf("unproven question emitted: %+v", e)
				}
				return nil
			}
			if err := c.Poll(context.Background(), emit); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "rollout-test.jsonl"), []byte(meta("s", test.source)+test.body), 0600); err != nil {
				t.Fatal(err)
			}
			if err := c.Poll(context.Background(), emit); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRestoreInputTurnAcrossLargeRecordsAndCancellation(t *testing.T) {
	contextLine := inputContext("native-turn")
	for _, filler := range []string{strings.Repeat("x", 65500) + "\n", strings.Repeat("x", maxRecordBytes+100) + "\n"} {
		content := meta("s", "user") + contextLine + filler
		turn, err := restoreInputTurn(context.Background(), strings.NewReader(content), int64(len(content)))
		if err != nil || turn.ID != "native-turn" {
			t.Fatalf("could not recover across chunk boundary: %+v %v", turn, err)
		}
		content += completion("native-turn", "task_complete")
		turn, err = restoreInputTurn(context.Background(), strings.NewReader(content), int64(len(content)))
		if err != nil || turn.ID != "" {
			t.Fatalf("restored completed turn: %+v %v", turn, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := restoreInputTurn(ctx, strings.NewReader(content), int64(len(content))); !errors.Is(err, context.Canceled) {
			t.Fatalf("restoration ignored cancellation: %v", err)
		}
	}
}
