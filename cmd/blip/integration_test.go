package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/codex"
	"github.com/TeamSillyBees/Blip/internal/event"
)

// This process test uses an isolated Codex home and a loopback server. It never
// changes the user's Codex configuration or sends a real channel notification.
func TestCLIEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("process integration")
	}
	dir := filepath.Join(t.TempDir(), "中文 路径 空格")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "blip.exe")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", output, err)
	}
	path := filepath.Join(dir, "config.json")
	call := func(args ...string) []byte {
		t.Helper()
		command := exec.Command(binary, append([]string{"--config", path}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("%v failed: %s %v", args, output, err)
		}
		return output
	}
	call("init")
	received := make(chan event.Event, 20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer local-test-only" {
			t.Error("missing custom webhook authentication")
		}
		var notification event.Event
		if err := json.NewDecoder(r.Body).Decode(&notification); err != nil {
			t.Error(err)
		}
		received <- notification
		w.WriteHeader(204)
	}))
	defer server.Close()
	home := filepath.Join(dir, "codex home")
	sessions := filepath.Join(home, "sessions", "2026", "10", "21")
	if err := os.MkdirAll(sessions, 0700); err != nil {
		t.Fatal(err)
	}
	settings := map[string]any{"schema_version": 1, "codex_home": home, "state_dir": filepath.Join(dir, "state"), "poll_interval": "100ms", "request_timeout": "1s", "max_attempts": 2, "destinations": []any{map[string]any{"id": "loopback", "kind": "webhook", "enabled": true, "url": server.URL, "token": "local-test-only"}}}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	metadata := func(id, source string) string {
		data, _ := json.Marshal(map[string]any{"type": "session_meta", "payload": map[string]any{"id": id, "session_id": id, "thread_source": source, "source": "vscode", "cwd": dir}})
		return string(data) + "\n"
	}
	complete := func(turn string) string {
		data, _ := json.Marshal(map[string]any{"timestamp": time.Now().UTC(), "type": "event_msg", "payload": map[string]any{"type": "task_complete", "turn_id": turn, "root_turn_id": turn, "last_agent_message": "完成中文回复"}})
		return string(data) + "\n"
	}
	rollout := filepath.Join(sessions, "rollout-root.jsonl")
	if err := os.WriteFile(rollout, []byte(metadata("root", "user")+complete("historical")), 0600); err != nil {
		t.Fatal(err)
	}
	call("install", "codex")
	t.Cleanup(func() { command := exec.Command(binary, "--config", path, "stop"); _, _ = command.CombinedOutput() })
	call("start")
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(dir, "state", "rollout-cursors.json")); return err == nil })
	_, windows, err := codex.Commands(binary, path)
	if err != nil {
		t.Fatal(err)
	}
	invokeHook := func(payload string) {
		t.Helper()
		command := hookProcess(binary, path, windows)
		command.Stdin = bytes.NewBufferString(payload)
		output, err := command.CombinedOutput()
		if err != nil || len(output) != 0 {
			t.Fatalf("generated hook command failed: %s %v", output, err)
		}
	}
	question := `{"hook_event_name":"PreToolUse","session_id":"root","turn_id":"one","tool_name":"request_user_input_async","tool_use_id":"call-1","tool_input":{"questions":[{"title":"选择渠道？","options":["飞书","Gotify"]}]}}`
	invokeHook(question)
	invokeHook(question)
	invokeHook(`{"hook_event_name":"PermissionRequest","session_id":"root","turn_id":"one","tool_name":"exec_command","tool_use_id":"permission-1","tool_input":{"command":"build","reason":"需要授权"}}`)
	appendRollout := func(content string) {
		t.Helper()
		file, err := os.OpenFile(rollout, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, err = file.WriteString(content)
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	appendRollout(complete("one"))
	_ = os.WriteFile(filepath.Join(sessions, "rollout-child.jsonl"), []byte(metadata("child", "subagent")+complete("child")), 0600)
	kinds := map[event.Kind]int{}
	for i := 0; i < 3; i++ {
		select {
		case notification := <-received:
			kinds[notification.Kind]++
			if notification.TurnID == "historical" {
				t.Fatal("historical completion replayed")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("notification timed out")
		}
	}
	for _, kind := range []event.Kind{event.InputRequested, event.ApprovalRequested, event.TurnCompleted} {
		if kinds[kind] != 1 {
			t.Fatalf("expected one %s: %v", kind, kinds)
		}
	}
	waitFor(t, func() bool {
		var status map[string]any
		_ = json.Unmarshal(call("status"), &status)
		return status["delivered"] == float64(3)
	})
	call("stop")
	question = string(bytes.ReplaceAll([]byte(question), []byte("call-1"), []byte("call-2")))
	invokeHook(question)           // Offline hook input remains durably queued.
	appendRollout(complete("two")) // The persisted cursor must catch up on restart.
	call("start")
	for i := 0; i < 2; i++ {
		select {
		case notification := <-received:
			if notification.TurnID == "historical" {
				t.Fatal("restart replayed history")
			}
		case <-time.After(10 * time.Second):
			t.Fatal("restart did not recover pending work")
		}
	}
	waitFor(t, func() bool {
		var status map[string]any
		_ = json.Unmarshal(call("status"), &status)
		return status["delivered"] == float64(5)
	})
	select {
	case extra := <-received:
		t.Fatalf("duplicate or subagent notification: %+v", extra)
	default:
	}
	call("stop")
	call("uninstall", "codex")
	if output := call("version"); len(output) == 0 {
		t.Fatal("version missing")
	}
	logData, _ := os.ReadFile(filepath.Join(dir, "state", "worker.log"))
	if bytes.Contains(logData, []byte("local-test-only")) {
		t.Fatal("credential leaked to logs")
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("condition not reached before timeout")
}
