package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/service"
	"github.com/TeamSillyBees/Blip/internal/store"
)

func TestInstallDiscoversHarnessesAndRegistersCustomHomes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("USERPROFILE", dir)
	t.Setenv("HOME", dir)
	t.Setenv("CODEX_HOME", filepath.Join(dir, "custom codex"))
	t.Setenv("HERMES_HOME", filepath.Join(dir, "custom hermes"))
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "custom claude"))
	path := filepath.Join(dir, "blip.json")
	call := func(args ...string) error {
		t.Helper()
		return run(append([]string{"--config", path, "install", "--register-only"}, args...), strings.NewReader(""), &bytes.Buffer{})
	}
	if err := call("dsh"); err == nil || !strings.Contains(err.Error(), "cordis.yml") {
		t.Fatalf("missing DSH profile: %v", err)
	}
	dsh := filepath.Join(dir, ".dsh", "profiles", "desktop")
	if err := os.MkdirAll(dsh, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dsh, "cordis.yml"), []byte("[]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// An existing named Codex home must not prevent installing the default home.
	work := filepath.Join(dir, "work")
	if err := call("codex", "--source", "work", "--home", work); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ command, id, home, file string }{
		{"codex", config.DefaultSourceID, os.Getenv("CODEX_HOME"), "hooks.json"},
		{"claude-code", "claude-code", os.Getenv("CLAUDE_CONFIG_DIR"), "settings.json"},
		{"hermes", "hermes", os.Getenv("HERMES_HOME"), "config.yaml"},
		{"dsh", "dsh-desktop", dsh, "cordis.patch.yml"},
	} {
		for i := 0; i < 2; i++ {
			if err := call(test.command); err != nil {
				t.Fatalf("%s: %v", test.command, err)
			}
		}
		c, err := config.ReadLocal(path)
		if err != nil {
			t.Fatal(err)
		}
		s, ok := c.Source(test.id)
		if !ok || !s.Enabled || s.Home != test.home {
			t.Fatalf("incorrect %s source: %+v", test.command, s)
		}
		if _, err := os.Stat(filepath.Join(test.home, test.file)); err != nil {
			t.Fatalf("%s registration missing: %v", test.command, err)
		}
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := call("codex", "--source", "work", "--home", filepath.Join(dir, "other")); err == nil {
		t.Fatal("reassigned an existing source ID")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed install changed saved sources")
	}
}

func TestFirstInstallSelectsOnlyTheRequestedAntigravityClient(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("USERPROFILE", dir)
	t.Setenv("HOME", dir)
	for command, id := range map[string]string{"antigravity": "antigravity-app", "agy": "agy"} {
		path := filepath.Join(t.TempDir(), "blip.json")
		if err := run([]string{"install", command, "--config", path, "--register-only"}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
		c, err := config.ReadLocal(path)
		if err != nil || len(c.Sources) != 1 || c.Sources[0].ID != id || !c.Sources[0].Enabled {
			t.Fatalf("install %s enabled unexpected sources: %+v %v", command, c.Sources, err)
		}
	}
}

// Exercise the real SCM and an ordinary worker under a unique config path.
// Registration and cleanup require an elevated Windows integration run.
func TestInstallCreatesAndControlsWindowsService(t *testing.T) {
	if runtime.GOOS != "windows" || os.Getenv("BLIP_TEST_WINDOWS_SERVICE") != "1" || testing.Short() {
		t.Skip("set BLIP_TEST_WINDOWS_SERVICE=1 in an elevated Windows terminal")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "blip.exe")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %s %v", out, err)
	}
	path := filepath.Join(dir, "blip.json")
	c := config.Config{SchemaVersion: 1, CodexHome: filepath.Join(dir, "codex"), StateDir: filepath.Join(dir, "state"), Destinations: []config.Destination{}}
	if err := store.WriteJSON(path, c); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude"))
	call := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(binary, append([]string{"--config", path}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s %v", args, out, err)
		}
	}
	t.Cleanup(func() {
		if s, err := service.Query(path); err != nil {
			t.Error(err)
		} else if s.Installed {
			call("service", "uninstall")
		} else {
			call("stop")
		}
	})
	check := func(state string) {
		t.Helper()
		s, err := service.Query(path)
		if err != nil || !s.Installed || s.State != state || s.StartType != "automatic_delayed" {
			t.Fatalf("SCM state=%+v, wanted %s: %v", s, state, err)
		}
	}
	call("start") // pre-existing per-user worker, no service registered
	call("install", "claude-code")
	check("running")
	call("install", "codex")
	check("running")
	// A broken config must never prevent a real SCM stop.
	valid, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	call("stop")
	check("stopped")
	if err := os.WriteFile(path, valid, 0600); err != nil {
		t.Fatal(err)
	}
	call("start")
	check("running")
	call("restart")
	check("running")
	call("stop")
	check("stopped")
	if running, err := isRunning(c); err != nil || running {
		t.Fatalf("unmanaged worker left running: %v %v", running, err)
	}
}
