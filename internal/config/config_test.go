package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigLiteralSettingsAndRelativePaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := `{"schema_version":1,"state_dir":"state","codex_home":"codex","destinations":[{"id":"custom","kind":"webhook","enabled":true,"url":"https://example.invalid/notify","token":"test-token"},{"id":"off","kind":"feishu","enabled":false,"url":""}]}`
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

func TestLocalCommandsDoNotRequireValidChannelSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	_ = os.WriteFile(path, []byte(`{"schema_version":1,"destinations":[{"id":"one","kind":"gotify","enabled":true,"url":"https://example.invalid"}]}`), 0600)
	if c, err := ReadLocal(path); err != nil || len(c.Enabled()) != 1 {
		t.Fatalf("hook cannot enqueue without credentials: %v", err)
	}
	if _, err := Read(path); err == nil {
		t.Fatal("sending must still require valid channel settings")
	}
}

func TestLiteralDollarSignsInCredentialsArePreserved(t *testing.T) {
	t.Setenv("BLIP_TEST_TOKEN", "not-the-literal-token")
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":1,"destinations":[{"id":"one","kind":"webhook","enabled":true,"url":"https://example.invalid","token":"literal$BLIP_TEST_TOKEN"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Read(path)
	if err != nil || c.Destinations[0].Token != "literal$BLIP_TEST_TOKEN" {
		t.Fatalf("literal credential changed: %v", err)
	}
}

func TestDeviceNameDefaultsForOldConfigurationWithoutChangingFile(t *testing.T) {
	path, original := writeDocument(t, `{"schema_version":1,"destinations":[]}`)
	for _, read := range []func(string) (Config, error){Read, ReadLocal} {
		c, err := read(path)
		if err != nil {
			t.Fatal(err)
		}
		if c.DeviceName != DefaultDeviceName() || ValidateDeviceName(c.DeviceName) != nil {
			t.Fatal("old configuration did not default to a valid device name")
		}
	}
	assertBytes(t, path, original)
	doc, err := OpenDocument(path)
	if err != nil || doc.Config.DeviceName != "" {
		t.Fatalf("editor must retain an absent device name for first setup: %v", err)
	}
}

func TestDeviceNameCustomValueIsUsedByBothReaders(t *testing.T) {
	path, _ := writeDocument(t, `{"schema_version":1,"device_name":"实验室 workstation","destinations":[]}`)
	for _, read := range []func(string) (Config, error){Read, ReadLocal} {
		c, err := read(path)
		if err != nil || c.DeviceName != "实验室 workstation" {
			t.Fatalf("custom device name was not preserved: %v", err)
		}
	}
}

func TestReadRejectsEnvironmentReferencesEvenWhenVariableIsSet(t *testing.T) {
	t.Setenv("BLIP_REMOVED_REFERENCE", "private-token")
	path, _ := writeDocument(t, `{"schema_version":1,"destinations":[{"id":"one","kind":"webhook","enabled":true,"url":"https://example.invalid","token":"${BLIP_REMOVED_REFERENCE}"}]}`)
	if _, err := Read(path); err == nil || !strings.Contains(err.Error(), "no longer supported") || strings.Contains(err.Error(), "private-token") || strings.Contains(err.Error(), "BLIP_REMOVED_REFERENCE") {
		t.Fatalf("environment reference was accepted or leaked its value: %v", err)
	}
}

func TestExampleUsesCurrentDeviceNameAndLiteralSettings(t *testing.T) {
	data, err := ExampleBytes()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "${") {
		t.Fatal("example still contains environment references")
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	if c.DeviceName != DefaultDeviceName() || len(c.Destinations) == 0 {
		t.Fatal("example did not include this computer's name and sample channels")
	}
	for _, d := range c.Destinations {
		if d.Enabled {
			t.Fatal("sample destinations must start disabled")
		}
		if err := ValidateDestination(d); err != nil {
			t.Fatalf("sample has invalid literal fields: %v", err)
		}
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
