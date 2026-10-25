// Package config loads local settings without copying credentials into events.
package config

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

// ReadLocal lets hooks and lifecycle commands operate without channel credentials.
// In particular, a running Codex desktop need not inherit a newly set token variable.
func ReadLocal(path string) (Config, error) { return read(path, false) }

func read(path string, resolve bool) (Config, error) {
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
	if c.SchemaVersion != 1 {
		return c, fmt.Errorf("configuration schema_version must be 1")
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
	if c.MaxAttempts < 1 || c.MaxAttempts > 20 {
		return c, fmt.Errorf("max_attempts must be between 1 and 20")
	}
	for name, value := range map[string]string{"poll_interval": c.PollInterval, "request_timeout": c.RequestTimeout} {
		duration, parseErr := time.ParseDuration(value)
		if parseErr != nil || duration < 100*time.Millisecond || duration > time.Minute {
			return c, fmt.Errorf("%s must be between 100ms and 1m", name)
		}
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
	seen := map[string]bool{}
	for i := range c.Destinations {
		d := &c.Destinations[i]
		if d.ID == "" || seen[d.ID] {
			return c, fmt.Errorf("destination IDs must be nonempty and unique")
		}
		seen[d.ID] = true
		switch d.Kind {
		case "feishu", "wecom", "gotify", "webhook":
		default:
			return c, fmt.Errorf("unsupported destination kind for %q", d.ID)
		}
		if !d.Enabled || !resolve {
			continue
		}
		for _, value := range []*string{&d.URL, &d.Token, &d.Secret} {
			if err := expand(value); err != nil {
				return c, fmt.Errorf("destination %q: %w", d.ID, err)
			}
		}
		parsed, parseErr := url.Parse(d.URL)
		if parseErr != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.Fragment != "" {
			return c, fmt.Errorf("destination %q requires an http(s) URL without userinfo or fragment", d.ID)
		}
		if d.Kind == "gotify" && d.Token == "" {
			return c, fmt.Errorf("destination %q requires an application token", d.ID)
		}
		for key, value := range d.Headers {
			if d.Kind != "webhook" {
				return c, fmt.Errorf("custom headers are only supported for webhook destinations")
			}
			if err := expand(&value); err != nil {
				return c, fmt.Errorf("destination %q: %w", d.ID, err)
			}
			if !validHeader(key, value) {
				return c, fmt.Errorf("destination %q contains an invalid header", d.ID)
			}
			d.Headers[key] = value
		}
		if d.Token != "" && d.Kind == "webhook" && hasAuthorization(d.Headers) {
			return c, fmt.Errorf("destination %q must choose token or Authorization header", d.ID)
		}
	}
	return c, nil
}

var variablePattern = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)

func expand(value *string) error {
	var missing bool
	*value = variablePattern.ReplaceAllStringFunc(*value, func(reference string) string {
		name := reference[2 : len(reference)-1]
		result, ok := os.LookupEnv(name)
		if !ok {
			missing = true
		}
		return result
	})
	if missing {
		return fmt.Errorf("a referenced environment variable is not set")
	}
	return nil
}

func validHeader(key, value string) bool {
	if key == "" || strings.ContainsAny(value, "\r\n") {
		return false
	}
	for _, char := range key {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", char)) {
			return false
		}
	}
	canonical := http.CanonicalHeaderKey(key)
	return canonical != "Host" && canonical != "Content-Length" && canonical != "Content-Type" && canonical != "Transfer-Encoding"
}

func hasAuthorization(headers map[string]string) bool {
	for key := range headers {
		if strings.EqualFold(key, "Authorization") {
			return true
		}
	}
	return false
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
