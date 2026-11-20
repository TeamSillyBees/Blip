package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TeamSillyBees/Blip/internal/agents"
	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/store"
)

func TestSourceCommandsMigrateDisableRemoveAndKeepBinding(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "配置 with spaces.json")
	if err := store.WriteJSON(path, config.Config{SchemaVersion: 1, CodexHome: filepath.Join(root, "default"), Destinations: []config.Destination{}}); err != nil {
		t.Fatal(err)
	}
	call := func(args ...string) string {
		t.Helper()
		var output bytes.Buffer
		if err := run(append([]string{"--config", path}, args...), strings.NewReader(""), &output); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return output.String()
	}
	call("sources", "add", "--source", "work", "--harness", "codex", "--home", filepath.Join(root, "中文 work"))
	c, err := config.ReadLocal(path)
	if err != nil || c.SchemaVersion != 2 || len(c.Sources) != 2 || c.Sources[0].EventVersion != 1 {
		t.Fatalf("migration failed: %+v %v", c, err)
	}
	call("install", "codex", "--source", "work", "--register-only")
	var doc map[string]any
	if err = json.Unmarshal([]byte(call("doctor", "--source", "work")), &doc); err != nil {
		t.Fatal(err)
	}
	call("sources", "disable", "--source", "work")
	c, err = config.ReadLocal(path)
	if err != nil || c.Sources[1].Enabled {
		t.Fatal("source was not disabled")
	}
	call("sources", "remove", "--source", "work")
	call("uninstall", "codex", "--source", "work")
	var output bytes.Buffer
	err = run([]string{"--config", path, "sources", "add", "--source", "work", "--harness", "codex", "--home", filepath.Join(root, "different")}, strings.NewReader(""), &output)
	if err == nil {
		t.Fatal("removed source ID was reused")
	}
	c, err = config.ReadLocal(path)
	if err != nil || len(c.Sources) != 1 {
		t.Fatal("failed binding check modified configuration")
	}
	if _, err = agents.InstalledSource(c, "work"); err != nil {
		t.Fatal("saved source was lost after uninstall")
	}
	if _, err = os.Stat(path + ".bak"); err != nil {
		t.Fatal("configuration backup missing")
	}
}

func TestHookErrorsReturnNeutralAndRecordOnlyErrorCode(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	c := config.Config{SchemaVersion: 1, CodexHome: filepath.Join(root, "codex"), StateDir: filepath.Join(root, "state"), Destinations: []config.Destination{}}
	if err := store.WriteJSON(path, c); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := run([]string{"--config", path, "hook"}, strings.NewReader(`{"private-key":"secret"`), &output); err != nil || output.Len() != 0 {
		t.Fatalf("hook altered process contract: %v %q", err, output.String())
	}
	data, err := os.ReadFile(agents.DiagnosticPath(c, config.DefaultSourceID))
	if err != nil || !bytes.Contains(data, []byte("hook_failed")) || bytes.Contains(data, []byte("secret")) {
		t.Fatalf("unsafe/missing hook diagnostic: %s %v", data, err)
	}
}

func TestAntigravitySourceNeedsHookInstallation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "blip.json")
	cfg := config.Config{SchemaVersion: 2, Sources: []config.Source{}, StateDir: filepath.Join(root, "state"), Destinations: []config.Destination{}}
	if err := store.WriteJSON(path, cfg); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "antigravity-cli")
	args := []string{"--config", path, "sources", "add", "--source", "agy", "--harness", "antigravity", "--home", home}
	var output bytes.Buffer
	if err := run(args, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := run([]string{"--config", path, "doctor", "--source", "agy"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	var result struct{ Sources []agents.SourceStatus }
	if err := json.Unmarshal(output.Bytes(), &result); err != nil || len(result.Sources) != 1 || result.Sources[0].Installation != "not_installed" || len(result.Sources[0].Capabilities) != 4 {
		t.Fatalf("wrong doctor result: %s", output.Bytes())
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("registering a source changed the agent home: %v", err)
	}
}
