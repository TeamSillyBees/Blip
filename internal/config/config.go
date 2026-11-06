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
	PollInterval   string        `json:"poll_interval,omitempty"`
	RequestTimeout string        `json:"request_timeout,omitempty"`
	MaxAttempts    int           `json:"max_attempts,omitempty"`
	Destinations   []Destination `json:"destinations"`
	Path           string        `json:"-"`
}

func DefaultPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "Blip", "config.json"), nil
}

func Read(path string) (Config, error) { return read(path, true) }

// ReadLocal lets hooks and lifecycle commands operate without validating channel
// credentials. Channel settings are validated when notifications are sent.
func ReadLocal(path string) (Config, error) { return read(path, false) }

func read(path string, validateChannels bool) (Config, error) {
	var c Config
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
		c.StateDir = filepath.Join(filepath.Dir(absolute), "data")
	}
	if c.CodexHome == "" {
		c.CodexHome = os.Getenv("CODEX_HOME")
	}
	if c.CodexHome == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return c, homeErr
		}
		c.CodexHome = filepath.Join(home, ".codex")
	}
	for _, target := range []*string{&c.StateDir, &c.CodexHome} {
		if !filepath.IsAbs(*target) {
			*target = filepath.Join(filepath.Dir(absolute), *target)
		}
		*target = filepath.Clean(*target)
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
