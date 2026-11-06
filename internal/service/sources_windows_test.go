//go:build windows

package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/TeamSillyBees/Blip/internal/agents"
	"github.com/TeamSillyBees/Blip/internal/config"
	"golang.org/x/sys/windows"
)

func TestMultiSourceServiceRecordsOnlyRequiredReadPaths(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "blip", "config.json")
	exe := filepath.Join(root, "blip.exe")
	if err := os.MkdirAll(filepath.Dir(configPath), 0700); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{configPath, exe} {
		if err := os.WriteFile(file, []byte("test"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	sources := []config.Source{{ID: "one", Harness: "codex", Enabled: true, Home: filepath.Join(root, "one")}, {ID: "two", Harness: "codex", Enabled: true, Home: filepath.Join(root, "two")}, {ID: "off", Harness: "codex", Enabled: false, Home: filepath.Join(root, "off")}}
	m, _, err := makeInstallMetadata(InstallOptions{ConfigPath: configPath, Executable: exe, StateDir: filepath.Join(root, "state"), Sources: sources})
	if err != nil || m.Format != 2 || len(m.ReadPaths) != 4 || m.CodexHome != "" {
		t.Fatalf("bad source metadata: %+v %v", m, err)
	}
	for _, target := range permissionTargets(m) {
		for _, source := range sources {
			if samePath(target.path, source.Home) {
				if !source.Enabled || target.permissions != identifyDirectory || target.inheritance != 0 || target.permissions&windows.FILE_LIST_DIRECTORY != 0 {
					t.Fatal("harness home grant is not limited to enabled-source metadata")
				}
			}
		}
		if samePath(target.path, filepath.Join(sources[2].Home, "sessions")) {
			t.Fatal("disabled source was granted")
		}
	}
	// No lookup against current configuration/registry is needed for cleanup.
	m.Grants = []aclChange{{Path: m.ReadPaths[0].Path, SID: "test"}}
	if err = validateRecordedGrants(m); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{sources[0].Home, root, filepath.Join(root, "unrelated")} {
		copy := m
		copy.ReadPaths = []agents.ReadPath{{Path: invalid, Directory: true}}
		if validateReadPaths(copy) == nil {
			t.Fatalf("broad/outside source permission accepted: %s", invalid)
		}
	}
}

func TestEmptySourceServiceNeedsNoCodexHome(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.json")
	exe := filepath.Join(root, "blip.exe")
	for _, file := range []string{path, exe} {
		if err := os.WriteFile(file, []byte("test"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	m, _, err := makeInstallMetadata(InstallOptions{ConfigPath: path, Executable: exe, StateDir: filepath.Join(root, "data"), Sources: []config.Source{}})
	if err != nil || m.Format != 2 || len(m.ReadPaths) != 0 {
		t.Fatalf("empty sources restored Codex: %+v %v", m, err)
	}
	for _, target := range permissionTargets(m) {
		if filepath.Base(target.path) == "sessions" {
			t.Fatal("empty sources granted a sessions directory")
		}
	}
}
