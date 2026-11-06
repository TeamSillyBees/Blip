package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRuntimeDefaultsPreserveInstallingUserPathsAcrossProfiles(t *testing.T) {
	path, original := writeDocument(t, `{"schema_version":1,"destinations":[]}`)
	serviceCodexHome := filepath.Join(t.TempDir(), "service-profile", ".codex")
	t.Setenv("CODEX_HOME", serviceCodexHome)
	installingProfile := t.TempDir()
	defaults := RuntimeDefaults{
		StateDir:  filepath.Join(installingProfile, "Blip", "data"),
		CodexHome: filepath.Join(installingProfile, ".codex"),
	}
	c, err := ReadWithDefaults(path, defaults)
	if err != nil {
		t.Fatal(err)
	}
	if c.StateDir != defaults.StateDir || c.CodexHome != defaults.CodexHome {
		t.Fatal("service configuration did not retain the installing user's paths")
	}
	for _, read := range []func(string) (Config, error){Read, ReadLocal} {
		local, err := read(path)
		if err != nil {
			t.Fatal(err)
		}
		if local.StateDir != filepath.Join(filepath.Dir(path), "data") || local.CodexHome != serviceCodexHome {
			t.Fatal("ordinary configuration reads changed their existing path defaults")
		}
	}
	assertBytes(t, path, original)
}

func TestRuntimeDefaultsYieldToExplicitConfigPaths(t *testing.T) {
	profile := t.TempDir()
	defaults := RuntimeDefaults{
		StateDir:  filepath.Join(profile, "captured-state"),
		CodexHome: filepath.Join(profile, "captured-codex"),
	}
	t.Setenv("CODEX_HOME", filepath.Join(profile, "service-codex"))
	for _, entry := range []struct {
		name      string
		stateDir  string
		codexHome string
	}{
		{"relative paths", "custom-state", filepath.Join("..", "custom-codex")},
		{"absolute paths", filepath.Join(profile, "explicit-state"), filepath.Join(profile, "explicit-codex")},
		{"state override only", "custom-state", ""},
		{"codex override only", "", "custom-codex"},
	} {
		t.Run(entry.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			data, err := json.Marshal(Config{SchemaVersion: 1, StateDir: entry.stateDir, CodexHome: entry.codexHome, Destinations: []Destination{}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			c, err := ReadWithDefaults(path, defaults)
			if err != nil {
				t.Fatal(err)
			}
			for _, target := range []struct{ got, explicit, fallback string }{
				{c.StateDir, entry.stateDir, defaults.StateDir},
				{c.CodexHome, entry.codexHome, defaults.CodexHome},
			} {
				want := target.explicit
				if want == "" {
					want = target.fallback
				} else if !filepath.IsAbs(want) {
					want = filepath.Join(filepath.Dir(path), want)
				}
				if target.got != filepath.Clean(want) {
					t.Fatal("runtime default overrode an explicit path or changed its resolution base")
				}
			}
		})
	}
}

func TestRuntimeDefaultsRejectRelativeCallerPaths(t *testing.T) {
	path, _ := writeDocument(t, `{"schema_version":1,"state_dir":"explicit-state","codex_home":"explicit-codex","destinations":[]}`)
	for _, defaults := range []RuntimeDefaults{
		{StateDir: "relative-state"},
		{CodexHome: "relative-codex"},
		{StateDir: "."},
		{CodexHome: filepath.Join("..", "profile")},
	} {
		if _, err := ReadWithDefaults(path, defaults); err == nil || !strings.Contains(err.Error(), "absolute path") {
			t.Fatalf("relative runtime default was accepted: %v", err)
		}
	}
}

func TestEmptyRuntimeDefaultsMatchOrdinaryRead(t *testing.T) {
	path, _ := writeDocument(t, `{"schema_version":1,"destinations":[]}`)
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "current-codex"))
	want, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadWithDefaults(path, RuntimeDefaults{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("empty runtime defaults changed configuration behavior")
	}
}

func TestRuntimeDefaultsKeepFullChannelValidationWithoutInterpolation(t *testing.T) {
	t.Setenv("BLIP_SERVICE_TEST_TOKEN", "private-token")
	defaults := RuntimeDefaults{StateDir: t.TempDir(), CodexHome: t.TempDir()}
	for _, token := range []string{"", "${BLIP_SERVICE_TEST_TOKEN}", "literal$BLIP_SERVICE_TEST_TOKEN"} {
		t.Run(token, func(t *testing.T) {
			data, err := json.Marshal(Config{
				SchemaVersion: 1,
				Destinations: []Destination{{
					ID: "phone", Kind: "gotify", Enabled: true, URL: "https://example.invalid", Token: token,
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			path, original := writeDocument(t, string(data))
			c, err := ReadWithDefaults(path, defaults)
			if token == "literal$BLIP_SERVICE_TEST_TOKEN" {
				if err != nil || c.Destinations[0].Token != token {
					t.Fatalf("literal token was changed or rejected: %v", err)
				}
			} else if err == nil || strings.Contains(err.Error(), "private-token") {
				t.Fatalf("invalid channel accepted or an environment value leaked: %v", err)
			}
			assertBytes(t, path, original)
		})
	}
}
