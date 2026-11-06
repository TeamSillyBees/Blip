package config

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// DefaultDeviceName returns this computer's hostname for new configurations and
// older files that do not specify a device name.
func DefaultDeviceName() string {
	name, err := os.Hostname()
	if err != nil || ValidateDeviceName(name) != nil {
		return "Unknown device"
	}
	return name
}

// ValidateDeviceName accepts a short, single-line name for notifications.
func ValidateDeviceName(name string) error {
	if !utf8.ValidString(name) || name == "" || strings.TrimSpace(name) != name || utf8.RuneCountInString(name) > 128 {
		return fmt.Errorf("device name must contain 1 to 128 characters without leading or trailing spaces")
	}
	for _, char := range name {
		if unicode.IsControl(char) || char == '\u2028' || char == '\u2029' {
			return fmt.Errorf("device name must not contain control characters or line breaks")
		}
	}
	return rejectReference(name)
}

// Clone returns independent settings suitable for staging an edit.
func (c Config) Clone() Config {
	if c.Destinations != nil {
		destinations := make([]Destination, len(c.Destinations))
		for i, d := range c.Destinations {
			destinations[i] = cloneDestination(d)
		}
		c.Destinations = destinations
	}
	return c
}

func cloneDestination(d Destination) Destination {
	if d.Headers != nil {
		headers := make(map[string]string, len(d.Headers))
		for name, value := range d.Headers {
			headers[name] = value
		}
		d.Headers = headers
	}
	return d
}

func validateStructure(c Config) error {
	if c.SchemaVersion != 1 {
		return fmt.Errorf("configuration schema_version must be 1")
	}
	if c.DeviceName != "" {
		if err := ValidateDeviceName(c.DeviceName); err != nil {
			return err
		}
	}
	if c.MaxAttempts < 0 || c.MaxAttempts > 20 {
		return fmt.Errorf("max_attempts must be between 1 and 20 (or omitted)")
	}
	for name, value := range map[string]string{"poll_interval": c.PollInterval, "request_timeout": c.RequestTimeout} {
		if value == "" {
			continue
		}
		duration, err := time.ParseDuration(value)
		if err != nil || duration < 100*time.Millisecond || duration > time.Minute {
			return fmt.Errorf("%s must be between 100ms and 1m", name)
		}
	}
	seen := make(map[string]bool)
	for _, d := range c.Destinations {
		if d.ID == "" || seen[d.ID] {
			return fmt.Errorf("destination IDs must be nonempty and unique")
		}
		seen[d.ID] = true
		if !supportedKind(d.Kind) {
			return fmt.Errorf("unsupported destination kind")
		}
	}
	return nil
}

func supportedKind(kind string) bool {
	switch kind {
	case "feishu", "wecom", "gotify", "webhook":
		return true
	default:
		return false
	}
}

// ValidateDestination checks literal channel settings even when the candidate
// is disabled. It never changes the destination and errors omit credentials.
func ValidateDestination(d Destination) error {
	if d.ID == "" {
		return fmt.Errorf("destination ID must be nonempty")
	}
	if !supportedKind(d.Kind) {
		return fmt.Errorf("unsupported destination kind")
	}
	for _, value := range []string{d.URL, d.Token, d.Secret} {
		if err := rejectReference(value); err != nil {
			return err
		}
	}
	if err := validateURL(d.URL); err != nil {
		return err
	}
	if d.Kind == "gotify" && d.Token == "" {
		return fmt.Errorf("destination requires an application token")
	}
	if d.Kind == "gotify" || d.Kind == "webhook" {
		for _, char := range []byte(d.Token) {
			if char <= ' ' || char == 127 {
				return fmt.Errorf("token contains whitespace or control characters")
			}
		}
	}
	headerNames := make(map[string]bool)
	for key, value := range d.Headers {
		if d.Kind != "webhook" {
			return fmt.Errorf("custom headers are only supported for webhook destinations")
		}
		if err := ValidateHeader(key, value); err != nil {
			return err
		}
		canonical := http.CanonicalHeaderKey(key)
		if headerNames[canonical] {
			return fmt.Errorf("destination contains duplicate header names")
		}
		headerNames[canonical] = true
	}
	if d.Token != "" && d.Kind == "webhook" && hasAuthorization(d.Headers) {
		return fmt.Errorf("destination must choose token or Authorization header")
	}
	return nil
}

func rejectReference(value string) error {
	if strings.Contains(value, "${") {
		return fmt.Errorf("environment variable references are no longer supported; enter a literal value")
	}
	return nil
}

func validateURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.Fragment != "" || strings.Contains(value, "#") {
		return fmt.Errorf("destination requires an http(s) URL without userinfo or fragment")
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return fmt.Errorf("destination has an invalid URL port")
		}
	}
	return nil
}

// ValidateHeader checks a literal custom webhook header and rejects headers
// managed by the HTTP sender.
func ValidateHeader(key, value string) error {
	if !validHeader(key, value) {
		return fmt.Errorf("destination contains an invalid or reserved header")
	}
	return rejectReference(value)
}

func validHeader(key, value string) bool {
	if key == "" {
		return false
	}
	for _, char := range []byte(value) {
		if char == 127 || (char < ' ' && char != '\t') {
			return false
		}
	}
	for _, char := range key {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", char)) {
			return false
		}
	}
	canonical := http.CanonicalHeaderKey(key)
	return canonical != "Host" && canonical != "Content-Length" && canonical != "Content-Type" && canonical != "Transfer-Encoding" && canonical != "X-Blip-Event-Id"
}

func hasAuthorization(headers map[string]string) bool {
	for key := range headers {
		if strings.EqualFold(key, "Authorization") {
			return true
		}
	}
	return false
}
