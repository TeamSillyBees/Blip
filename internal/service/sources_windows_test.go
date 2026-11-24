//go:build windows

package service

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/TeamSillyBees/Blip/internal/agents"
	"github.com/TeamSillyBees/Blip/internal/config"
	"golang.org/x/sys/windows"
)

func TestActivateAntigravitySourcePreservesLegacyServiceLedger(t *testing.T) {
	root := t.TempDir()
	codeHome := filepath.Join(root, "codex")
	installed := installMetadata{
		Format: 1, Name: "Blip-test", ConfigPath: filepath.Join(root, "config.json"),
		Executable: filepath.Join(root, "service", "blip.exe"), StateDir: filepath.Join(root, "state"), CodexHome: codeHome,
		Grants: []aclChange{{Path: filepath.Join(codeHome, "sessions"), SID: "legacy"}, {Path: filepath.Join(codeHome, "session_index.jsonl"), SID: "legacy"}},
	}
	args := serviceArguments(installed)
	s := config.Source{ID: "agy", Harness: "antigravity", Enabled: true, Home: filepath.Join(root, "agy"), EventVersion: 2}
	plan := installMetadata{Format: 2, Sources: []config.Source{s}, ReadPaths: sourceReadPaths(config.Config{}, s)}
	for i := 0; i < 2; i++ {
		if err := recordServiceSources(&installed, plan); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(args, serviceArguments(installed)) || installed.Format != 2 || len(installed.Sources) != 2 ||
			installed.Sources[0].ID != config.DefaultSourceID || installed.Sources[1] != s || len(installed.ReadPaths) != 5 || len(installed.Grants) != 2 {
			t.Fatalf("upgrade lost grants, changed SCM identity or duplicated a source: %+v", installed)
		}
	}
	plan.Sources[0].Home = filepath.Join(root, "other")
	if err := recordServiceSources(&installed, plan); err == nil {
		t.Fatal("service allowed an existing ID to change homes")
	}
}

func TestActivateSourceJournalsAndRepairsReadGrant(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session_index.jsonl")
	sid, err := windows.StringToSid("S-1-5-80-304021-304022-304023-304024-304025")
	if err != nil {
		t.Fatal(err)
	}
	target := permissionTarget{path: path, permissions: windows.FILE_GENERIC_READ}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("cannot persist service metadata")
	if _, err := applyPermission(target, sid, false, func(aclChange) error { return failure }); !errors.Is(err, failure) || allowedSIDEntries(t, path, sid.String(), true) != 0 {
		t.Fatalf("ACL changed without a durable record: %v", err)
	}
	var grants []aclChange
	record := func(g aclChange) error {
		grants = append(grants, g)
		return nil
	}
	if _, err := applyPermission(target, sid, false, record); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if i == 1 {
			// A client replacing its index also replaces the earlier explicit ACE.
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := applyPermission(target, sid, true, record); err != nil {
			t.Fatal(err)
		}
		if len(grants) != 1 || allowedSIDEntries(t, path, sid.String(), true) != 1 {
			t.Fatalf("missing or duplicate read access: %+v", grants)
		}
	}
}

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
