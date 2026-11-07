package setup

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/TeamSillyBees/Blip/internal/config"
)

func TestExistingConfigurationVersionsPreservePathsCredentialsAndIdentities(t *testing.T) {
	for name, sourceFields := range map[string]string{
		"legacy": `"schema_version":1,"codex_home":"../legacy home"`,
		"multiple sources": `"schema_version":2,"sources":[
  {"id":"codex-default","harness":"codex","enabled":true,"home":"../legacy home","event_version":1},
  {"id":"claude-work","harness":"claude-code","enabled":true,"home":"relative claude home"},
  {"id":"future","harness":"future-agent","enabled":false,"home":"relative future home","event_version":2}
]`,
		"empty sources": `"schema_version":2,"sources":[]`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "existing.json")
			original := []byte(`{` + sourceFields + `,
  "device_name":"Original device",
  "state_dir":"relative state",
  "max_attempts":0,
  "destinations":[
    {"id":"stable-existing","kind":"webhook","enabled":true,"url":"https://example.invalid/notify","token":"literal$TOKEN","headers":{"X-Key":"literal-secret"}},
    {"id":"disabled-example","kind":"gotify","enabled":false,"token":"${UNCHANGED_EXAMPLE}","priority":0}
  ]
}
`)
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			before, err := config.OpenDocument(path)
			if err != nil {
				t.Fatal(err)
			}
			p, output := runScript(t, path, selectStep("save"))
			unchanged, _ := os.ReadFile(path)
			if p.result.Changed || p.result.Apply || !bytes.Equal(unchanged, original) {
				t.Fatal("unchanged save rewrote existing configuration or requested runtime changes")
			}
			if _, err := os.Stat(path + ".bak"); !os.IsNotExist(err) {
				t.Fatal("unchanged save replaced a backup")
			}
			p, renamedOutput := runScript(t, path, selectStep("device"), inputStep("Renamed device"), selectStep("save"))
			after, err := config.OpenDocument(path)
			if err != nil {
				t.Fatal(err)
			}
			want := before.Config.Clone()
			want.DeviceName = "Renamed device"
			if !reflect.DeepEqual(after.Config, want) {
				t.Fatal("editing device name changed schema, sources, paths, identities, credentials or omitted defaults")
			}
			backup, _ := os.ReadFile(path + ".bak")
			if !bytes.Equal(backup, original) {
				t.Fatal("save did not preserve the original configuration backup")
			}
			if _, err := config.Read(path); err != nil {
				t.Fatalf("edited existing configuration no longer loads: %v", err)
			}
			for _, secret := range []string{"literal$TOKEN", "literal-secret", "${UNCHANGED_EXAMPLE}"} {
				if strings.Contains(output+renamedOutput+p.shown.String(), secret) {
					t.Fatal("configuration summary disclosed a credential")
				}
			}
		})
	}
}
