package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigEnvironmentAndRelativePaths(t *testing.T) {
	t.Setenv("BLIP_TEST_URL", "https://example.invalid/notify")
	t.Setenv("BLIP_TEST_TOKEN", "test-token")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := `{"schema_version":1,"state_dir":"state","codex_home":"codex","destinations":[{"id":"custom","kind":"webhook","enabled":true,"url":"${BLIP_TEST_URL}","token":"${BLIP_TEST_TOKEN}"},{"id":"off","kind":"feishu","enabled":false,"url":"${UNSET_EXAMPLE_VARIABLE}"}]}`
	_ = os.WriteFile(path, []byte(data), 0600)
	c, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.StateDir != filepath.Join(dir, "state") || c.CodexHome != filepath.Join(dir, "codex") || c.Destinations[0].Token != "test-token" || len(c.Enabled()) != 1 {
		t.Fatal("configuration resolution failed")
	}
}

func TestInvalidConfigDoesNotLeakCredentials(t *testing.T) {
	for _, suffix := range []string{`,"token":"${BLIP_MISSING_TEST_TOKEN}"`, ` ,"headers":{"Authorization":"one"},"token":"two"`, ` ,"headers":{"Bad Header":"secret"}`} {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		data := `{"schema_version":1,"destinations":[{"id":"custom","kind":"webhook","enabled":true,"url":"https://example.invalid/?key=secret"` + suffix + `}]}`
		_ = os.WriteFile(path, []byte(data), 0600)
		_, err := Read(path)
		if err == nil || strings.Contains(err.Error(), "key=secret") {
			t.Fatalf("unsafe config validation: %v", err)
		}
	}
}

func TestLocalCommandsDoNotRequireNotificationSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	_ = os.WriteFile(path, []byte(`{"schema_version":1,"destinations":[{"id":"one","kind":"gotify","enabled":true,"url":"${BLIP_URL_UNAVAILABLE_IN_CODEX}","token":"${BLIP_TOKEN_UNAVAILABLE_IN_CODEX}"}]}`), 0600)
	if c, err := ReadLocal(path); err != nil || len(c.Enabled()) != 1 {
		t.Fatalf("hook cannot enqueue without credentials: %v", err)
	}
	if _, err := Read(path); err == nil {
		t.Fatal("sending must still require resolved credentials")
	}
}

func TestLiteralDollarSignsInCredentialsArePreserved(t *testing.T) {
	value := "literal$credential"
	if err := expand(&value); err != nil || value != "literal$credential" {
		t.Fatal("literal credential was expanded")
	}
}

func TestDestinationRejectsUnsendableSettings(t *testing.T) {
	for _, entry := range []struct {
		name  string
		apply func(*Destination)
	}{
		{"empty hostname", func(d *Destination) { d.URL = "http://:8080/?key=private-url" }},
		{"zero port", func(d *Destination) { d.URL = "https://example.invalid:0/?key=private-url" }},
		{"port out of range", func(d *Destination) { d.URL = "https://example.invalid:65536/?key=private-url" }},
		{"webhook token space", func(d *Destination) { d.Token = "private token" }},
		{"webhook token newline", func(d *Destination) { d.Token = "private-token\n" }},
		{"gotify token control", func(d *Destination) { d.Kind, d.Token = "gotify", "private-token\x00" }},
		{"gotify token delete", func(d *Destination) { d.Kind, d.Token = "gotify", "private-token\x7f" }},
		{"header control", func(d *Destination) { d.Headers = map[string]string{"X-Key": "private-header\x00"} }},
		{"header delete", func(d *Destination) { d.Headers = map[string]string{"X-Key": "private-header\x7f"} }},
		{"duplicate header spelling", func(d *Destination) {
			d.Headers = map[string]string{"X-Key": "private-header", "x-key": "another-private-header"}
		}},
		{"reserved event identity", func(d *Destination) { d.Headers = map[string]string{"x-blip-event-id": "private-header"} }},
	} {
		t.Run(entry.name, func(t *testing.T) {
			destination := Destination{ID: "test", Kind: "webhook", Enabled: true, URL: "https://example.invalid/?key=private-url"}
			entry.apply(&destination)
			data, err := json.Marshal(Config{SchemaVersion: 1, Destinations: []Destination{destination}})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Read(path); err == nil || strings.Contains(err.Error(), "private") {
				t.Fatalf("configuration must reject invalid settings without exposing credentials: %v", err)
			}
		})
	}
}

func TestDestinationAllowsValidHeaderWhitespaceAndPortBoundaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	data := `{"schema_version":1,"destinations":[{"id":"low","kind":"webhook","enabled":true,"url":"http://localhost:1/","headers":{"X-Label":"two words\tand a tab"}},{"id":"high","kind":"gotify","enabled":true,"url":"http://[::1]:65535/","token":"valid-token"}]}`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path); err != nil {
		t.Fatalf("valid settings rejected: %v", err)
	}
}
