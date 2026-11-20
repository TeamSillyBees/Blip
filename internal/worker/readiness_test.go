package worker

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/store"
)

func TestReadyCallbackRunsOnceAfterStatusIsPublished(t *testing.T) {
	dir := t.TempDir()
	c := config.Config{StateDir: filepath.Join(dir, "state"), CodexHome: filepath.Join(dir, "codex"), PollInterval: "100ms", RequestTimeout: "1s", MaxAttempts: 2}
	if err := os.MkdirAll(filepath.Join(c.CodexHome, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	ready := make(chan Status, 1)
	done := make(chan error, 1)
	go func() {
		done <- RunWithReady(ctx, c, log.New(io.Discard, "", 0), func() {
			calls.Add(1)
			var status Status
			_ = store.ReadJSON(filepath.Join(c.StateDir, "worker.json"), &status)
			ready <- status
		})
	}()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("worker stopped with an error: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("worker did not stop")
		}
	}()
	var initial Status
	select {
	case initial = <-ready:
		if !initial.Ready || initial.PID != os.Getpid() || initial.Heartbeat.IsZero() {
			t.Fatalf("callback ran before the ready status was published: %+v", initial)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("worker never signalled readiness")
	}
	// Several idle collection passes should leave the status file alone.
	time.Sleep(350 * time.Millisecond)
	var idle Status
	if err := store.ReadJSON(filepath.Join(c.StateDir, "worker.json"), &idle); err != nil || !idle.Heartbeat.Equal(initial.Heartbeat) {
		t.Fatalf("idle polling rewrote ready status: %v", err)
	}
	line := `{"type":"session_meta","payload":{"id":"root","source":"cli"}}` + "\n" +
		`{"timestamp":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","type":"event_msg","payload":{"type":"task_complete","turn_id":"new"}}` + "\n"
	if err := os.WriteFile(filepath.Join(c.CodexHome, "sessions", "rollout-new.jsonl"), []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		var current Status
		if store.ReadJSON(filepath.Join(c.StateDir, "worker.json"), &current) == nil && current.Heartbeat.After(initial.Heartbeat) && current.Sources[0].LastEventAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not publish the new event before the heartbeat")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatal("readiness was reported more than once")
	}
}

func TestCorruptCheckpointDegradesSourceWithoutFailingStartup(t *testing.T) {
	dir := t.TempDir()
	c := config.Config{StateDir: filepath.Join(dir, "state"), CodexHome: filepath.Join(dir, "codex"), PollInterval: "100ms", RequestTimeout: "1s", MaxAttempts: 2}
	if err := store.WriteFile(filepath.Join(c.StateDir, "rollout-cursors.json"), []byte(`{"initialized":true,"files":null}`)); err != nil {
		t.Fatal(err)
	}
	called := false
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := RunWithReady(ctx, c, log.New(io.Discard, "", 0), func() {
		called = true
		var status Status
		if err := store.ReadJSON(filepath.Join(c.StateDir, "worker.json"), &status); err != nil || !status.Ready || !status.Degraded || status.Sources[0].ErrorCode != "checkpoint_invalid" {
			t.Errorf("missing degraded status: %+v %v", status, err)
		}
		cancel()
	})
	if err != nil || !called {
		t.Fatalf("source failure stopped common delivery: called=%v, error=%v", called, err)
	}
	data, _ := os.ReadFile(filepath.Join(c.StateDir, "rollout-cursors.json"))
	if string(data) != `{"initialized":true,"files":null}` {
		t.Fatal("corrupt checkpoint was reset")
	}
}

func TestQueueFailureDoesNotReportReady(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(state, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	called := false
	err := RunWithReady(context.Background(), config.Config{StateDir: state}, log.New(io.Discard, "", 0), func() { called = true })
	if err == nil || called {
		t.Fatalf("common queue failure reported ready: %v", err)
	}
}
