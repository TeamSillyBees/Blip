package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

func TestWatchedRolloutsAndMissedPublications(t *testing.T) {
	ctx := context.Background()
	c := Collector{SessionsDir: t.TempDir(), StatePath: filepath.Join(t.TempDir(), "cursors.json")}
	c.Watch()
	defer c.Close()
	var turns []string
	poll := func() {
		t.Helper()
		if err := c.Poll(ctx, func(e event.Event) error { turns = append(turns, e.TurnID); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	awaitTurns := func(want ...string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for {
			poll()
			if slices.Equal(turns, want) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("received %v, want %v", turns, want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	poll()
	if c.watch.native == nil {
		t.Skip("native directory notifications unavailable")
	}
	dir := filepath.Join(c.SessionsDir, "new", "date")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-new.jsonl")
	content := meta("s", "user") + completion("one", "task_complete")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	awaitTurns("one")

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Equal size and timestamp: the named notification must still validate
	// the cursor anchor before deciding this file has already been consumed.
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(content, `"one"`, `"two"`)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	awaitTurns("one", "two")

	if err := c.watch.native.Remove(dir); err != nil {
		t.Fatal(err)
	}
	appendFile(t, path, completion("missed", "task_complete"))
	c.watch.rescan() // Exercise the periodic directory reconciliation immediately.
	awaitTurns("one", "two", "missed")
	// Real Codex sessions can keep the log open between turns.
	writer, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.WriteString(completion("rewatched", "task_complete")); err != nil {
		t.Fatal(err)
	}
	awaitTurns("one", "two", "missed", "rewatched")
}

func TestRolloutCheckpointWrittenOnlyForProgressAndRetriedOnError(t *testing.T) {
	ctx := context.Background()
	c := Collector{SessionsDir: t.TempDir(), StatePath: filepath.Join(t.TempDir(), "cursors.json")}
	emit := func(event.Event) error { return nil }
	path := filepath.Join(c.SessionsDir, "rollout-static.jsonl")
	if err := os.WriteFile(path, []byte(meta("s", "user")), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.Poll(ctx, emit); err != nil {
		t.Fatal(err)
	}
	old := time.Unix(1700000000, 0)
	if err := os.Chtimes(c.StatePath, old, old); err != nil {
		t.Fatal(err)
	}
	appendFile(t, path, "{") // An incomplete line changes size but cannot advance the cursor.
	if err := c.Poll(ctx, emit); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(c.StatePath); err != nil || !info.ModTime().Equal(old) {
		t.Fatal("checkpoint changed without consumable input", err)
	}
	// Force publication to fail after progress. The next poll must persist
	// the advanced cursor even when the source no longer changes.
	parent := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(parent, nil, 0600); err != nil {
		t.Fatal(err)
	}
	c.StatePath = filepath.Join(parent, "cursors.json")
	appendFile(t, path, "}\n")
	if err := c.Poll(ctx, emit); err == nil {
		t.Fatal("expected checkpoint publication to fail")
	}
	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	if err := c.Poll(ctx, emit); err != nil {
		t.Fatal(err)
	}
	var saved checkpoint
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReadJSON(c.StatePath, &saved); err != nil || saved.Files[path] == nil || saved.Files[path].Offset != info.Size() {
		t.Fatal("checkpoint retry lost progress", err)
	}
}

func TestFingerprintRecheckRecoversUnreportedEqualSizeReplacement(t *testing.T) {
	// A filesystem without a usable watcher can miss every native hint.
	c := Collector{SessionsDir: t.TempDir(), StatePath: filepath.Join(t.TempDir(), "cursors.json")}
	c.Watch()
	defer c.Close()
	c.watch.retryAt = time.Now().Add(time.Hour)
	var turns []string
	poll := func() {
		t.Helper()
		if err := c.Poll(context.Background(), func(e event.Event) error {
			turns = append(turns, e.TurnID)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	poll()
	path := filepath.Join(c.SessionsDir, "rollout-replace.jsonl")
	content := meta("s", "user") + completion("old", "task_complete")
	stamp := time.Unix(1700000000, 0)
	write := func(data string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	write(content)
	poll()
	write(strings.ReplaceAll(content, `"old"`, `"new"`))
	poll()
	if !slices.Equal(turns, []string{"old"}) {
		t.Fatalf("unexpected turns before fingerprint recheck: %v", turns)
	}
	c.state.Files[path].checkedAt = time.Now().Add(-5 * time.Minute)
	poll()
	if !slices.Equal(turns, []string{"old", "new"}) {
		t.Fatalf("fingerprint recheck missed replacement: %v", turns)
	}
}

func TestInterruptedBaselineResumesAcrossRestart(t *testing.T) {
	c := Collector{SessionsDir: t.TempDir(), StatePath: filepath.Join(t.TempDir(), "cursors.json")}
	baseline := time.Now().Add(-time.Minute).UTC()
	c.loaded, c.state = true, checkpoint{BaselineAt: baseline, Files: map[string]*cursor{}}
	path := filepath.Join(c.SessionsDir, "rollout-session.jsonl")
	if err := os.WriteFile(path, []byte(meta("s", "user")+completion("one", "task_complete")+completion("two", "task_complete")), 0600); err != nil {
		t.Fatal(err)
	}
	var turns []string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := c.Poll(ctx, func(e event.Event) error {
		turns = append(turns, e.TurnID)
		cancel()
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected interrupted scan, got %v", err)
	}
	c = Collector{SessionsDir: c.SessionsDir, StatePath: c.StatePath}
	if err := c.Poll(context.Background(), func(e event.Event) error { turns = append(turns, e.TurnID); return nil }); err != nil {
		t.Fatal(err)
	}
	if !c.state.BaselineAt.Equal(baseline) || c.state.Baselining || !slices.Equal(turns, []string{"one", "two"}) {
		t.Fatalf("restart lost original baseline or progress: %v", turns)
	}
}

func TestWatchedRolloutDrainsRecordBudgetWithoutAnotherWrite(t *testing.T) {
	c := Collector{SessionsDir: t.TempDir(), StatePath: filepath.Join(t.TempDir(), "cursors.json")}
	c.Watch()
	defer c.Close()
	var turns []string
	emit := func(e event.Event) error { turns = append(turns, e.TurnID); return nil }
	if err := c.Poll(context.Background(), emit); err != nil {
		t.Fatal(err)
	}
	content := meta("s", "user") + strings.Repeat("{}\n", 2100) + completion("tail", "task_complete")
	if err := os.WriteFile(filepath.Join(c.SessionsDir, "rollout-backlog.jsonl"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(turns) == 0 && time.Now().Before(deadline) {
		if err := c.Poll(context.Background(), emit); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !slices.Equal(turns, []string{"tail"}) {
		t.Fatalf("unconsumed records waited for another write: %v", turns)
	}
}
