package agents

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/codex"
	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/event"
	"github.com/TeamSillyBees/Blip/internal/store"
)

const inputHook = `{"hook_event_name":"PreToolUse","session_id":"same-session","turn_id":"same-turn","tool_name":"request_user_input","tool_use_id":"same-request","tool_input":{"questions":[{"question":"选择？","options":["继续","结束"]}]}}`

func sourceConfig(t *testing.T) config.Config {
	t.Helper()
	root := t.TempDir()
	return config.Config{SchemaVersion: 2, Path: filepath.Join(root, "config.json"), StateDir: filepath.Join(root, "state"), DeviceName: "test-device", Sources: []config.Source{{ID: "one", Harness: "codex", EventVersion: 2, Enabled: true, Home: filepath.Join(root, "one")}, {ID: "two", Harness: "codex", EventVersion: 2, Enabled: true, Home: filepath.Join(root, "two")}}, Destinations: []config.Destination{{ID: "test", Kind: "webhook", Enabled: true}}}
}

func TestSourceIDsSeparateIdenticalNativeHooks(t *testing.T) {
	c := sourceConfig(t)
	ctx := context.Background()
	for _, id := range []string{"one", "two", "one"} {
		if err := Capture(ctx, c, id, strings.NewReader(inputHook)); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := (store.Queue{Dir: c.StateDir}).Load()
	if err != nil || len(jobs) != 2 {
		t.Fatalf("cross-source deduplication: %d %v", len(jobs), err)
	}
	if jobs[0].Event.ID == jobs[1].Event.ID || jobs[0].Event.SourceID == jobs[1].Event.SourceID {
		t.Fatal("sources collided")
	}
	for _, job := range jobs {
		if job.Event.SchemaVersion != 2 || job.Event.DeviceName != "test-device" || len(job.Event.Questions) != 1 {
			t.Fatalf("lost event data: %+v", job.Event)
		}
	}
	// Disabled and removed callbacks remain neutral and never enqueue.
	c.Sources[0].Enabled = false
	if err = Capture(ctx, c, "one", strings.NewReader(strings.ReplaceAll(inputHook, "same-request", "new-request"))); err != nil {
		t.Fatal(err)
	}
	c.Sources = []config.Source{}
	if err = Capture(ctx, c, "two", strings.NewReader(inputHook)); err != nil {
		t.Fatal(err)
	}
	jobs, err = (store.Queue{Dir: c.StateDir}).Load()
	if err != nil || len(jobs) != 2 {
		t.Fatal("disabled/removed source changed queue")
	}
}

func TestDefaultSourceRetainsLegacyIDsAcrossProtocolUpgrade(t *testing.T) {
	c := sourceConfig(t)
	now := time.Now()
	legacy, err := codex.ParseHook(strings.NewReader(inputHook), now)
	if err != nil {
		t.Fatal(err)
	}
	source := config.Source{ID: config.DefaultSourceID, Harness: "codex", Home: t.TempDir(), EventVersion: 2, Enabled: true}
	a, err := Builtins().Open(c, source)
	if err != nil {
		t.Fatal(err)
	}
	events, err := a.ParseHook(context.Background(), []byte(inputHook), now)
	if err != nil || len(events) != 1 || events[0].ID != legacy.ID || events[0].SourceID != source.ID {
		t.Fatalf("default ID changed: %+v %v", events, err)
	}
	if StateDir(c, source) != c.StateDir {
		t.Fatal("legacy checkpoint moved")
	}
	q := store.Queue{Dir: c.StateDir}
	if _, err = q.Enqueue(context.Background(), *legacy, []string{"test"}); err != nil {
		t.Fatal(err)
	}
	if added, err := q.Enqueue(context.Background(), events[0], []string{"test"}); err != nil || added {
		t.Fatalf("upgrade requeued old identity: %v %v", added, err)
	}
	jobs, err := q.Load()
	if err != nil || jobs[0].Event.SchemaVersion != 1 {
		t.Fatal("upgrade rewrote original snapshot")
	}
}

func TestBindingSurvivesRemovalAndRejectsReuse(t *testing.T) {
	c := sourceConfig(t)
	original := c.Sources[0]
	if err := Bind(context.Background(), c, original); err != nil {
		t.Fatal(err)
	}
	c.Sources = []config.Source{}
	saved, err := InstalledSource(c, original.ID)
	if err != nil || saved.Home != original.Home {
		t.Fatalf("removed installation lost: %+v %v", saved, err)
	}
	changed := original
	changed.Home = t.TempDir()
	if err = Bind(context.Background(), c, changed); !errors.Is(err, ErrBinding) {
		t.Fatalf("reused ID: %v", err)
	}
	if err = CheckBinding(c, changed); !errors.Is(err, ErrBinding) {
		t.Fatal("read-only title lookup crossed binding")
	}
	if err = os.WriteFile(bindingPath(c, original.ID), []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = Bind(context.Background(), c, original); !errors.Is(err, ErrBinding) {
		t.Fatal("corrupt binding reset")
	}
}

func TestLegacyInstallationCannotBeSilentlyRebound(t *testing.T) {
	c := sourceConfig(t)
	if err := store.WriteJSON(filepath.Join(c.StateDir, "installation.json"), map[string]any{"codex_home": t.TempDir(), "commands": []string{"owned"}}); err != nil {
		t.Fatal(err)
	}
	s := config.Source{ID: config.DefaultSourceID, Harness: "codex", Home: t.TempDir(), EventVersion: 1}
	if err := Bind(context.Background(), c, s); !errors.Is(err, ErrBinding) {
		t.Fatalf("legacy manifest ignored: %v", err)
	}
}

func TestReadHookBoundsInputAndEOF(t *testing.T) {
	if _, err := ReadHook(context.Background(), strings.NewReader(strings.Repeat("x", MaxHookBytes+1))); err == nil {
		t.Fatal("oversized payload accepted")
	}
	reader, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := ReadHook(ctx, reader); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("missing EOF deadline: %v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("stdin wait exceeded budget")
	}
	if _, err := writer.Write([]byte("x")); err == nil {
		t.Fatal("cancelled input pipe left open")
	}
}

func TestTitlesDoNotCrossSourcesOrDeletedBindings(t *testing.T) {
	c := sourceConfig(t)
	for _, s := range c.Sources {
		if err := store.WriteFile(filepath.Join(s.Home, "session_index.jsonl"), []byte(`{"id":"same","thread_name":"`+s.ID+`"}`+"\n")); err != nil {
			t.Fatal(err)
		}
	}
	e := event.Event{SchemaVersion: 2, Agent: "codex", SourceID: "two", SessionID: "same"}
	if title := ResolveTitle(context.Background(), c, e); title != "two" {
		t.Fatalf("wrong source title: %q", title)
	}
	c.Sources = c.Sources[:1]
	if title := ResolveTitle(context.Background(), c, e); title != "" {
		t.Fatalf("removed source inherited another title: %q", title)
	}
}

func TestInstallationCanBeRemovedAfterSourceDeletion(t *testing.T) {
	c := sourceConfig(t)
	source := c.Sources[0]
	if err := Bind(context.Background(), c, source); err != nil {
		t.Fatal(err)
	}
	a, err := Builtins().Open(c, source)
	if err != nil {
		t.Fatal(err)
	}
	installer := a.(Installer)
	executable := filepath.Join(t.TempDir(), "blip.exe")
	if err := os.WriteFile(executable, []byte("native target"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = installer.Install(context.Background(), executable); err != nil {
		t.Fatal(err)
	}
	if installer.InstallationStatus() != "configured_unverified" {
		t.Fatal("installed commands not found")
	}
	c.Sources = []config.Source{}
	saved, err := InstalledSource(c, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, err = Builtins().Open(c, saved)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.(Installer).Uninstall(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.(Installer).InstallationStatus() != "not_installed" {
		t.Fatal("installation manifest remains")
	}
	if _, err = os.Stat(bindingPath(c, source.ID)); err != nil {
		t.Fatal("uninstall released permanent source identity")
	}
}
