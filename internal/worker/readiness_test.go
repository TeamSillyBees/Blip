package worker

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/codex"
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
	deadline := time.Now().Add(2 * time.Second)
	for {
		var current Status
		if store.ReadJSON(filepath.Join(c.StateDir, "worker.json"), &current) == nil && current.Heartbeat.After(initial.Heartbeat) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not complete another collection pass")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if calls.Load() != 1 {
		t.Fatal("readiness was reported more than once")
	}
}

func TestReadyCallbackDoesNotRunOnFailedStartup(t *testing.T) {
	dir := t.TempDir()
	c := config.Config{StateDir: filepath.Join(dir, "state"), CodexHome: filepath.Join(dir, "codex"), PollInterval: "100ms", RequestTimeout: "1s", MaxAttempts: 2}
	if err := store.WriteFile(filepath.Join(c.StateDir, "rollout-cursors.json"), []byte(`{"initialized":true,"files":null}`)); err != nil {
		t.Fatal(err)
	}
	called := false
	err := RunWithReady(context.Background(), c, log.New(io.Discard, "", 0), func() { called = true })
	if !errors.Is(err, codex.ErrCheckpoint) || called {
		t.Fatalf("failed initialization reported readiness: called=%v, error=%v", called, err)
	}
}
