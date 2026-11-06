package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

func TestDSHNativeRuntime(t *testing.T) {
	runtimePath := os.Getenv("BLIP_DSH_TEST_RUNTIME")
	if runtimePath == "" || testing.Short() {
		t.Skip("set BLIP_DSH_TEST_RUNTIME to an extracted installed DSH runtime (containing node_modules)")
	}
	dir := filepath.Join(t.TempDir(), "中文 $ & ' 路径")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "blip.exe")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", out, err)
	}
	profile := filepath.Join(dir, "profile")
	if err := store.WriteFile(filepath.Join(profile, "cordis.yml"), []byte("[]\n")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "blip.json")
	c := config.Config{SchemaVersion: 2, StateDir: filepath.Join(dir, "state"), Sources: []config.Source{{ID: "dsh-test", Harness: "dsh", Home: profile, Enabled: true, EventVersion: 2}}, Destinations: []config.Destination{{ID: "test", Kind: "webhook", Enabled: true, URL: "http://127.0.0.1:1/unused"}}}
	if err := store.WriteJSON(path, c); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(binary, "--config", path, "install", "dsh", "--source", "dsh-test").CombinedOutput(); err != nil {
		t.Fatalf("install: %s %v", out, err)
	}
	script, _ := filepath.Abs("../../internal/agents/testdata/dsh/probe.mjs")
	capture := filepath.Join(dir, "captured.jsonl")
	command := exec.Command("node", script, runtimePath, profile, filepath.Join(c.StateDir, "sources", "dsh-test", "installation.json"), capture)
	command.Dir = dir
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("native runtime: %s %v", out, err)
	} else {
		t.Log(string(out))
	}
	jobs, err := (store.Queue{Dir: c.StateDir}).Load()
	if err != nil {
		t.Fatal(err)
	}
	counts := map[event.Kind]int{}
	for _, job := range jobs {
		counts[job.Event.Kind]++
		if job.Event.SessionID == "child-session" {
			t.Fatal("child notification")
		}
	}
	if counts[event.TurnCompleted] != 4 || counts[event.AttentionRequired] != 2 || counts[event.InputRequested] != 2 || counts[event.ApprovalRequested] != 1 {
		t.Fatal(counts)
	}
	if fixture := os.Getenv("BLIP_DSH_FIXTURE_DIR"); fixture != "" {
		data, err := os.ReadFile(capture)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.WriteFile(filepath.Join(fixture, "native-runtime-hooks.jsonl"), data); err != nil {
			t.Fatal(err)
		}
	}
}
