package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/TeamSillyBees/Blip/internal/config"
)

func TestCommandHelpAndVersionDoNotRequireConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.json")
	for _, args := range [][]string{{}, {"--help"}, {"configure", "--help"}, {"config", "--help"}, {"setup", "--help"}, {"restart", "--help"}, {"install", "codex", "--help"}, {"version"}, {"--version"}} {
		var output bytes.Buffer
		if err := run(append([]string{"--config", path}, args...), strings.NewReader(""), &output); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if output.Len() == 0 {
			t.Fatalf("%v produced no help/version output", args)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("help or version wrote a configuration")
	}
}

func TestMissingConfigurationSuggestsCopyableCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "中文 user's $config.json")
	for _, command := range []string{"start", "restart", "status", "test"} {
		err := run([]string{command, "--config", path}, strings.NewReader(""), &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), cliCommand(path, "configure")) {
			t.Fatalf("%s did not suggest configuration: %v", command, err)
		}
	}
	quote := "'\\''"
	if runtime.GOOS == "windows" {
		quote = "''"
	}
	want := "blip restart --config '" + strings.ReplaceAll(path, "'", quote) + "'"
	if got := cliCommand(path, "restart"); got != want {
		t.Fatalf("unsafe or incorrect command: %q, want %q", got, want)
	}
}

func TestStatusJSONFlagAndRedirectedOutputStayMachineReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":2,"sources":[],"destinations":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	var redirected, explicit bytes.Buffer
	for i, args := range [][]string{{"status"}, {"status", "--json"}} {
		output := []*bytes.Buffer{&redirected, &explicit}[i]
		if err := run(append(args, "--config", path), strings.NewReader(""), output); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(redirected.Bytes(), explicit.Bytes()) || !json.Valid(explicit.Bytes()) {
		t.Fatalf("status JSON contract changed: %q / %q", redirected.String(), explicit.String())
	}
	var report map[string]any
	if err := json.Unmarshal(explicit.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"running", "ready", "degraded", "sources", "pending", "delivered", "failed", "quarantined", "state_dir", "destinations", "collector_error", "latency"} {
		if _, exists := report[field]; !exists {
			t.Errorf("status removed the existing %q field", field)
		}
	}
	if report["running"] != false || report["ready"] != false || report["pending"] != float64(0) {
		t.Fatalf("incorrect status values: %s", explicit.String())
	}
}

func TestSourceInferenceHandlesUniqueAmbiguousAndLegacySources(t *testing.T) {
	c := config.Config{SchemaVersion: 2, Sources: []config.Source{{ID: "claude-work", Harness: "claude-code"}}}
	if id, err := inferSource(c, "claude-code"); err != nil || id != "claude-work" {
		t.Fatalf("unique source was not inferred: %q %v", id, err)
	}
	c.Sources = append(c.Sources, config.Source{ID: "claude-home", Harness: "claude-code"})
	if _, err := inferSource(c, "claude-code"); err == nil || !strings.Contains(err.Error(), "--source") || !strings.Contains(err.Error(), "claude-home") {
		t.Fatalf("ambiguous source was guessed: %v", err)
	}
	c.Sources = append(c.Sources, config.Source{ID: config.DefaultSourceID, Harness: "codex"}, config.Source{ID: "codex-work", Harness: "codex"})
	if id, err := inferSource(c, "codex"); err != nil || id != config.DefaultSourceID {
		t.Fatalf("legacy Codex shortcut changed: %q %v", id, err)
	}
}

func TestConfigFlagBeforeAndAfterCommand(t *testing.T) {
	for _, after := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "中文配置 with spaces.json")
		args := []string{"--config", path, "init"}
		if after {
			args = []string{"init", "--config", path}
		}
		var output bytes.Buffer
		if err := run(args, strings.NewReader(""), &output); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("config flag was ignored: %v", err)
		}
		document, err := config.OpenDocument(path)
		if err != nil {
			t.Fatal(err)
		}
		if document.Config.DeviceName != config.DefaultDeviceName() {
			t.Fatalf("init did not save the default device name: %q", document.Config.DeviceName)
		}
	}
}

func TestUnexpectedArgumentsFailBeforeMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	for _, args := range [][]string{{"unknown-command"}, {"init", "unexpected"}, {"install", "unknown-agent"}, {"install", "codex", "unexpected"}, {"configure", "unexpected"}} {
		var output bytes.Buffer
		if err := run(append([]string{"--config", path}, args...), strings.NewReader(""), &output); err == nil {
			t.Fatalf("%v unexpectedly succeeded", args)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("invalid command wrote a configuration")
	}
}

func TestConfigureRejectsNoninteractiveInputWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	var output bytes.Buffer
	if err := run([]string{"configure", "--config", path}, strings.NewReader(""), &output); err == nil {
		t.Fatal("configure accepted non-terminal input")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("non-terminal configure wrote a configuration")
	}
}
