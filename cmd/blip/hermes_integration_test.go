package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

func TestHermesNativeShellHooks(t *testing.T) {
	python, repo := os.Getenv("BLIP_HERMES_TEST_PYTHON"), os.Getenv("BLIP_HERMES_TEST_REPO")
	if python == "" || repo == "" || testing.Short() {
		t.Skip("set BLIP_HERMES_TEST_PYTHON and BLIP_HERMES_TEST_REPO to an installed Hermes runtime")
	}
	dir := filepath.Join(t.TempDir(), "中文 $ & ' 路径")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "blip.exe")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", out, err)
	}
	path, home := filepath.Join(dir, "config.json"), filepath.Join(dir, "hermes-profile")
	c := config.Config{SchemaVersion: 2, StateDir: filepath.Join(dir, "state"), Sources: []config.Source{{ID: "hermes-test", Harness: "hermes", Home: home, EventVersion: 2, Enabled: true}}, Destinations: []config.Destination{{ID: "test", Kind: "webhook", Enabled: true, URL: "http://127.0.0.1:1/unused"}}}
	if err := store.WriteJSON(path, c); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOCALAPPDATA", t.TempDir())
	if out, err := exec.Command(binary, "--config", path, "install", "hermes", "--source", "hermes-test", "--register-only").CombinedOutput(); err != nil {
		t.Fatalf("install: %s %v", out, err)
	}
	script, err := filepath.Abs("../../internal/agents/testdata/hermes/probe.py")
	if err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(dir, "captured.jsonl")
	command := exec.Command(python, script, filepath.Join(home, "config.yaml"), capture)
	command.Dir = dir
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		if strings.HasPrefix(key, "HERMES_") || key == "PYTHONPATH" {
			continue
		}
		command.Env = append(command.Env, env)
	}
	command.Env = append(command.Env, "HERMES_HOME="+home, "PYTHONPATH="+repo, "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1")
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Hermes native shell callback: %s %v", out, err)
	} else {
		t.Log(strings.TrimSpace(string(out)))
	}
	jobs, err := (store.Queue{Dir: c.StateDir}).Load()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[event.Kind]int{}
	for _, job := range jobs {
		counts[job.Event.Kind]++
		if job.Event.SessionID != "test-session" {
			t.Fatalf("child leaked: %+v", job.Event)
		}
		if job.Event.Kind == event.InputRequested && (len(job.Event.Questions) != 1 || len(job.Event.Questions[0].Options) != 2) {
			t.Fatalf("native clarify options missing: %+v", job.Event)
		}
	}
	if counts[event.TurnCompleted] != 2 || counts[event.InputRequested] != 1 || counts[event.ApprovalRequested] != 1 || counts[event.AttentionRequired] != 1 {
		data, _ := json.Marshal(jobs)
		t.Fatalf("unexpected native events: %v\n%s", counts, data)
	}
	if fixtureDir := os.Getenv("BLIP_HERMES_FIXTURE_DIR"); fixtureDir != "" {
		data, err := os.ReadFile(capture)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.WriteFile(filepath.Join(fixtureDir, "native-shell-hooks.jsonl"), data); err != nil {
			t.Fatal(err)
		}
	}
}
