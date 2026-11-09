package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/TeamSillyBees/Blip/internal/config"
)

func TestInstallUninstallPreservesOtherHooksAndSettings(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "Codex 配置")
	_ = os.MkdirAll(home, 0700)
	original := `{"description":"user config","hooks":{"Stop":[{"hooks":[{"type":"command","command":"existing-stop","timeout":9}]}],"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"existing-input","statusMessage":"keep me"}]}]}}`
	path := filepath.Join(home, "hooks.json")
	_ = os.WriteFile(path, []byte(original), 0600)
	toml := []byte("# keep comments\n[features]\nhooks = true\n")
	_ = os.WriteFile(filepath.Join(home, "config.toml"), toml, 0600)
	c := config.Config{Path: filepath.Join(dir, "Blip 空格", "config.json"), CodexHome: home, StateDir: filepath.Join(dir, "data")}
	executable := filepath.Join(dir, "程序 空格", "blip.exe")
	if err := Install(context.Background(), c, executable); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)
	if err := Install(context.Background(), c, executable); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Fatal("reinstall duplicated or changed handlers")
	}
	if err := Uninstall(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	var expected, actual any
	_ = json.Unmarshal([]byte(original), &expected)
	cleaned, _ := os.ReadFile(path)
	_ = json.Unmarshal(cleaned, &actual)
	if !reflect.DeepEqual(expected, actual) {
		t.Fatalf("other configuration changed: %s", cleaned)
	}
	current, _ := os.ReadFile(filepath.Join(home, "config.toml"))
	if string(current) != string(toml) {
		t.Fatal("Codex config.toml was rewritten")
	}
	backups, _ := filepath.Glob(path + ".blip-backup-*")
	if len(backups) == 0 {
		t.Fatal("missing backup")
	}
}

func TestInstallDoesNotOverwriteDisabledOrMalformedConfiguration(t *testing.T) {
	for _, entry := range []struct{ toml, hooks string }{{"[features]\nhooks = false\n", `{}`}, {"", `broken`}} {
		dir := t.TempDir()
		home := filepath.Join(dir, "codex")
		_ = os.MkdirAll(home, 0700)
		_ = os.WriteFile(filepath.Join(home, "config.toml"), []byte(entry.toml), 0600)
		path := filepath.Join(home, "hooks.json")
		_ = os.WriteFile(path, []byte(entry.hooks), 0600)
		c := config.Config{Path: filepath.Join(dir, "config.json"), CodexHome: home, StateDir: filepath.Join(dir, "data")}
		if err := Install(context.Background(), c, filepath.Join(dir, "blip.exe")); err == nil {
			t.Fatal("unsupported configuration was overwritten")
		}
		data, _ := os.ReadFile(path)
		if string(data) != entry.hooks {
			t.Fatal("user hooks modified despite validation failure")
		}
		for _, path := range []string{filepath.Join(home, ".blip-hooks"), filepath.Join(c.StateDir, "installation.json")} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("validation failure created managed files: %s", path)
			}
		}
	}
}

func TestPathQuoting(t *testing.T) {
	_, windows, err := Commands(`C:\工具 空格\blip.exe`, `C:\配置 目录\config.json`)
	if err != nil || windows != `"C:\工具 空格\blip.exe" --config "C:\配置 目录\config.json" hook` {
		t.Fatalf("bad Windows command %q %v", windows, err)
	}
	if _, _, err = Commands(`C:\%TEMP%\blip.exe`, `C:\cfg.json`); err == nil {
		t.Fatal("cmd expansion was permitted in a literal path")
	}
}
