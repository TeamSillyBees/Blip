package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/service"
	"github.com/TeamSillyBees/Blip/internal/store"
)

func TestAntigravityInstallRegistersOnceAndReusesExistingSources(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	profile := t.TempDir()
	t.Setenv("USERPROFILE", profile)
	t.Setenv("HOME", profile)
	t.Setenv("CODEX_HOME", filepath.Join(profile, ".codex"))
	path := filepath.Join(profile, "blip.json")
	if err := store.WriteJSON(path, config.Config{SchemaVersion: 1, Destinations: []config.Destination{}}); err != nil {
		t.Fatal(err)
	}
	call := func(args ...string) string {
		t.Helper()
		var out bytes.Buffer
		if err := run(append(append([]string{"--config", path}, args...), "--register-only"), strings.NewReader(""), &out); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out.String()
	}
	if out := call("install", "agy"); !strings.Contains(out, "通知渠道") {
		t.Fatalf("missing first-use guidance: %s", out)
	}
	call("install", "antigravity")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	call("install", "agy")
	call("install", "antigravity")
	again, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(original, again) {
		t.Fatal("repeated installs rewrote the configuration")
	}
	hooksPath := filepath.Join(profile, ".gemini", "config", "hooks.json")
	var groups map[string]json.RawMessage
	if err := store.ReadJSON(hooksPath, &groups); err != nil || len(groups) != 2 {
		t.Fatalf("expected one shared settings file and two source groups: %v %v", groups, err)
	}
	c, err := config.ReadLocal(path)
	if err != nil || len(c.Sources) != 3 || c.Sources[0].EventVersion != 1 {
		t.Fatalf("default configuration: %v %v", c.Sources, err)
	}
	for id, dir := range map[string]string{"agy": "antigravity-cli", "antigravity-app": "antigravity"} {
		s, ok := c.Source(id)
		if !ok || !s.Enabled || s.Harness != "antigravity" || s.EventVersion != 2 || s.Home != filepath.Join(profile, ".gemini", dir) {
			t.Fatalf("wrong source for %s: %+v", id, s)
		}
	}

	// A pre-existing registration can have a relative home, a custom ID and
	// a saved checkpoint. Enabling it must not allocate a new identity.
	c.Sources[1].ID = "cli-custom"
	c.Sources[1].Home = filepath.Join(".gemini", "antigravity-cli")
	c.Sources[1].Enabled = false
	if err := store.WriteJSON(path, c); err != nil {
		t.Fatal(err)
	}
	call("install", "agy")
	call("install", "agy", "--source", "cli-custom")
	c, err = config.ReadLocal(path)
	if err != nil || len(c.Sources) != 3 || c.Sources[1].ID != "cli-custom" || !c.Sources[1].Enabled {
		t.Fatalf("manual source not reused: %+v %v", c.Sources, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"missing", config.DefaultSourceID} {
		if err := run([]string{"--config", path, "install", "agy", "--source", id}, strings.NewReader(""), &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted incompatible source %s", id)
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed install changed configuration")
	}
}

func TestAntigravityInstallStartsAndReloadsWorker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows installs use SCM; see the Windows service integration tests")
	}
	testAntigravityInstallDelivery(t, false)
}

func TestAntigravityHookNeutralResponses(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "blip.json")
	s := config.Source{ID: "agy", Harness: "antigravity", Home: filepath.Join(root, "agy"), Enabled: true, EventVersion: 2}
	c := config.Config{SchemaVersion: 2, StateDir: filepath.Join(root, "data"), Sources: []config.Source{s}, Destinations: []config.Destination{{ID: "local", Kind: "webhook", Enabled: true, URL: "http://127.0.0.1:1/unused"}}}
	callback := func(name string, payload string, want string) {
		t.Helper()
		var out bytes.Buffer
		if err := run([]string{"--config", path, "hook", "--source", "agy", "--antigravity-event", name}, strings.NewReader(payload), &out); err != nil || strings.TrimSpace(out.String()) != want {
			t.Fatalf("%s returned %q, %v", name, out.String(), err)
		}
	}
	const allow = `{"decision":"allow"}`
	callback("PreToolUse", "invalid", allow) // even if the config was removed
	if err := store.WriteJSON(path, c); err != nil {
		t.Fatal(err)
	}
	callback("PreToolUse", "invalid", allow)
	callback("Stop", "invalid", "{}")
	payload, err := json.Marshal(map[string]any{"conversationId": "s", "artifactDirectoryPath": filepath.Join(s.Home, "brain", "s"), "stepIdx": 2,
		"toolCall": map[string]any{"id": "p", "name": "ask_permission", "args": map[string]string{"Action": "read_file", "Target": "example"}}})
	if err != nil {
		t.Fatal(err)
	}
	callback("PreToolUse", string(payload), allow)
	jobs, err := (store.Queue{Dir: c.StateDir}).Load()
	if err != nil || len(jobs) != 1 || jobs[0].Event.Kind != event.ApprovalRequested || jobs[0].Event.RequestID != "p" {
		t.Fatalf("explicit permission hook: %+v %v", jobs, err)
	}
}

func TestAntigravityInstallUpgradesWindowsService(t *testing.T) {
	if runtime.GOOS != "windows" || os.Getenv("BLIP_TEST_WINDOWS_SERVICE") != "1" {
		t.Skip("set BLIP_TEST_WINDOWS_SERVICE=1 in an elevated terminal for isolated SCM integration")
	}
	testAntigravityInstallDelivery(t, true)
}

func testAntigravityInstallDelivery(t *testing.T, serviceMode bool) {
	t.Helper()
	if testing.Short() {
		t.Skip("process integration")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "blip.exe")
	if out, err := exec.Command("go", "build", "-ldflags", "-X main.version=install-test", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", out, err)
	}
	previous := os.Getenv("BLIP_TEST_PREVIOUS_SERVICE_EXE")
	if serviceMode && previous == "" {
		previous = filepath.Join(dir, "previous.exe")
		if out, err := exec.Command("go", "build", "-o", previous, ".").CombinedOutput(); err != nil {
			t.Fatalf("build previous worker: %s %v", out, err)
		}
	}
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	t.Setenv("LOCALAPPDATA", t.TempDir())
	codexHome := filepath.Join(dir, "codex")
	t.Setenv("CODEX_HOME", codexHome)
	path := filepath.Join(dir, "blip.json")
	t.Cleanup(func() {
		args := []string{"--config", path, "stop"}
		if serviceMode {
			args = []string{"--config", path, "service", "uninstall"}
		}
		if out, err := exec.Command(binary, args...).CombinedOutput(); err != nil {
			t.Errorf("cleanup: %s %v", out, err)
		}
	})
	received := make(chan event.Event, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var e event.Event
		if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
			t.Error(err)
		}
		received <- e
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	// Start from a real v1 config; its existing channel and Codex home survive.
	if err := store.WriteJSON(path, config.Config{SchemaVersion: 1,
		Destinations: []config.Destination{{ID: "local", Kind: "webhook", Enabled: true, URL: server.URL}},
	}); err != nil {
		t.Fatal(err)
	}
	if serviceMode {
		for _, action := range []string{"install", "start"} {
			if out, err := exec.Command(previous, "--config", path, "service", action).CombinedOutput(); err != nil {
				t.Fatalf("prepare old service: %s %v", out, err)
			}
		}
		// A service's captured home takes precedence over the upgrading shell.
		t.Setenv("CODEX_HOME", filepath.Join(dir, "different-shell-home"))
	}
	for _, client := range []struct{ name, dir, id string }{
		{"agy", "antigravity-cli", "agy"}, {"antigravity", "antigravity", "antigravity-app"},
	} {
		home := filepath.Join(dir, ".gemini", client.dir)
		if out, err := exec.Command(binary, "install", client.name, "--config", path).CombinedOutput(); err != nil {
			t.Fatalf("install %s: %s %v", client.name, out, err)
		}
		var groups map[string]struct {
			PreToolUse []struct{ Hooks []struct{ Command string } }
		}
		if err := store.ReadJSON(filepath.Join(dir, ".gemini", "config", "hooks.json"), &groups); err != nil {
			t.Fatal(err)
		}
		payload, err := json.Marshal(map[string]any{
			"conversationId": "test", "artifactDirectoryPath": filepath.Join(home, "brain", "test"),
			"toolCall": map[string]any{"id": "q", "name": "ask_question", "args": map[string]any{"questions": []any{map[string]any{"question": "Continue?", "options": []string{"Yes", "No"}}}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		// Both clients invoke the groups in the shared file. Only the group for
		// the originating client should enqueue a job.
		for _, group := range groups {
			cmd := hookProcess(group.PreToolUse[0].Hooks[0].Command)
			cmd.Stdin = bytes.NewReader(payload)
			out, err := cmd.CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != `{"decision":"allow"}` {
				t.Fatalf("installed hook returned %q: %v", out, err)
			}
		}
		select {
		case e := <-received:
			if e.SourceID != client.id || e.Kind != event.InputRequested {
				t.Fatalf("wrong handoff after install %s: %+v", client.name, e)
			}
		case <-time.After(10 * time.Second):
			status, _ := exec.Command(binary, "--config", path, "status", "--json").CombinedOutput()
			log, _ := os.ReadFile(filepath.Join(dir, "data", "worker.log"))
			t.Fatalf("install %s did not start collecting: %s\n%s", client.name, status, log)
		}
	}
	if out, err := exec.Command(binary, "--config", path, "install", "agy").CombinedOutput(); err != nil {
		t.Fatalf("repeated install: %s %v", out, err)
	}
	c, err := config.ReadLocal(path)
	if err != nil || len(c.Sources) != 3 || c.Sources[0].Home != codexHome {
		t.Fatalf("previous configuration was lost: %+v %v", c.Sources, err)
	}
	if serviceMode {
		status, err := service.Query(path)
		if err != nil || status.State != "running" || !status.Installed {
			t.Fatalf("Windows service was not activated: %+v %v", status, err)
		}
		if out, err := exec.Command(status.Executable, "version").CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "install-test" {
			t.Fatalf("managed binary did not upgrade: %s %v", out, err)
		}
	}
}
