package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/agents"
	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

// An opt-in contract test against the official CLI. The only model endpoint is
// a loopback stub, settings are isolated, and no channel receives a real alert.
func TestClaudeNativeHooks(t *testing.T) {
	claude := os.Getenv("BLIP_CLAUDE_TEST_BINARY")
	if claude == "" || testing.Short() {
		t.Skip("set BLIP_CLAUDE_TEST_BINARY to an official Claude Code executable")
	}
	dir := filepath.Join(t.TempDir(), "中文 $ & ' 路径")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "blip.exe")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", out, err)
	}
	home := filepath.Join(dir, "claude-home")
	path := filepath.Join(dir, "config.json")
	c := config.Config{SchemaVersion: 2, StateDir: filepath.Join(dir, "state"), Sources: []config.Source{{ID: "claude-test", Harness: "claude-code", EventVersion: 2, Enabled: true, Home: home}}, Destinations: []config.Destination{{ID: "test", Kind: "webhook", Enabled: true, URL: "http://127.0.0.1:1/unused"}}}
	if err := store.WriteJSON(path, c); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOCALAPPDATA", t.TempDir())
	install := exec.Command(binary, "--config", path, "install", "claude-code", "--source", "claude-test", "--register-only")
	if out, err := install.CombinedOutput(); err != nil {
		t.Fatalf("install: %s %v", out, err)
	}
	settingsPath := filepath.Join(home, "settings.json")
	var settings map[string]any
	if err := store.ReadJSON(settingsPath, &settings); err != nil {
		t.Fatal(err)
	}
	testBinary, _ := os.Executable()
	capture := filepath.Join(dir, "captured.jsonl")
	hooks := settings["hooks"].(map[string]any)
	for name, value := range hooks {
		hooks[name] = append(value.([]any), map[string]any{"hooks": []any{map[string]any{"type": "command", "command": testBinary, "args": []string{"-test.run=^TestClaudeHookRecorder$"}, "timeout": 5}}})
	}
	if err := store.WriteJSON(settingsPath, settings); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	requests := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.Path)
		mu.Unlock()
		if strings.Contains(r.URL.Path, "count_tokens") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"input_tokens":10}`)
			return
		}
		if r.URL.Path != "/v1/messages" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, item := range []struct{ name, data string }{
			{"message_start", `{"type":"message_start","message":{"id":"msg_local_test","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0}}}`},
			{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
			{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Local test completed."}}`},
			{"content_block_stop", `{"type":"content_block_stop","index":0}`},
			{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`},
			{"message_stop", `{"type":"message_stop"}`},
		} {
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", item.name, item.data)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, claude, "-p", "Local notification integration test", "--model", "claude-sonnet-4-6", "--tools", "", "--settings", settingsPath, "--setting-sources", "", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--system-prompt", "Return the local test response.", "--output-format", "json")
	command.Dir = dir
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		if strings.HasPrefix(key, "ANTHROPIC_") || strings.HasPrefix(key, "CLAUDE_") || key == "HOME" || key == "USERPROFILE" || key == "BLIP_CLAUDE_HOOK_CAPTURE" {
			continue
		}
		command.Env = append(command.Env, env)
	}
	command.Env = append(command.Env, "ANTHROPIC_BASE_URL="+server.URL, "ANTHROPIC_API_KEY=blip-local-test-only", "CLAUDE_CONFIG_DIR="+home, "HOME="+dir, "USERPROFILE="+dir, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1", "DISABLE_TELEMETRY=1", "DISABLE_ERROR_REPORTING=1", "BLIP_CLAUDE_HOOK_CAPTURE="+capture)
	output, err := command.CombinedOutput()
	mu.Lock()
	paths := append([]string{}, requests...)
	mu.Unlock()
	if err != nil {
		t.Fatalf("native CLI: %v; paths=%v; output=%s", err, paths, output)
	}
	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("native hook did not run: %v; paths=%v; output=%s", err, paths, output)
	}
	seen := map[string]bool{}
	var sanitized bytes.Buffer
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte{'\n'}) {
		var h map[string]any
		if err := json.Unmarshal(line, &h); err != nil {
			t.Fatal(err)
		}
		name, _ := h["hook_event_name"].(string)
		seen[name] = true
		session, _ := h["session_id"].(string)
		prompt, _ := h["prompt_id"].(string)
		if session == "" || prompt == "" {
			t.Fatalf("native identity missing: %s", line)
		}
		t.Logf("native %s: prompt_id=%t, agent_id=%v", name, h["prompt_id"] != nil, h["agent_id"])
		h["session_id"], h["prompt_id"] = "test-session", "test-prompt"
		h["cwd"], h["transcript_path"] = "C:/blip-test/中文 $ & ' 路径", "C:/blip-test/claude-home/projects/test/test-session.jsonl"
		_ = json.NewEncoder(&sanitized).Encode(h)
	}
	if !seen["UserPromptSubmit"] || !seen["Stop"] {
		t.Fatalf("missing native callbacks: %v", seen)
	}
	// The candidate is durable after the real CLI exits. A fresh Blip worker
	// turns it into a neutral reminder; no sending process is started here.
	if fixtureDir := os.Getenv("BLIP_CLAUDE_FIXTURE_DIR"); fixtureDir != "" {
		if err := store.WriteFile(filepath.Join(fixtureDir, "native-hooks.jsonl"), sanitized.Bytes()); err != nil {
			t.Fatal(err)
		}
	}
	resolved, err := config.ReadLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := agents.Builtins().Open(resolved, resolved.Sources[0])
	if err != nil {
		t.Fatal(err)
	}
	collector := adapter.(agents.Collector)
	deadline := time.Now().Add(3 * time.Second)
	notified := false
	for time.Now().Before(deadline) {
		if err := collector.Poll(ctx, func(e event.Event) error {
			_, err := (store.Queue{Dir: resolved.StateDir}).Enqueue(ctx, e, []string{"test"})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		jobs, err := (store.Queue{Dir: resolved.StateDir}).Load()
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) > 0 {
			if len(jobs) != 1 || jobs[0].Event.Reason != "stop_signal" {
				t.Fatalf("bad native stop: %+v", jobs)
			}
			notified = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !notified {
		t.Fatal("real hook failed to persist a usable stop candidate")
	}
	t.Logf("official CLI completed with durable Stop candidate; endpoints=%v", paths)
}

func TestClaudeHookRecorder(t *testing.T) {
	path := os.Getenv("BLIP_CLAUDE_HOOK_CAPTURE")
	if path == "" {
		t.Skip("native hook helper")
	}
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 1024*1024))
	if err != nil {
		os.Exit(1)
	}
	var h map[string]any
	if json.Unmarshal(data, &h) != nil {
		os.Exit(1)
	}
	data, _ = json.Marshal(h)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		os.Exit(1)
	}
	_, err = f.Write(append(data, '\n'))
	_ = f.Close()
	if err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestClaudeExecFormCLI(t *testing.T) {
	// This always runs, even when the optional official CLI is unavailable.
	if testing.Short() {
		t.Skip("process integration")
	}
	dir := filepath.Join(t.TempDir(), "中文 $ & ' 路径")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "blip.exe")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", out, err)
	}
	path := filepath.Join(dir, "config.json")
	home := filepath.Join(dir, "claude")
	c := config.Config{SchemaVersion: 2, StateDir: filepath.Join(dir, "state"), Sources: []config.Source{{ID: "claude", Harness: "claude-code", EventVersion: 2, Enabled: true, Home: home}}, Destinations: []config.Destination{{ID: "test", Kind: "webhook", Enabled: true, URL: "http://127.0.0.1:1/unused"}}}
	if err := store.WriteJSON(path, c); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOCALAPPDATA", t.TempDir())
	if out, err := exec.Command(binary, "--config", path, "install", "claude-code", "--source", "claude", "--register-only").CombinedOutput(); err != nil {
		t.Fatalf("install: %s %v", out, err)
	}
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string
				Args    []string
			}
		}
	}
	if err := store.ReadJSON(filepath.Join(home, "settings.json"), &settings); err != nil {
		t.Fatal(err)
	}
	h := settings.Hooks["PreToolUse"][0].Hooks[0]
	command := exec.Command(h.Command, h.Args...)
	command.Stdin = strings.NewReader(`{"hook_event_name":"PreToolUse","session_id":"s","prompt_id":"p","tool_name":"AskUserQuestion","tool_use_id":"q","tool_input":{"questions":[{"question":"选择？","options":[{"label":"继续"}]}]}}`)
	if out, err := command.CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatalf("hook response must be neutral: %s %v", out, err)
	}
	jobs, err := (store.Queue{Dir: c.StateDir}).Load()
	if err != nil || len(jobs) != 1 || jobs[0].Event.Kind != event.InputRequested || len(jobs[0].Event.Questions) != 1 {
		t.Fatalf("exec form did not enqueue: %+v %v", jobs, err)
	}
}
