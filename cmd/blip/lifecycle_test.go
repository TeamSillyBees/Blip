package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/service"
	"github.com/TeamSillyBees/Blip/internal/store"
)

func TestLifecycleUsesRegisteredServiceAndNeverFallsBackOnFailure(t *testing.T) {
	failure := errors.New("access denied")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := store.WriteJSON(path, config.Config{SchemaVersion: 2, Sources: []config.Source{}}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		action    string
		installed bool
		queryErr  error
		stopErr   error
		want      []string
		wantErr   bool
	}{
		{name: "ordinary worker", want: []string{"stop worker", "start worker"}},
		{name: "unsupported platform", queryErr: service.ErrUnsupported, want: []string{"stop worker", "start worker"}},
		{name: "installed service", installed: true, want: []string{"stop service", "start service"}},
		{name: "start installed service", action: "start", installed: true, want: []string{"start service"}},
		{name: "stop installed service", action: "stop", installed: true, want: []string{"stop service"}},
		{name: "start ordinary worker", action: "start", want: []string{"start worker"}},
		{name: "stop ordinary worker", action: "stop", want: []string{"stop worker"}},
		{name: "query denied", queryErr: failure, wantErr: true},
		{name: "service stop denied", installed: true, stopErr: failure, want: []string{"stop service"}, wantErr: true},
		{name: "worker did not stop", stopErr: failure, want: []string{"stop worker"}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls []string
			m := lifecycle{
				query:        func(string) (service.Status, error) { return service.Status{Installed: test.installed}, test.queryErr },
				startService: func(context.Context, string) error { calls = append(calls, "start service"); return nil },
				stopService:  func(context.Context, string) error { calls = append(calls, "stop service"); return test.stopErr },
				startWorker: func(context.Context, config.Config, io.Writer) error {
					calls = append(calls, "start worker")
					return nil
				},
				stopWorker: func(context.Context, config.Config, io.Writer) error {
					calls = append(calls, "stop worker")
					return test.stopErr
				},
			}
			action := test.action
			if action == "" {
				action = "restart"
			}
			err := m.run(context.Background(), path, action, io.Discard)
			if (err != nil) != test.wantErr || !reflect.DeepEqual(calls, test.want) {
				t.Fatalf("calls=%v err=%v; want calls=%v error=%v", calls, err, test.want, test.wantErr)
			}
		})
	}
}

func TestRestartValidatesChannelsBeforeStoppingCurrentWorker(t *testing.T) {
	dir := t.TempDir()
	c := config.Config{SchemaVersion: 1, CodexHome: filepath.Join(dir, "codex"), StateDir: filepath.Join(dir, "state"), Destinations: []config.Destination{{ID: "broken", Kind: "webhook", Enabled: true, URL: "invalid-url"}}}
	path := filepath.Join(dir, "config.json")
	if err := store.WriteJSON(path, c); err != nil {
		t.Fatal(err)
	}
	lock, available, err := (store.Queue{Dir: c.StateDir}).WorkerLock()
	if err != nil || !available {
		t.Fatalf("worker lock: %v", err)
	}
	defer lock.Close()
	if err := execute(context.Background(), path, []string{"restart"}, nil, io.Discard); err == nil {
		t.Fatal("restart accepted invalid channel settings")
	}
	if _, err := os.Stat(filepath.Join(c.StateDir, "stop")); !os.IsNotExist(err) {
		t.Fatal("invalid configuration interrupted the running worker")
	}
}

func TestCancelledStopDoesNotWriteStopRequest(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := stop(ctx, config.Config{StateDir: dir}, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop ignored cancellation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "stop")); !os.IsNotExist(err) {
		t.Fatal("cancelled stop wrote a request")
	}
}
