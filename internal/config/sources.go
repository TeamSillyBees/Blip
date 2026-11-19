package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

const DefaultSourceID = "codex-default"

type Source struct {
	ID           string `json:"id"`
	Harness      string `json:"harness"`
	EventVersion int    `json:"event_version,omitempty"`
	Enabled      bool   `json:"enabled"`
	Home         string `json:"home"`
}

func (s Source) Resolved(configPath string) Source {
	s.Home = absoluteHome(configPath, s.Home)
	if s.EventVersion == 0 {
		s.EventVersion = 2
	}
	return s
}

// Keep an explicit empty v2 list: omitting it would silently lose the user's
// decision to collect nothing. Legacy documents retain their original shape.
func (c Config) MarshalJSON() ([]byte, error) {
	type alias Config
	var sources *[]Source
	if c.SchemaVersion == 2 {
		sources = &c.Sources
	}
	return json.Marshal(struct {
		alias
		Sources *[]Source `json:"sources,omitempty"`
	}{alias(c), sources})
}

// EffectiveSources does not rewrite legacy configuration or move its state.
func (c Config) EffectiveSources() []Source {
	if c.SchemaVersion == 2 {
		return append([]Source{}, c.Sources...)
	}
	return []Source{{ID: DefaultSourceID, Harness: "codex", EventVersion: 1, Enabled: true, Home: c.CodexHome}}
}

func (c Config) Source(id string) (Source, bool) {
	for _, s := range c.EffectiveSources() {
		if s.ID == id {
			return s, true
		}
	}
	return Source{}, false
}

var safeIdentifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func ValidSourceID(id string) bool {
	if !safeIdentifier.MatchString(id) {
		return false
	}
	// These basenames are devices, even with extensions, on Windows.
	upper := strings.ToUpper(id)
	switch upper {
	case "CON", "PRN", "AUX", "NUL", "CLOCK$":
		return false
	}
	if len(upper) == 4 && (strings.HasPrefix(upper, "COM") || strings.HasPrefix(upper, "LPT")) && upper[3] >= '0' && upper[3] <= '9' {
		return false
	}
	return true
}

func absoluteHome(configPath, home string) string {
	if !filepath.IsAbs(home) {
		home = filepath.Join(filepath.Dir(configPath), home)
	}
	abs, err := filepath.Abs(home)
	if err == nil {
		return filepath.Clean(abs)
	}
	return filepath.Clean(home)
}

// CanonicalHome resolves existing parent symlinks too, so a missing leaf below
// an aliased home cannot register the same environment twice.
func CanonicalHome(home string) (string, error) {
	abs, err := filepath.Abs(home)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(abs)
	var suffix []string
	for {
		resolved, resolveErr := resolveHomePath(current)
		if resolveErr == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			if runtime.GOOS == "windows" {
				resolved = strings.ToLower(resolved)
			}
			return resolved, nil
		}
		if !os.IsNotExist(resolveErr) {
			return "", resolveErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", resolveErr
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

func validateSources(c Config) error {
	if c.SchemaVersion == 1 {
		if c.Sources != nil {
			return fmt.Errorf("sources requires configuration schema_version 2")
		}
		return nil
	}
	if c.Sources == nil {
		return fmt.Errorf("configuration v2 requires an explicit sources array")
	}
	if c.CodexHome != "" {
		return fmt.Errorf("configuration v2 uses sources instead of codex_home")
	}
	ids, homes := map[string]bool{}, map[string]bool{}
	for _, s := range c.Sources {
		id := strings.ToLower(s.ID)
		if !ValidSourceID(s.ID) || ids[id] {
			return fmt.Errorf("source IDs must be unique, safe ASCII directory names")
		}
		ids[id] = true
		if !safeIdentifier.MatchString(s.Harness) || s.Home == "" || strings.ContainsRune(s.Home, 0) {
			return fmt.Errorf("source %q requires a harness and home", s.ID)
		}
		if strings.EqualFold(s.ID, DefaultSourceID) && (s.ID != DefaultSourceID || s.Harness != "codex") {
			return fmt.Errorf("codex-default is reserved for the legacy Codex source")
		}
		if s.EventVersion != 0 && s.EventVersion != 2 && !(s.ID == DefaultSourceID && s.EventVersion == 1) {
			return fmt.Errorf("event_version 1 is only supported for codex-default; other sources require 2")
		}
		home, err := CanonicalHome(absoluteHome(c.Path, s.Home))
		if err != nil {
			// An inaccessible source is a runtime diagnostic, not a reason to
			// stop delivery for the entire configuration.
			home = absoluteHome(c.Path, s.Home)
			if runtime.GOOS == "windows" {
				home = strings.ToLower(home)
			}
		}
		key := s.Harness + "\x00" + home
		if homes[key] {
			return fmt.Errorf("duplicate home for harness %q", s.Harness)
		}
		homes[key] = true
	}
	return nil
}

// MigrateSources stages a v1 -> v2 change. Save retains the document's backup
// and stale-write checks; ordinary reads never migrate on disk.
func (d *Document) MigrateSources(defaults RuntimeDefaults) error {
	if d.Config.SchemaVersion == 2 {
		return nil
	}
	resolved, err := d.Config.Clone().resolve(false, defaults)
	if err != nil {
		return err
	}
	d.Config.Sources = resolved.EffectiveSources()
	d.Config.CodexHome = ""
	d.Config.SchemaVersion = 2
	return nil
}
