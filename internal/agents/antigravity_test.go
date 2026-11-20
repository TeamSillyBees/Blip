package agents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

func TestAntigravityNativeHooks(t *testing.T) {
	root := t.TempDir()
	app := config.Source{ID: "app", Harness: "antigravity", Home: filepath.Join(root, "antigravity"), Enabled: true, EventVersion: 2}
	cli := config.Source{ID: "cli", Harness: "antigravity", Home: filepath.Join(root, "antigravity-cli"), Enabled: true, EventVersion: 2}
	c := config.Config{SchemaVersion: 2, StateDir: filepath.Join(root, "state"), Sources: []config.Source{app, cli}, Destinations: []config.Destination{{ID: "test", Enabled: true}}}
	ctx := context.Background()
	a := newAntigravity(c, cli)
	if _, polls := a.(Collector); polls {
		t.Fatal("hook-only source implements Collector")
	}
	if _, reads := a.(FileReader); reads {
		t.Fatal("service requested access to client files")
	}
	if err := RecordCheckpointFault(ctx, c, cli, checkpointFailed("antigravity-summaries", event.ID("antigravity-summaries"), "")); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("testdata/antigravity/native-hooks.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var h map[string]any
		if err := json.Unmarshal([]byte(line), &h); err != nil {
			t.Fatal(err)
		}
		name := h["hookEventName"].(string)
		h["artifactDirectoryPath"] = filepath.Join(cli.Home, "brain", "session")
		h["workspacePaths"] = []string{filepath.Join(root, "My Project")}
		for attempt := 0; attempt < 2; attempt++ {
			if attempt == 1 {
				delete(h, "hookEventName")
			} // registration supplies the name on older clients
			payload, err := json.Marshal(h)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range c.Sources {
				if err := CaptureAntigravity(ctx, c, s.ID, name, bytes.NewReader(payload)); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	jobs, err := (store.Queue{Dir: c.StateDir}).Load()
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[event.Kind]int{}
	for _, j := range jobs {
		e := j.Event
		if e.SourceID != cli.ID || e.Source != "antigravity/hooks" || e.SessionID != "session" || e.Project != "My Project" || e.TurnID != "" {
			t.Fatalf("incorrect source or session: %+v", e)
		}
		kinds[e.Kind]++
		if e.Kind == event.InputRequested && (e.RequestID != "call-question" || len(e.Questions) != 1 || e.Questions[0].Text != "Test input?" || !slices.Equal(e.Questions[0].Options, []string{"Yes", "No"})) {
			t.Fatalf("lost question: %+v", e)
		}
		if e.Kind == event.ApprovalRequested && (e.RequestID != "call-outside" || e.Detail != "申请在沙箱外运行命令。\nTesting Blip prompts") {
			t.Fatalf("wrong command approval: %+v", e)
		}
	}
	if len(jobs) != 4 || kinds[event.ApprovalRequested] != 1 || kinds[event.InputRequested] != 1 || kinds[event.TurnCompleted] != 2 {
		t.Fatalf("wrong events after replay: %v", kinds)
	}
	if _, err := os.Stat(DiagnosticPath(c, app.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("foreign callback changed app diagnostics: %v", err)
	}
	if _, err := os.Stat(cli.Home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hook needed or created client data: %v", err)
	}
}

func TestAntigravityCommandApprovalRequiresExplicitSandboxRequest(t *testing.T) {
	home := t.TempDir()
	a := newAntigravity(config.Config{StateDir: t.TempDir()}, config.Source{ID: "app", Home: home})
	for _, tc := range []struct {
		flag any
		want int
	}{{true, 1}, {"true", 1}, {false, 0}, {"false", 0}, {nil, 0}, {"yes", 0}, {1, 0}} {
		args := map[string]any{"CommandLine": "git commit -m example"}
		if tc.flag != nil {
			args["BypassSandbox"] = tc.flag
		}
		raw, err := json.Marshal(map[string]any{
			"hookEventName": "PreToolUse", "conversationId": "chat", "artifactDirectoryPath": filepath.Join(home, "brain", "chat"),
			"stepIdx": 15, "toolCall": map[string]any{"name": "run_command", "args": args},
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := a.ParseHook(context.Background(), raw, time.Now())
		if err != nil || len(got) != tc.want {
			t.Fatalf("BypassSandbox=%#v: %+v %v", tc.flag, got, err)
		}
		if tc.want == 1 && (got[0].Kind != event.ApprovalRequested || got[0].Detail != "申请在沙箱外运行命令。" || got[0].ID != event.SourceEventID("app", "antigravity", "approval_requested", "chat", "step:15")) {
			t.Fatalf("BypassSandbox=%#v: wrong approval from step: %+v", tc.flag, got[0])
		}
	}
}

func TestAntigravityHookSemantics(t *testing.T) {
	root := t.TempDir()
	a := newAntigravity(config.Config{StateDir: filepath.Join(root, "state")}, config.Source{ID: "app", Home: root})
	for _, tc := range []struct {
		name, reason, tool string
		idle               bool
		want               event.Kind
	}{
		{"Stop", "model_stop", "", true, event.TurnCompleted}, {"Stop", "ERROR", "", true, event.AttentionRequired},
		{"Stop", "max_steps_exceeded", "", true, event.AttentionRequired}, {"Stop", "NO_TOOL_CALL", "", false, ""},
		{"Stop", "USER_CANCELED", "", true, ""}, {"Stop", "CHAINED", "", true, ""},
		{"PreToolUse", "", "ask_permission", false, event.ApprovalRequested}, {"PreToolUse", "", "run_command", false, ""},
	} {
		h := map[string]any{"hookEventName": tc.name, "conversationId": "chat", "transcriptPath": filepath.Join(root, "brain", "chat", ".system_generated", "logs", "transcript.jsonl"),
			"terminationReason": tc.reason, "fullyIdle": tc.idle, "stepIdx": 5, "toolCall": map[string]any{"name": tc.tool}}
		raw, _ := json.Marshal(h)
		got, err := a.ParseHook(context.Background(), raw, time.Now())
		if err != nil || (tc.want == "" && len(got) != 0) || (tc.want != "" && (len(got) != 1 || got[0].Kind != tc.want || got[0].Validate() != nil || got[0].RequestID != "")) {
			t.Fatalf("%+v: %+v %v", tc, got, err)
		}
	}
}

func TestAntigravitySharedHookOwnership(t *testing.T) {
	profile := t.TempDir()
	t.Setenv("HOME", profile)
	t.Setenv("USERPROFILE", profile)
	source := config.Source{ID: "agy", Harness: "antigravity", Home: filepath.Join(profile, ".gemini", "antigravity-cli")}
	c := config.Config{Path: filepath.Join(profile, "first.json"), StateDir: t.TempDir()}
	first := newAntigravity(c, source).(*antigravityAdapter)
	c.Path = filepath.Join(profile, "second.json")
	second := newAntigravity(c, source).(*antigravityAdapter)
	path := filepath.Join(profile, ".gemini", "config", "hooks.json")
	user := []byte(`{"custom":{"Stop":[{"command":"my-hook","value":9007199254740993}]}}`)
	if err := store.WriteFile(path, user); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	exe := filepath.Join(profile, "中文 ' tools", "blip.exe")
	for _, a := range []*antigravityAdapter{first, second, first} {
		if err := a.Install(ctx, exe); err != nil {
			t.Fatal(err)
		}
	}
	doc, _, _, err := readHookSettings(path)
	if err != nil || len(doc) != 3 {
		t.Fatalf("settings: %v %v", doc, err)
	}
	group := doc[first.groupName()].(map[string]any)
	group["enabled"] = false
	if err := store.WriteJSON(path, doc); err != nil {
		t.Fatal(err)
	}
	if err := first.Install(ctx, exe); err != nil || first.InstallationStatus() != "disabled_by_harness" {
		t.Fatalf("lost user toggle: %v", err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	group["Stop"] = []any{map[string]any{"command": "user-edit"}}
	if err := store.WriteJSON(path, doc); err != nil {
		t.Fatal(err)
	}
	if err := first.Uninstall(ctx); err == nil {
		t.Fatal("removed a modified hook group")
	}
	if err := store.WriteFile(path, saved); err != nil {
		t.Fatal(err)
	}
	if err := first.Uninstall(ctx); err != nil || second.InstallationStatus() != "configured_unverified" {
		t.Fatalf("changed second registration: %v", err)
	}
	if err := second.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	doc, _, _, err = readHookSettings(path)
	got, marshalErr := json.Marshal(doc)
	if err != nil || marshalErr != nil || !bytes.Equal(got, user) {
		t.Fatalf("third-party definition changed: %s %v %v", got, err, marshalErr)
	}
}
