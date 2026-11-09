package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/TeamSillyBees/Blip/internal/config"
	"github.com/TeamSillyBees/Blip/internal/store"
)

type launcherCapture struct {
	Args    []string
	Cwd     string
	Payload []byte
}

func TestMain(m *testing.M) {
	// Let a copy of the native test binary stand in for blip, without invoking
	// another shell or a language runtime to parse its arguments and stdin.
	if path := os.Getenv("BLIP_CODEX_TEST_CAPTURE"); path != "" {
		cwd, _ := os.Getwd()
		payload, _ := io.ReadAll(os.Stdin)
		data, _ := json.Marshal(launcherCapture{Args: os.Args, Cwd: cwd, Payload: payload})
		if err := os.WriteFile(path, data, 0600); err != nil {
			os.Exit(3)
		}
		fmt.Println("notification output must not reach the agent")
		fmt.Fprintln(os.Stderr, "notification failures must not block the agent")
		os.Exit(2)
	}
	os.Exit(m.Run())
}

func launcherConfig(t *testing.T) config.Config {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "中文 空格 & (test) 'quoted'")
	return config.Config{Path: filepath.Join(dir, "config.json"), CodexHome: filepath.Join(dir, "codex"), StateDir: filepath.Join(dir, "state")}
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func loadInstallation(t *testing.T, c config.Config) installation {
	t.Helper()
	var m installation
	if err := store.ReadJSON(filepath.Join(c.StateDir, "installation.json"), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestLauncherForwardsNativeArgumentsStdinAndUpgrade(t *testing.T) {
	c := launcherConfig(t)
	// These metacharacters used to be rejected even in target paths. The new
	// entry handles them as batch/POSIX literals, with no PATH lookup or eval.
	c.Path = filepath.Join(filepath.Dir(c.Path), "%PATH% ! caret^", "配置.json")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary := readBytes(t, exe)
	capturePath := filepath.Join(t.TempDir(), "capture.json")
	workdir := t.TempDir()
	expectedCwd, err := os.Stat(workdir)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"questions":["选择？ & %PATH% !"],"padding":"` + strings.Repeat("abc", 50000) + `"}`)
	var firstCommands []string
	var oldBinary string
	for _, version := range []string{"node-v22", "node-v24"} {
		path := filepath.Join(filepath.Dir(c.Path), version, "程序 %PATH% ! caret^", "blip.exe")
		if err := store.WriteFileMode(path, binary, 0700); err != nil {
			t.Fatal(err)
		}
		if err := InstallSourceVersion(context.Background(), c, path, "work", 2); err != nil {
			t.Fatal(err)
		}
		m := loadInstallation(t, c)
		if firstCommands == nil {
			firstCommands = m.Commands
		} else {
			if !reflect.DeepEqual(firstCommands, m.Commands) {
				t.Fatal("registered command changed on upgrade")
			}
			if err := os.Remove(oldBinary); err != nil {
				t.Fatal(err)
			}
		}
		command := launcherProcess(m.Commands[0])
		command.Env = append(os.Environ(), "BLIP_CODEX_TEST_CAPTURE="+capturePath)
		command.Dir = workdir
		command.Stdin = bytes.NewReader(payload)
		if out, err := command.CombinedOutput(); err != nil || len(out) != 0 {
			t.Fatalf("launcher must be silent and fail open: %s %v", out, err)
		}
		var got launcherCapture
		if err := store.ReadJSON(capturePath, &got); err != nil {
			t.Fatalf("launcher did not execute native target: %v", err)
		}
		want := []string{path, "--config", c.Path, "hook", "--source", "work"}
		actualCwd, err := os.Stat(got.Cwd)
		if err != nil {
			t.Fatal(err)
		}
		// macOS resolves /var to /private/var in the child. Check directory
		// identity so equivalent paths still verify that cwd was preserved.
		if !reflect.DeepEqual(got.Args, want) || !bytes.Equal(got.Payload, payload) || !os.SameFile(expectedCwd, actualCwd) {
			t.Fatalf("arguments/cwd/stdin changed: args=%q cwd=%q payload matches=%v", got.Args, got.Cwd, bytes.Equal(got.Payload, payload))
		}
		if err := os.Remove(capturePath); err != nil {
			t.Fatal(err)
		}
		oldBinary = path
	}
	// An uninstalled package or an incomplete external cleanup must not block
	// a Codex operation. Doctor separately reports missing managed files.
	if err := os.Remove(oldBinary); err != nil {
		t.Fatal(err)
	}
	for _, removeTarget := range []bool{false, true} {
		if removeTarget {
			m := loadInstallation(t, c)
			for _, file := range m.Launcher.Files {
				if strings.HasPrefix(file.Name, "run.") {
					if err := os.Remove(filepath.Join(m.Launcher.Dir, file.Name)); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
		if out, err := launcherProcess(firstCommands[0]).CombinedOutput(); err != nil || len(out) != 0 {
			t.Fatalf("missing target must fail open: %s %v", out, err)
		}
	}
}

func TestStableHooksMigrateLegacyAndPreserveIdentity(t *testing.T) {
	for _, tc := range []struct {
		id      string
		version int
	}{{"", 1}, {"work", 1}, {"", 2}, {"work", 2}} {
		t.Run(fmt.Sprintf("%s-v%d", tc.id, tc.version), func(t *testing.T) {
			c := launcherConfig(t)
			ctx := context.Background()
			old := filepath.Join(t.TempDir(), "old", "blip.exe")
			posix, windows, err := Commands(old, c.Path)
			if err != nil {
				t.Fatal(err)
			}
			if tc.id != "" {
				posix += " --source " + tc.id
				windows += " --source " + tc.id
			}
			// Seed the pre-launcher manifest/registration format.
			other := map[string]any{"hooks": map[string]any{"PreToolUse": []any{map[string]any{"matcher": "Bash", "hooks": []any{map[string]any{"type": "command", "command": "orca-hook", "timeout": 10}}}}}}
			path := filepath.Join(c.CodexHome, "hooks.json")
			if err := store.WriteJSON(path, other); err != nil {
				t.Fatal(err)
			}
			if err := editHooksVersion(ctx, c, []string{posix, windows}, true, tc.version, nil); err != nil {
				t.Fatal(err)
			}
			legacy := readBytes(t, path)
			if err := InstallSourceVersion(ctx, c, old, tc.id, tc.version); err != nil {
				t.Fatal(err)
			}
			first := readBytes(t, path)
			m := loadInstallation(t, c)
			entry := "hook.sh"
			if runtime.GOOS == "windows" {
				entry = "hook.cmd"
			}
			entryBefore := readBytes(t, filepath.Join(m.Launcher.Dir, entry))
			backupsBefore, _ := filepath.Glob(path + ".blip-backup-*")
			newBinary := filepath.Join(t.TempDir(), "new", "blip.exe")
			if err := InstallSourceVersion(ctx, c, newBinary, tc.id, tc.version); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first, readBytes(t, path)) || !bytes.Equal(entryBefore, readBytes(t, filepath.Join(m.Launcher.Dir, entry))) {
				t.Fatal("upgrade changed the hook definition or stable entry")
			}
			backupsAfter, _ := filepath.Glob(path + ".blip-backup-*")
			if len(backupsAfter) != len(backupsBefore) {
				t.Fatal("unchanged hooks were backed up on reinstall")
			}
			if bytes.Contains(first, []byte("blip.exe")) || bytes.Contains(first, []byte("--config")) || bytes.Contains(first, []byte("--source")) {
				t.Fatal("registered hooks expose mutable target arguments")
			}
			foundLegacyBackup := false
			for _, backup := range backupsAfter {
				foundLegacyBackup = foundLegacyBackup || bytes.Equal(legacy, readBytes(t, backup))
			}
			if !foundLegacyBackup || InstallationStatus(c, tc.version) != "configured_unverified" {
				t.Fatal("migration lost its backup or cannot be diagnosed")
			}
			if err := Uninstall(ctx, c); err != nil {
				t.Fatal(err)
			}
			var remaining any
			if err := json.Unmarshal(readBytes(t, path), &remaining); err != nil || !jsonEqualValue(other, remaining) {
				t.Fatalf("uninstall damaged another hook: %s", readBytes(t, path))
			}
			if _, err := os.Stat(m.Launcher.Dir); !os.IsNotExist(err) {
				t.Fatal("uninstall left managed scripts")
			}
		})
	}
}

func jsonEqualValue(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}

func TestLauncherIsolationAndUninstallWithMissingHooks(t *testing.T) {
	first := launcherConfig(t)
	second := first
	second.StateDir = filepath.Join(t.TempDir(), "other-state")
	second.Path = filepath.Join(t.TempDir(), "other-config.json")
	ctx := context.Background()
	for _, c := range []config.Config{first, second} {
		if err := InstallSourceVersion(ctx, c, filepath.Join(t.TempDir(), "blip.exe"), "work", 2); err != nil {
			t.Fatal(err)
		}
	}
	a, b := loadInstallation(t, first), loadInstallation(t, second)
	if a.Launcher.Dir == b.Launcher.Dir || a.Commands[0] == b.Commands[0] {
		t.Fatal("independent configurations share a mutable launcher")
	}
	hooksPath := filepath.Join(first.CodexHome, "hooks.json")
	// Preserve user formatting and the first registration's position even
	// after another configuration has appended its own hooks.
	var document any
	if err := json.Unmarshal(readBytes(t, hooksPath), &document); err != nil {
		t.Fatal(err)
	}
	formatted, _ := json.MarshalIndent(document, "", "\t")
	if err := store.WriteFile(hooksPath, formatted); err != nil {
		t.Fatal(err)
	}
	if err := InstallSourceVersion(ctx, first, filepath.Join(t.TempDir(), "upgraded.exe"), "work", 2); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(formatted, readBytes(t, hooksPath)) {
		t.Fatal("upgrading one registration reordered hooks or changed user formatting")
	}
	if err := Uninstall(ctx, first); err != nil {
		t.Fatal(err)
	}
	if InstallationStatus(second, 2) != "configured_unverified" {
		t.Fatal("uninstall removed another configuration's hooks")
	}
	if err := os.Remove(filepath.Join(second.CodexHome, "hooks.json")); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(b.Launcher.Dir, "user.txt")
	if err := store.WriteFile(unrelated, []byte("keep me")); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(ctx, second); err != nil {
		t.Fatal(err)
	}
	for _, file := range b.Launcher.Files {
		if _, err := os.Stat(filepath.Join(b.Launcher.Dir, file.Name)); !os.IsNotExist(err) {
			t.Fatal("missing hooks.json prevented launcher cleanup")
		}
	}
	if string(readBytes(t, unrelated)) != "keep me" {
		t.Fatal("uninstall removed an unrelated file")
	}
}

func TestLauncherDetectsEditsAndRepairsMissingFiles(t *testing.T) {
	c := launcherConfig(t)
	ctx := context.Background()
	exe := filepath.Join(t.TempDir(), "blip.exe")
	if err := Install(ctx, c, exe); err != nil {
		t.Fatal(err)
	}
	m := loadInstallation(t, c)
	path := filepath.Join(m.Launcher.Dir, m.Launcher.Files[0].Name)
	original := readBytes(t, path)
	hooksBefore := readBytes(t, filepath.Join(c.CodexHome, "hooks.json"))
	if err := store.WriteFile(path, []byte("user edit")); err != nil {
		t.Fatal(err)
	}
	if InstallationStatus(c, 1) != "invalid_configuration" {
		t.Fatal("edited launcher reported healthy")
	}
	for _, change := range []func() error{func() error { return Install(ctx, c, exe) }, func() error { return Uninstall(ctx, c) }} {
		if err := change(); err == nil || !strings.Contains(err.Error(), "edited") {
			t.Fatalf("edited launcher was overwritten: %v", err)
		}
		if !bytes.Equal(hooksBefore, readBytes(t, filepath.Join(c.CodexHome, "hooks.json"))) || string(readBytes(t, path)) != "user edit" {
			t.Fatal("conflict changed existing hooks or launcher")
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if InstallationStatus(c, 1) != "missing_configuration" {
		t.Fatal("missing launcher reported healthy")
	}
	if err := Install(ctx, c, exe); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, readBytes(t, path)) || InstallationStatus(c, 1) != "configured_unverified" {
		t.Fatal("reinstall did not repair missing launcher")
	}
}

func TestLauncherRecoversInterruptedUpgrade(t *testing.T) {
	for _, newTarget := range []bool{false, true} {
		for _, uninstall := range []bool{false, true} {
			t.Run(fmt.Sprintf("new=%t-uninstall=%t", newTarget, uninstall), func(t *testing.T) {
				c := launcherConfig(t)
				ctx := context.Background()
				if err := Install(ctx, c, filepath.Join(t.TempDir(), "old.exe")); err != nil {
					t.Fatal(err)
				}
				m := loadInstallation(t, c)
				next, err := newHookLauncher(c, filepath.Join(t.TempDir(), "new.exe"), "", runtime.GOOS == "windows")
				if err != nil {
					t.Fatal(err)
				}
				m.Launcher = mergeLauncher(m.Launcher, &next)
				if err := store.WriteJSON(filepath.Join(c.StateDir, "installation.json"), m); err != nil {
					t.Fatal(err)
				}
				if newTarget {
					if err := next.write(); err != nil {
						t.Fatal(err)
					}
				}
				if uninstall {
					err = Uninstall(ctx, c)
				} else {
					err = Install(ctx, c, filepath.Join(t.TempDir(), "retry.exe"))
				}
				if err != nil {
					t.Fatalf("journal could not recover interrupted replacement: %v", err)
				}
			})
		}
	}
}
