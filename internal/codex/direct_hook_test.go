package codex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/platform"
	"github.com/TeamSillyBees/Blip/internal/store"
)

// A native recorder exercises the same command interpreter Codex uses,
// including the Windows wrapping of a quoted executable followed by arguments.
func TestMain(m *testing.M) {
	if capture := os.Getenv("BLIP_CODEX_TEST_CAPTURE"); capture != "" {
		input, _ := io.ReadAll(os.Stdin)
		cwd, _ := os.Getwd()
		if err := store.WriteJSON(capture, capturedHook{os.Args[1:], string(input), cwd}); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type capturedHook struct {
	Args       []string
	Input, Cwd string
}

func TestDirectCommandForwardsArgumentsAndStdin(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "中文 & (tools) 'Blip'")
	exe := filepath.Join(dir, "blip.exe")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WriteFileMode(exe, data, 0700); err != nil {
		t.Fatal(err)
	}
	custom := filepath.Join(dir, "用户 config.json")
	line, err := platform.HookCommand(exe, custom, "work")
	if err != nil {
		t.Fatal(err)
	}
	capture := filepath.Join(t.TempDir(), "capture.json")
	cmd := launcherProcess(line)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "BLIP_CODEX_TEST_CAPTURE="+capture)
	const payload = `{"text":"中文 & %PATH% !"}`
	cmd.Stdin = strings.NewReader(payload)
	if output, err := cmd.CombinedOutput(); err != nil || len(output) != 0 {
		t.Fatalf("native callback: %s %v", output, err)
	}
	var got capturedHook
	if err := store.ReadJSON(capture, &got); err != nil {
		t.Fatal(err)
	}
	wantDir, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	gotDir, err := os.Stat(got.Cwd)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Args, []string{"hook", "--source", "work", "--config", custom}) || got.Input != payload || !os.SameFile(wantDir, gotDir) {
		t.Fatalf("native callback received %+v", got)
	}
}

func TestDirectRegistrationRebuildsOldBlipHooksWithoutScriptsOrManifest(t *testing.T) {
	dir := t.TempDir()
	c := config.Config{Path: filepath.Join(dir, "config.json"), StateDir: filepath.Join(dir, "state"), CodexHome: filepath.Join(dir, "codex")}
	exe := filepath.Join(dir, "Programs", "Blip", "blip.exe")
	if err := store.WriteFile(exe, []byte("fixed binary")); err != nil {
		t.Fatal(err)
	}
	state := c.StateDir
	if runtime.GOOS == "windows" {
		state = strings.ToLower(state)
	}
	digest := sha256.Sum256([]byte(state))
	old := filepath.Join(c.CodexHome, ".blip-hooks", fmt.Sprintf("%x", digest[:16]))
	legacy := `"` + filepath.ToSlash(filepath.Join(old, "hook.cmd")) + `"`
	oldDirect := `"C:/old npm/bin/blip.exe" --config "` + c.Path + `" hook`
	other := map[string]any{"type": "command", "command": "another-tool", "statusMessage": "Other"}
	var oldHandlers []any
	for _, line := range []string{legacy, legacy, oldDirect} {
		oldHandlers = append(oldHandlers, map[string]any{"type": "command", "command": line, "statusMessage": hookMarker})
	}
	path := filepath.Join(c.CodexHome, "hooks.json")
	doc := map[string]any{"description": "keep", "hooks": map[string]any{"PreToolUse": []any{map[string]any{"matcher": "Bash", "hooks": append(oldHandlers, other)}}}}
	if err := store.WriteJSON(path, doc); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := InstallSourceVersion(ctx, c, exe, config.DefaultSourceID, 2); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("hook.cmd")) || bytes.Contains(raw, []byte("old npm")) || bytes.Count(raw, []byte(hookMarker)) != 6 || !bytes.Contains(raw, []byte("another-tool")) {
		t.Fatalf("incorrect rebuilt hooks: %s", raw)
	}
	if s := InstallationStatus(c, config.DefaultSourceID, 2); s != "configured_unverified" {
		t.Fatal(s)
	}
	if _, err := os.Stat(filepath.Join(c.CodexHome, ".blip-hooks")); !os.IsNotExist(err) {
		t.Fatal("created script directory")
	}
	manifest := filepath.Join(c.StateDir, "installation.json")
	stamp := time.Unix(1_600_000_000, 0)
	for _, file := range []string{path, manifest} {
		if err := os.Chtimes(file, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	if err := InstallSourceVersion(ctx, c, exe, config.DefaultSourceID, 2); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{path, manifest} {
		if info, err := os.Stat(file); err != nil || !info.ModTime().Equal(stamp) {
			t.Fatalf("redundant write to %s", file)
		}
	}
	// A second configuration can share a Codex home and the same executable.
	second := c
	second.Path = filepath.Join(dir, "other.json")
	second.StateDir = filepath.Join(dir, "other state")
	if err := InstallSourceVersion(ctx, second, exe, "work", 2); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(manifest); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(ctx, c, exe, config.DefaultSourceID); err != nil {
		t.Fatal(err)
	}
	if s := InstallationStatus(second, "work", 2); s != "configured_unverified" {
		t.Fatal(s)
	}
	if err := Uninstall(ctx, second, exe, "work"); err != nil {
		t.Fatal(err)
	}
	var cleaned map[string]any
	if err := store.ReadJSON(path, &cleaned); err != nil {
		t.Fatal(err)
	}
	doc["hooks"] = map[string]any{"PreToolUse": []any{map[string]any{"matcher": "Bash", "hooks": []any{other}}}}
	want, _ := json.Marshal(doc)
	got, _ := json.Marshal(cleaned)
	if !bytes.Equal(got, want) {
		t.Fatalf("uninstall changed foreign hooks: %s", got)
	}
	if data, err := os.ReadFile(exe); err != nil || string(data) != "fixed binary" {
		t.Fatal("uninstall touched the fixed executable")
	}
}
