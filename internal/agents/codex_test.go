package agents

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

func TestCodexV2HookLifecycleAndInstallation(t *testing.T) {
	c := sourceConfig(t)
	a := newCodex(c, c.Sources[0]).(*codexAdapter)
	ctx := context.Background()
	now := time.Now().UTC()
	emit := func(e event.Event) error {
		_, err := (store.Queue{Dir: c.StateDir}).Enqueue(ctx, e, []string{"test"})
		return err
	}
	hook := func(name, turn, agent string) {
		t.Helper()
		data, _ := json.Marshal(map[string]any{"hook_event_name": name, "session_id": "s", "turn_id": turn, "agent_id": agent, "cwd": "/project"})
		if err := a.ConsumeHook(ctx, data, now, emit); err != nil {
			t.Fatal(err)
		}
	}
	hook("Stop", "stop", "")
	hook("Stop", "cancel", "")
	hook("Interrupt", "cancel", "")
	hook("Stop", "child", "subagent")
	if err := a.ledger.flush(ctx, now.Add(time.Second), emit); err != nil {
		t.Fatal(err)
	}
	jobs, err := (store.Queue{Dir: c.StateDir}).Load()
	if err != nil || len(jobs) != 1 || jobs[0].Event.TurnID != "stop" || jobs[0].Event.Kind != event.AttentionRequired {
		t.Fatalf("bad hook lifecycle: %+v %v", jobs, err)
	}
	if err = a.Install(ctx, filepath.Join(t.TempDir(), "blip.exe")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(c.Sources[0].Home, "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var installed struct {
		Hooks map[string][]map[string]any `json:"hooks"`
	}
	if err = json.Unmarshal(data, &installed); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Stop", "Interrupt", "UserPromptSubmit", "PreToolUse", "PostToolUse", "PermissionRequest"} {
		if len(installed.Hooks[name]) != 1 {
			t.Fatalf("missing v2 %s hook", name)
		}
	}
	if _, ok := installed.Hooks["PreToolUse"][0]["matcher"]; ok {
		t.Fatal("v2 cannot observe resumed tool execution")
	}
}

func TestCodexV2RolloutFailureAndCancellation(t *testing.T) {
	c := sourceConfig(t)
	a := newCodex(c, c.Sources[0]).(*codexAdapter)
	ctx := context.Background()
	emit := func(e event.Event) error {
		_, err := (store.Queue{Dir: c.StateDir}).Enqueue(ctx, e, []string{"test"})
		return err
	}
	if err := a.Poll(ctx, emit); err != nil {
		t.Fatal(err)
	}
	lines := `{"type":"session_meta","payload":{"id":"s","source":"cli"}}` + "\n"
	for _, item := range []struct{ kind, turn string }{{"turn_failed", "failure"}, {"task_complete", "failure"}, {"turn_aborted", "cancel"}, {"task_complete", "cancel"}, {"task_complete", "normal"}} {
		line, _ := json.Marshal(map[string]any{"timestamp": time.Now().UTC(), "type": "event_msg", "payload": map[string]any{"type": item.kind, "turn_id": item.turn}})
		lines += string(line) + "\n"
	}
	if err := store.WriteFile(filepath.Join(c.Sources[0].Home, "sessions", "rollout-test.jsonl"), []byte(lines)); err != nil {
		t.Fatal(err)
	}
	if err := a.Poll(ctx, emit); err != nil {
		t.Fatal(err)
	}
	jobs, err := (store.Queue{Dir: c.StateDir}).Load()
	if err != nil || len(jobs) != 2 {
		t.Fatalf("bad rollout lifecycle: %+v %v", jobs, err)
	}
	for _, job := range jobs {
		if job.Event.TurnID == "failure" && (job.Event.Kind != event.AttentionRequired || job.Event.Reason != "error") {
			t.Fatal("failure labelled complete")
		}
		if job.Event.TurnID == "cancel" {
			t.Fatal("manual cancellation notified")
		}
	}
}
