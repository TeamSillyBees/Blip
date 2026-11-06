package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
)

func TestServiceHelpDoesNotRequireConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	for _, operation := range []string{"", "install", "uninstall", "start", "stop", "status"} {
		args := []string{"--config", path, "service"}
		if operation != "" {
			args = append(args, operation)
		}
		var output bytes.Buffer
		if err := run(append(args, "--help"), strings.NewReader(""), &output); err != nil || output.Len() == 0 {
			t.Fatalf("service %s help: output=%q error=%v", operation, output.String(), err)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("service help created configuration")
	}
}

func TestServiceRejectsUnexpectedArgumentsBeforeMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	for _, args := range [][]string{{"service", "unknown"}, {"service", "install", "unexpected"}, {"service", "run"}} {
		if err := run(append([]string{"--config", path}, args...), strings.NewReader(""), &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted invalid command: %v", args)
		}
	}
}

func TestExplicitConfigurationWorksWithoutWindowsProfileEnvironment(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows configuration directory uses APPDATA")
	}
	t.Setenv("APPDATA", "")
	path := filepath.Join(t.TempDir(), "config.json")
	for _, args := range [][]string{{"--help"}, {"version"}, {"service", "--help"}, {"--config", path, "init"}} {
		if err := run(args, strings.NewReader(""), &bytes.Buffer{}); err != nil {
			t.Fatalf("explicit configuration or configuration-free command %v failed: %v", args, err)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("explicit configuration was not created: %v", err)
	}
	if err := run([]string{"status"}, strings.NewReader(""), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "specify --config") {
		t.Fatalf("missing default path was not diagnosed: %v", err)
	}
}

func TestServiceInstallRequiresEnabledDestination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"destinations":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	err := manageService(context.Background(), "install", path, &bytes.Buffer{})
	if runtime.GOOS == "windows" {
		if err == nil || !strings.Contains(err.Error(), "enable at least one destination") {
			t.Fatalf("unconfigured service installation was not refused: %v", err)
		}
	} else if err == nil || !strings.Contains(err.Error(), "only available on Windows") {
		t.Fatalf("unexpected non-Windows service error: %v", err)
	}
}

func TestServiceWorkerUsesCapturedPathsAndStopsOnCancellation(t *testing.T) {
	dir := t.TempDir()
	defaults := config.RuntimeDefaults{StateDir: filepath.Join(dir, "user state"), CodexHome: filepath.Join(dir, "user codex")}
	t.Setenv("CODEX_HOME", filepath.Join(dir, "service profile codex"))
	path := filepath.Join(dir, "config.json")
	data := `{"schema_version":1,"poll_interval":"100ms","destinations":[{"id":"test","kind":"webhook","enabled":true,"url":"http://127.0.0.1:1"}]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(defaults.CodexHome, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ready := false
	err := runServiceWorker(ctx, path, defaults, func() { ready = true; cancel() })
	if err != nil || !ready {
		t.Fatalf("service worker did not start and stop: ready=%v error=%v", ready, err)
	}
	for _, name := range []string{"worker.log", "service.log", "worker.json", "rollout-cursors.json"} {
		if _, err := os.Stat(filepath.Join(defaults.StateDir, name)); err != nil {
			t.Fatalf("missing user state file %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "service profile codex")); !os.IsNotExist(err) {
		t.Fatal("service unexpectedly used its own profile")
	}
}

func TestServiceWorkerRecordsConfigurationFailureBeforeReady(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data, _ := json.Marshal(map[string]any{"schema_version": 1, "private-secret-field": "private-secret-value", "destinations": []any{}})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	defaults := config.RuntimeDefaults{StateDir: filepath.Join(dir, "state"), CodexHome: filepath.Join(dir, "codex")}
	ready := false
	if err := runServiceWorker(context.Background(), path, defaults, func() { ready = true }); err == nil || ready {
		t.Fatalf("invalid configuration reported ready: %v", err)
	}
	logged, err := os.ReadFile(filepath.Join(defaults.StateDir, "service.log"))
	if err != nil || !bytes.Contains(logged, []byte("configuration")) || bytes.Contains(logged, []byte("private-secret")) {
		t.Fatalf("configuration diagnostic missing or leaked a value: %s %v", logged, err)
	}
}
