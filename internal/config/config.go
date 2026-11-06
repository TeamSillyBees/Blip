// Package config loads local settings without copying credentials into events.
package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type Destination struct {
	ID       string            `json:"id"`
	Kind     string            `json:"kind"`
	Enabled  bool              `json:"enabled"`
	URL      string            `json:"url"`
	Token    string            `json:"token,omitempty"`
	Secret   string            `json:"secret,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Priority int               `json:"priority,omitempty"`
}

type Config struct {
	SchemaVersion  int           `json:"schema_version"`
	DeviceName     string        `json:"device_name,omitempty"`
	StateDir       string        `json:"state_dir,omitempty"`
	CodexHome      string        `json:"codex_home,omitempty"`
	Sources        []Source      `json:"sources,omitempty"`
	PollInterval   string        `json:"poll_interval,omitempty"`
	RequestTimeout string        `json:"request_timeout,omitempty"`
	MaxAttempts    int           `json:"max_attempts,omitempty"`
	Destinations   []Destination `json:"destinations"`
	Path           string        `json:"-"`
}

// RuntimeDefaults preserves paths captured from the installing user's profile
// when a service runs under a different account. Nonempty paths must be absolute.
type RuntimeDefaults struct {
	StateDir  string
	CodexHome string
}

func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "Blip", "config.json"), nil
}

func Read(path string) (Config, error) { return read(path, true, RuntimeDefaults{}) }

// ReadWithDefaults validates the configuration for sending and uses the supplied
// paths only when their config fields are empty. Explicit relative config paths
// remain relative to the configuration directory, never the service's directory.
func ReadWithDefaults(path string, defaults RuntimeDefaults) (Config, error) {
	return read(path, true, defaults)
}

// ReadLocal lets hooks and lifecycle commands operate without validating channel
// credentials. Channel settings are validated when notifications are sent.
func ReadLocal(path string) (Config, error) { return read(path, false, RuntimeDefaults{}) }

func read(path string, validateChannels bool, defaults RuntimeDefaults) (Config, error) {
	var c Config
	for _, entry := range []struct{ name, path string }{
		{"state_dir", defaults.StateDir},
		{"codex_home", defaults.CodexHome},
	} {
		if entry.path != "" && !filepath.IsAbs(entry.path) {
			return c, fmt.Errorf("runtime default %s must be an absolute path", entry.name)
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return c, err
	}
	file, err := os.Open(absolute)
	if err != nil {
		return c, fmt.Errorf("open configuration: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&c); err != nil {
		return c, fmt.Errorf("invalid configuration JSON: %w", err)
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return c, fmt.Errorf("configuration must contain exactly one JSON object")
	}
	c.Path = absolute
	if err := validateStructure(c); err != nil {
		return c, err
	}
	if c.DeviceName == "" {
		c.DeviceName = DefaultDeviceName()
	}
	if c.PollInterval == "" {
		c.PollInterval = "1s"
	}
	if c.RequestTimeout == "" {
		c.RequestTimeout = "5s"
	}
	if c.MaxAttempts == 0 {
		c.MaxAttempts = 5
	}
	if c.StateDir == "" {
		c.StateDir = defaults.StateDir
	}
	if c.StateDir == "" {
		c.StateDir = filepath.Join(filepath.Dir(absolute), "data")
	}
	if c.SchemaVersion == 1 && c.CodexHome == "" {
		c.CodexHome = defaults.CodexHome
	}
	if c.SchemaVersion == 1 && c.CodexHome == "" {
		c.CodexHome = os.Getenv("CODEX_HOME")
	}
	if c.SchemaVersion == 1 && c.CodexHome == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return c, homeErr
		}
		c.CodexHome = filepath.Join(home, ".codex")
	}
	for _, target := range []*string{&c.StateDir, &c.CodexHome} {
		if *target == "" {
			continue
		}
		if !filepath.IsAbs(*target) {
			*target = filepath.Join(filepath.Dir(absolute), *target)
		}
		*target = filepath.Clean(*target)
	}
	for i := range c.Sources {
		if c.Sources[i].EventVersion == 0 {
			c.Sources[i].EventVersion = 2
		}
		c.Sources[i].Home = absoluteHome(absolute, c.Sources[i].Home)
	}
	for i := range c.Destinations {
		d := &c.Destinations[i]
		if !d.Enabled || !validateChannels {
			continue
		}
		if err := ValidateDestination(*d); err != nil {
			return c, fmt.Errorf("destination %q: %w", d.ID, err)
		}
	}
	return c, nil
}

func (c Config) PollDuration() time.Duration {
	value, _ := time.ParseDuration(c.PollInterval)
	return value
}
func (c Config) TimeoutDuration() time.Duration {
	value, _ := time.ParseDuration(c.RequestTimeout)
	return value
}
func (c Config) Enabled() []Destination {
	result := []Destination{}
	for _, d := range c.Destinations {
		if d.Enabled {
			result = append(result, d)
		}
	}
	return result
}
