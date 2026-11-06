package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func TestDocumentMissingFileHasNoSideEffectsUntilSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new", "config.json")
	doc, err := OpenDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Config.SchemaVersion != 1 || doc.Config.Destinations == nil || len(doc.Config.Destinations) != 0 {
		t.Fatal("missing configuration should start with an empty schema 1 document")
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("opening a missing document created its directory")
	}
	if err := doc.Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	fields := readFields(t, path)
	if len(fields) != 2 || string(fields["schema_version"]) != "1" || string(fields["destinations"]) != "[]" {
		t.Fatalf("runtime defaults should remain omitted: %v", fields)
	}
	assertPrivate(t, path)
	assertMissing(t, path+".bak")
}

func TestDocumentCancelAndUnchangedSavePreserveOriginalBytes(t *testing.T) {
	path, original := writeDocument(t, `{"schema_version":1, "destinations":[]}`)
	doc, err := OpenDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	staged := doc.Config.Clone()
	staged.Destinations = []Destination{{ID: "cancelled", Kind: "webhook"}}
	assertBytes(t, path, original)
	assertMissing(t, path+".bak")
	assertMissing(t, path+".lock")
	if err := doc.Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertBytes(t, path, original)
	assertMissing(t, path+".bak")
}

func TestDocumentPreservesReferencesPathsAndOmittedDefaults(t *testing.T) {
	t.Setenv("BLIP_EDITOR_TOKEN", "do-not-serialize-this-secret")
	t.Setenv("BLIP_EDITOR_URL", "https://example.invalid/")
	path, original := writeDocument(t, `{
  "schema_version":1,
  "state_dir":"relative/data",
  "codex_home":"../codex",
  "max_attempts":0,
  "destinations":[
    {"id":"existing","kind":"gotify","enabled":true,"url":"${BLIP_EDITOR_URL}","token":"${BLIP_EDITOR_TOKEN}"},
    {"id":"sample","kind":"webhook","enabled":false}
  ]
}`)
	doc, err := OpenDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Config.StateDir != "relative/data" || doc.Config.CodexHome != "../codex" || doc.Config.PollInterval != "" || doc.Config.MaxAttempts != 0 {
		t.Fatal("editing resolved paths or applied defaults")
	}
	doc.Config.Destinations[0].Priority = 4
	doc.Config.Destinations = append(doc.Config.Destinations, Destination{ID: "new", Kind: "webhook", Enabled: true, URL: "${BLIP_EDITOR_URL}", Headers: map[string]string{"X-Token": "${BLIP_EDITOR_TOKEN}"}})
	if err := doc.Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertBytes(t, path+".bak", original)
	assertPrivate(t, path)
	assertPrivate(t, path+".bak")
	fields := readFields(t, path)
	if _, ok := fields["poll_interval"]; ok {
		t.Fatal("save materialized omitted poll_interval")
	}
	if string(fields["max_attempts"]) != "0" || string(fields["state_dir"]) != `"relative/data"` || string(fields["codex_home"]) != `"../codex"` {
		t.Fatal("save changed unrelated raw settings")
	}
	var destinations []map[string]json.RawMessage
	if err := json.Unmarshal(fields["destinations"], &destinations); err != nil {
		t.Fatal(err)
	}
	if _, ok := destinations[1]["url"]; ok {
		t.Fatal("untouched disabled destination gained omitted fields")
	}
	data, _ := os.ReadFile(path)
	if bytes.Contains(data, []byte("do-not-serialize-this-secret")) || !bytes.Contains(data, []byte("${BLIP_EDITOR_TOKEN}")) {
		t.Fatal("save expanded a credential reference")
	}
	if err := doc.Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertBytes(t, path+".bak", original)
	assertBytes(t, path, data)
	if _, err := Read(path); err != nil {
		t.Fatalf("saved configuration is unreadable: %v", err)
	}
}

func TestDocumentPreservesHeaderEditsWithoutAliasingSnapshot(t *testing.T) {
	path, original := writeDocument(t, `{"schema_version":1,"destinations":[{"id":"web","kind":"webhook","enabled":true,"url":"https://example.invalid","headers":{"X-Key":"${OLD_KEY}"}}]}`)
	doc, err := OpenDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	clone := doc.Config.Clone()
	clone.Destinations[0].Headers["X-Key"] = "${NEW_KEY}"
	if doc.Config.Destinations[0].Headers["X-Key"] != "${OLD_KEY}" {
		t.Fatal("staged clone mutated the document")
	}
	doc.Config = clone
	if err := doc.Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertBytes(t, path+".bak", original)
	reopened, err := OpenDocument(path)
	if err != nil || reopened.Config.Destinations[0].Headers["X-Key"] != "${NEW_KEY}" {
		t.Fatalf("header edit did not persist: %v", err)
	}
}

func TestDocumentRejectsConcurrentChanges(t *testing.T) {
	path, original := writeDocument(t, `{"schema_version":1,"destinations":[]}`)
	first, _ := OpenDocument(path)
	second, _ := OpenDocument(path)
	first.Config.PollInterval = "2s"
	second.Config.PollInterval = "3s"
	if err := first.Save(context.Background()); err != nil {
		t.Fatal(err)
	}
	winner, _ := os.ReadFile(path)
	if err := second.Save(context.Background()); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale editor did not report conflict: %v", err)
	}
	assertBytes(t, path, winner)
	assertBytes(t, path+".bak", original)
}

func TestDocumentConcurrentSavesHaveOneWinner(t *testing.T) {
	path, original := writeDocument(t, `{"schema_version":1,"destinations":[]}`)
	first, _ := OpenDocument(path)
	second, _ := OpenDocument(path)
	first.Config.PollInterval = "2s"
	second.Config.PollInterval = "3s"
	start := make(chan struct{})
	errorsFound := make(chan error, 2)
	var workers sync.WaitGroup
	for _, doc := range []*Document{first, second} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			errorsFound <- doc.Save(context.Background())
		}()
	}
	close(start)
	workers.Wait()
	close(errorsFound)
	saved, conflicted := 0, 0
	for err := range errorsFound {
		if err == nil {
			saved++
		} else if errors.Is(err, ErrConflict) {
			conflicted++
		} else {
			t.Fatalf("unexpected save failure: %v", err)
		}
	}
	if saved != 1 || conflicted != 1 {
		t.Fatalf("expected one successful save and one conflict, got %d/%d", saved, conflicted)
	}
	assertBytes(t, path+".bak", original)
	if _, err := ReadLocal(path); err != nil {
		t.Fatal("concurrent save left a partial document")
	}
}

func TestDocumentRejectsExternalCreationDeletionAndMalformedReplacement(t *testing.T) {
	for _, mode := range []string{"creation", "deletion", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if mode != "creation" {
				if err := os.WriteFile(path, []byte(`{"schema_version":1,"destinations":[]}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			doc, err := OpenDocument(path)
			if err != nil {
				t.Fatal(err)
			}
			doc.Config.PollInterval = "2s"
			if mode == "deletion" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte(`{"other-writer":`), 0600); err != nil {
				t.Fatal(err)
			}
			if err := doc.Save(context.Background()); !errors.Is(err, ErrConflict) {
				t.Fatalf("external change was overwritten: %v", err)
			}
			assertMissing(t, path+".bak")
			if mode == "deletion" {
				assertMissing(t, path)
			} else {
				assertBytes(t, path, []byte(`{"other-writer":`))
			}
		})
	}
}

func TestDocumentLockRespectsCancellation(t *testing.T) {
	path, original := writeDocument(t, `{"schema_version":1,"destinations":[]}`)
	doc, _ := OpenDocument(path)
	doc.Config.PollInterval = "2s"
	lock := flock.New(path + ".lock")
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := doc.Save(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected cancelled lock acquisition: %v", err)
	}
	assertBytes(t, path, original)
	assertMissing(t, path+".bak")
}

func TestDocumentInvalidEditsCannotPartiallyUpdateFile(t *testing.T) {
	for _, change := range []struct {
		name string
		edit func(*Config)
	}{
		{"URL", func(c *Config) { c.Destinations[0].URL = "file:///private-secret" }},
		{"reference", func(c *Config) { c.Destinations[0].Token = "${private-secret}" }},
		{"ID", func(c *Config) { c.Destinations = append(c.Destinations, c.Destinations[0]) }},
		{"timing", func(c *Config) { c.PollInterval = "0s" }},
		{"new disabled", func(c *Config) { c.Destinations = append(c.Destinations, Destination{ID: "new", Kind: "gotify"}) }},
	} {
		t.Run(change.name, func(t *testing.T) {
			path, original := writeDocument(t, `{"schema_version":1,"destinations":[{"id":"web","kind":"webhook","enabled":true,"url":"https://example.invalid"}]}`)
			doc, _ := OpenDocument(path)
			change.edit(&doc.Config)
			if err := doc.Save(context.Background()); err == nil || strings.Contains(err.Error(), "private-secret") {
				t.Fatalf("invalid edit accepted or secret exposed: %v", err)
			}
			assertBytes(t, path, original)
			assertMissing(t, path+".bak")
			assertMissing(t, path+".lock")
		})
	}
}

func TestDocumentCanDisableBrokenDestinationWithoutRepairingIt(t *testing.T) {
	path, original := writeDocument(t, `{"schema_version":1,"destinations":[{"id":"broken","kind":"gotify","enabled":true,"url":"not-a-url","token":"${unfinished"}]}`)
	doc, err := OpenDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	doc.Config.Destinations[0].Enabled = false
	if err := doc.Save(context.Background()); err != nil {
		t.Fatalf("cannot disable a broken channel: %v", err)
	}
	assertBytes(t, path+".bak", original)
	reopened, err := OpenDocument(path)
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.Config.Destinations[0]
	if got.Enabled || got.URL != "not-a-url" || got.Token != "${unfinished" {
		t.Fatal("disabling changed the channel's preserved fields")
	}
	got.URL = "another-invalid-url"
	reopened.Config.Destinations[0] = got
	if err := reopened.Save(context.Background()); err == nil {
		t.Fatal("arbitrary edits to disabled destinations must still be validated")
	}
}

func TestDocumentBackupFailureLeavesOriginalUntouched(t *testing.T) {
	path, original := writeDocument(t, `{"schema_version":1,"destinations":[]}`)
	doc, _ := OpenDocument(path)
	doc.Config.PollInterval = "2s"
	if err := os.Mkdir(path+".bak", 0700); err != nil {
		t.Fatal(err)
	}
	if err := doc.Save(context.Background()); err == nil {
		t.Fatal("save succeeded after backup failed")
	}
	assertBytes(t, path, original)
	files, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".blip-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("failed save left temporary files: %v", err)
	}
}

func TestDocumentRejectsAmbiguousOrUnknownJSON(t *testing.T) {
	for _, input := range []string{
		`{"schema_version":1,"private-secret":"value","destinations":[]}`,
		`{"schema_version":1,"destinations":[{"id":"x","kind":"webhook","private-secret":"value"}]}`,
		`{"schema_version":1,"schema_version":1,"destinations":[]}`,
		`{"schema_version":1,"Schema_Version":1,"destinations":[]}`,
		`{"schema_version":1,"destinations":[{"id":"x","kind":"webhook","URL":"https://example.invalid"}]}`,
		`{"schema_version":1,"destinations":[]} {}`,
		`{"schema_version":1,"destinations":[{"id":"x","kind":"webhook","headers":{"X-Key":"one","X-Key":"two"}}]}`,
		`{"destinations":[]}`,
		`null`,
		`{"schema_version":1,`,
	} {
		path, original := writeDocument(t, input)
		if _, err := OpenDocument(path); err == nil || strings.Contains(err.Error(), "private-secret") {
			t.Fatalf("ambiguous input accepted or secret exposed: %v", err)
		}
		assertBytes(t, path, original)
		assertMissing(t, path+".bak")
	}
}

func writeDocument(t *testing.T, data string) (string, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	return path, []byte(data)
}

func readFields(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	return fields
}

func assertBytes(t *testing.T, path string, expected []byte) {
	t.Helper()
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, expected) {
		t.Fatalf("file changed unexpectedly: %s (%v)", path, err)
	}
}

func assertMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected file: %s (%v)", path, err)
	}
}

func assertPrivate(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("configuration file permissions must be 0600: %v", info.Mode().Perm())
	}
}
