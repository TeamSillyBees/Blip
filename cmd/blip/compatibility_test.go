package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/store"
)

func TestScriptTestKeepsOfflineQueueOnlyContract(t *testing.T) {
	for _, waitFlag := range []string{"", "--wait=false"} {
		t.Run(waitFlag, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "配置 with spaces.json")
			// The old command accepts incomplete channel settings for offline
			// queuing; validation and network access belong to the worker.
			c := config.Config{SchemaVersion: 2, Sources: []config.Source{}, StateDir: filepath.Join(dir, "state"), Destinations: []config.Destination{{ID: "legacy-target", Kind: "webhook", Enabled: true, URL: "needs-repair"}}}
			if err := store.WriteJSON(path, c); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			args := []string{"test", "--config", path}
			if waitFlag != "" {
				args = append(args, waitFlag)
			}
			var output bytes.Buffer
			if err := run(args, strings.NewReader(""), &output); err != nil {
				t.Fatalf("offline test command changed: %v", err)
			}
			jobs, err := (store.Queue{Dir: c.StateDir}).Load()
			if err != nil || len(jobs) != 1 || len(jobs[0].Deliveries) != 1 || jobs[0].Deliveries[0].DestinationID != "legacy-target" || jobs[0].Deliveries[0].Attempts != 0 {
				t.Fatalf("offline notification was not left queued: %+v %v", jobs, err)
			}
			if !strings.HasPrefix(output.String(), "Test notification queued.") {
				t.Fatalf("offline success no longer reports queuing: %q", output.String())
			}
			if _, err := os.Stat(filepath.Join(c.StateDir, "worker.json")); !os.IsNotExist(err) {
				t.Fatal("offline test started a worker")
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("offline test rewrote configuration")
			}
		})
	}
}

func TestBareCodexCommandsKeepDefaultIdentityAfterSourceRemoval(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	home := filepath.Join(dir, "default home")
	otherHome := filepath.Join(dir, "other home")
	c := config.Config{SchemaVersion: 1, CodexHome: home, Destinations: []config.Destination{}}
	if err := store.WriteJSON(path, c); err != nil {
		t.Fatal(err)
	}
	call := func(args ...string) error {
		return run(append([]string{"--config", path}, args...), strings.NewReader(""), &bytes.Buffer{})
	}
	for _, args := range [][]string{{"install", "codex"}, {"sources", "add", "--source", "other", "--harness", "codex", "--home", otherHome}, {"sources", "remove", "--source", config.DefaultSourceID}} {
		if err := call(args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if err := call("install", "codex"); err == nil || !strings.Contains(err.Error(), config.DefaultSourceID) {
		t.Fatalf("bare install was redirected to another Codex home: %v", err)
	}
	if err := call("uninstall", "codex"); err != nil {
		t.Fatalf("could not uninstall removed default source: %v", err)
	}
	if _, err := os.Stat(filepath.Join(otherHome, "hooks.json")); !os.IsNotExist(err) {
		t.Fatal("default command modified the other Codex home")
	}
	data, err := os.ReadFile(filepath.Join(home, "hooks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Hooks map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	for name, groups := range root.Hooks {
		if len(groups) != 0 {
			t.Fatalf("default %s hook remained after uninstall: %s", name, data)
		}
	}
}

func TestBareSourcesStillShowsHelpInScriptsWithoutConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	var output bytes.Buffer
	if err := run([]string{"sources", "--config", path}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Usage:") || !strings.Contains(output.String(), "sources") {
		t.Fatalf("bare sources no longer shows help: %q", output.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("sources help created configuration")
	}
}

func TestStopKeepsSuccessfulRequestExitWhenWorkerIsStillFinishing(t *testing.T) {
	if testing.Short() {
		t.Skip("exercises the CLI's ten-second graceful stop deadline")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	c := config.Config{SchemaVersion: 2, Sources: []config.Source{}, StateDir: filepath.Join(dir, "state"), Destinations: []config.Destination{{ID: "needs-repair", Kind: "webhook", Enabled: true, URL: "invalid-url"}}}
	if err := store.WriteJSON(path, c); err != nil {
		t.Fatal(err)
	}
	lock, acquired, err := (store.Queue{Dir: c.StateDir}).WorkerLock()
	if err != nil || !acquired {
		t.Fatalf("hold worker lock: %v", err)
	}
	defer lock.Close()
	var output bytes.Buffer
	started := time.Now()
	if err := run([]string{"stop", "--config", path}, strings.NewReader(""), &output); err != nil {
		t.Fatalf("stop request changed its exit contract: %v", err)
	}
	if time.Since(started) < 9*time.Second || !strings.Contains(output.String(), "Stop requested; the worker is finishing its current operation.") {
		t.Fatalf("stop did not wait then report the accepted request: %s", output.String())
	}
	if data, err := os.ReadFile(filepath.Join(c.StateDir, "stop")); err != nil || string(data) != "stop\n" {
		t.Fatalf("stop request was lost: %q %v", data, err)
	}
}
