package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLegacySourceMigrationPreservesResolvedHomeAndVersion(t *testing.T) {
	path, original := writeDocument(t, `{"schema_version":1,"codex_home":"agent-home","destinations":[]}`)
	c, err := ReadLocal(path)
	if err != nil {
		t.Fatal(err)
	}
	s := c.EffectiveSources()
	if len(s) != 1 || s[0].ID != DefaultSourceID || s[0].EventVersion != 1 || s[0].Home != filepath.Join(filepath.Dir(path), "agent-home") {
		t.Fatalf("bad legacy mapping: %+v", s)
	}
	assertBytes(t, path, original)
	doc, err := OpenDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = doc.MigrateSources(); err != nil {
		t.Fatal(err)
	}
	if err = doc.Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertBytes(t, path+".bak", original)
	c, err = ReadLocal(path)
	if err != nil || c.SchemaVersion != 2 || c.CodexHome != "" || c.Sources[0] != s[0] {
		t.Fatalf("migration lost identity: %+v %v", c.Sources, err)
	}
}

func TestV2SourcesEmptyAndStrictIdentity(t *testing.T) {
	for _, data := range []string{
		`{"schema_version":2,"destinations":[]}`,
		`{"schema_version":2,"sources":null,"destinations":[]}`,
		`{"schema_version":2,"codex_home":"old","sources":[],"destinations":[]}`,
		`{"schema_version":1,"sources":[],"destinations":[]}`,
	} {
		path, _ := writeDocument(t, data)
		if _, err := ReadLocal(path); err == nil {
			t.Fatalf("accepted invalid sources: %s", data)
		}
	}
	path, _ := writeDocument(t, `{"schema_version":2,"sources":[],"destinations":[]}`)
	c, err := ReadLocal(path)
	if err != nil || len(c.EffectiveSources()) != 0 || c.CodexHome != "" {
		t.Fatalf("empty sources became Codex: %+v %v", c, err)
	}
	data, _ := json.Marshal(c)
	var restored Config
	if err = json.Unmarshal(data, &restored); err != nil || restored.Sources == nil {
		t.Fatalf("empty sources lost on save: %s", data)
	}
	for _, id := range []string{"../x", ".", "..", "a/b", "a\\b", "CON", "com1", "Codex-Default"} {
		c.Sources = []Source{{ID: id, Harness: "codex", Home: t.TempDir(), EventVersion: 2}}
		if err = validateStructure(c); err == nil {
			t.Fatalf("accepted unsafe ID %q", id)
		}
	}
}

func TestV2DefaultsAndDuplicateHomes(t *testing.T) {
	path, _ := writeDocument(t, `{"schema_version":2,"sources":[{"id":"work","harness":"codex","home":"work","enabled":true}],"destinations":[]}`)
	c, err := ReadLocal(path)
	if err != nil || c.Sources[0].EventVersion != 2 || !filepath.IsAbs(c.Sources[0].Home) {
		t.Fatalf("source defaults: %+v %v", c.Sources, err)
	}
	clone := c.Clone()
	clone.Sources[0].Home = "changed"
	if c.Sources[0].Home == "changed" {
		t.Fatal("clone shares sources")
	}
	duplicate := c.Sources[0]
	duplicate.ID = "other"
	c.Sources = append(c.Sources, duplicate)
	if err = validateStructure(c); err == nil {
		t.Fatal("duplicate home accepted")
	}
	c.Sources[1].Harness = "unavailable-harness"
	if err = validateStructure(c); err != nil {
		t.Fatalf("unknown adapter must not block queue delivery: %v", err)
	}
}

func TestDuplicateSymlinkHomes(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	link := filepath.Join(dir, "alias")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlink permission unavailable")
	}
	c := Config{SchemaVersion: 2, Sources: []Source{{ID: "a", Harness: "codex", Home: filepath.Join(real, "missing")}, {ID: "b", Harness: "codex", Home: filepath.Join(link, "missing")}}}
	if err := validateStructure(c); err == nil {
		t.Fatal("symlink alias accepted")
	}
}
