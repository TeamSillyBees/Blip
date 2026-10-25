package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

func meta(id, source string) string {
	data, _ := json.Marshal(map[string]any{"timestamp": time.Now().UTC(), "type": "session_meta", "payload": map[string]any{"id": id, "session_id": id, "cwd": "C:\\项目\\Blip", "source": "vscode", "thread_source": source}})
	return string(data) + "\n"
}
func completion(turn, kind string) string {
	data, _ := json.Marshal(map[string]any{"timestamp": time.Now().UTC(), "type": "event_msg", "payload": map[string]any{"type": kind, "turn_id": turn, "root_turn_id": turn, "last_agent_message": "回复完成"}})
	return string(data) + "\n"
}
func appendFile(t *testing.T, path, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCollectorBaselinePartialLinesRestartAndSubagents(t *testing.T) {
	dir := t.TempDir()
	sessions := filepath.Join(dir, "sessions")
	_ = os.MkdirAll(sessions, 0700)
	root := filepath.Join(sessions, "rollout-root.jsonl")
	_ = os.WriteFile(root, []byte(meta("root", "user")+completion("historical", "task_complete")), 0600)
	checkpointPath := filepath.Join(dir, "cursor.json")
	collector := Collector{SessionsDir: sessions, StatePath: checkpointPath}
	var events []event.Event
	emit := func(e event.Event) error { events = append(events, e); return nil }
	if err := collector.Poll(context.Background(), emit); err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatal("first start replayed history")
	}
	line := completion("new-turn", "task_complete")
	appendFile(t, root, line[:len(line)/2])
	if err := collector.Poll(context.Background(), emit); err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatal("partial record was emitted")
	}
	collector = Collector{SessionsDir: sessions, StatePath: checkpointPath}
	appendFile(t, root, line[len(line)/2:])
	if err := collector.Poll(context.Background(), emit); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].TurnID != "new-turn" {
		t.Fatalf("completion lost: %+v", events)
	}
	appendFile(t, root, completion("aborted", "turn_aborted"))
	_ = collector.Poll(context.Background(), emit)
	collector = Collector{SessionsDir: sessions, StatePath: checkpointPath}
	appendFile(t, root, completion("aborted", "task_complete"))
	child := filepath.Join(sessions, "rollout-child.jsonl")
	_ = os.WriteFile(child, []byte(meta("child", "subagent")+completion("child-turn", "task_complete")), 0600)
	appendFile(t, root, strings.Replace(completion("failed", "task_complete"), `"type":"task_complete"`, `"status":"failed","type":"task_complete"`, 1))
	if err := collector.Poll(context.Background(), emit); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatal("interrupted, failed, or subagent event emitted")
	}
	// Rewritten files are read from the new beginning, with a refreshed session identity.
	_ = os.WriteFile(root, []byte(meta("replacement", "user")+completion("short", "task_complete")), 0600)
	if err := collector.Poll(context.Background(), emit); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].SessionID != "replacement" {
		t.Fatal("truncated/replaced file was not handled")
	}
}

func TestCollectorPersistsBeforeRestartAndReplayIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	sessions := filepath.Join(dir, "sessions")
	_ = os.MkdirAll(sessions, 0700)
	c := Collector{SessionsDir: sessions, StatePath: filepath.Join(dir, "cursor.json")}
	queue := store.Queue{Dir: filepath.Join(dir, "queue")}
	emit := func(e event.Event) error {
		_, err := queue.Enqueue(context.Background(), e, []string{"one"})
		return err
	}
	if err := c.Poll(context.Background(), emit); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sessions, "rollout-new.jsonl")
	_ = os.WriteFile(path, []byte(meta("s", "user")+completion("t", "task_complete")), 0600)
	if err := c.Poll(context.Background(), emit); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after enqueue but before the cursor checkpoint.
	var prior checkpoint
	prior.Initialized = true
	prior.BaselineAt = time.Now().Add(-time.Hour)
	prior.Files = map[string]*cursor{}
	_ = store.WriteJSON(c.StatePath, prior)
	c = Collector{SessionsDir: sessions, StatePath: c.StatePath}
	if err := c.Poll(context.Background(), emit); err != nil {
		t.Fatal(err)
	}
	jobs, err := queue.Load()
	if err != nil || len(jobs) != 1 {
		t.Fatal("replay duplicated the job")
	}
}

func TestRootIdentityMustBePositive(t *testing.T) {
	for _, metadata := range []Metadata{{ID: "x", Source: json.RawMessage(`{"subagent":{"thread_spawn":{}}}`)}, {ID: "x", Source: json.RawMessage(`"unknown"`)}, {ID: "child", SessionID: "parent", ThreadSource: "user"}, {ID: "child", SessionID: "parent", ThreadSource: "user", Source: json.RawMessage(`{"client":"app"}`)}} {
		if metadata.IsRoot() {
			t.Fatal("non-root session accepted")
		}
	}
	if !(Metadata{ID: "cli", Source: json.RawMessage(`"cli"`)}).IsRoot() {
		t.Fatal("legacy CLI root rejected")
	}
}

func TestLaterDiscoveredHistoricalFilesDoNotFloodNotifications(t *testing.T) {
	dir := t.TempDir()
	sessions := filepath.Join(dir, "sessions")
	_ = os.MkdirAll(sessions, 0700)
	c := Collector{SessionsDir: sessions, StatePath: filepath.Join(dir, "cursor.json")}
	count := 0
	emit := func(event.Event) error { count++; return nil }
	if err := c.Poll(context.Background(), emit); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339Nano)
	old := completion("old", "task_complete")
	var line map[string]any
	_ = json.Unmarshal([]byte(old), &line)
	line["timestamp"] = oldTime
	data, _ := json.Marshal(line)
	_ = os.WriteFile(filepath.Join(sessions, "rollout-restored.jsonl"), []byte(meta("s", "user")+string(data)+"\n"), 0600)
	if err := c.Poll(context.Background(), emit); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("old records discovered after initialization were pushed")
	}
}
