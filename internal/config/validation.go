package config

import (
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

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

// ValidateDestination checks a candidate even when it is disabled. With resolve
// false, environment references are checked without reading their values. With
// resolve true, a private copy is expanded and checked for sending. The caller's
// destination, including its headers, is never changed. Errors omit credentials.
func ValidateDestination(d Destination, resolve bool) error {
	d = cloneDestination(d)
	return validateDestination(&d, resolve)
}

func validateDestination(d *Destination, resolve bool) error {
	if d.ID == "" {
		return fmt.Errorf("destination ID must be nonempty")
	}
	if !supportedKind(d.Kind) {
		return fmt.Errorf("unsupported destination kind")
	}
	for _, value := range []*string{&d.URL, &d.Token, &d.Secret} {
		if err := validateReferences(*value); err != nil {
			return err
		}
		if resolve {
			if err := expand(value); err != nil {
				return err
			}
		}
	}
	address := d.URL
	if !resolve {
		address = templateURL(address)
	}
	if err := validateURL(address); err != nil {
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
		if resolve {
			if err := expand(&value); err != nil {
				return err
			}
			if !validHeader(key, value) {
				return fmt.Errorf("destination contains an invalid header")
			}
		}
		canonical := http.CanonicalHeaderKey(key)
		if headerNames[canonical] {
			return fmt.Errorf("destination contains duplicate header names")
		}
		headerNames[canonical] = true
		d.Headers[key] = value
	}
	if d.Token != "" && d.Kind == "webhook" && hasAuthorization(d.Headers) {
		return fmt.Errorf("destination must choose token or Authorization header")
	}
	return nil
}

func validateReferences(value string) error {
	for {
		start := strings.Index(value, "${")
		if start < 0 {
			return nil
		}
		value = value[start:]
		match := variablePattern.FindStringIndex(value)
		if match == nil || match[0] != 0 {
			return fmt.Errorf("environment references must use ${VARIABLE_NAME}")
		}
		value = value[match[1]:]
	}
}

// templateURL substitutes harmless syntactic stand-ins, never environment
// values. A reference may contain a whole URL, a scheme, host, port, or path.
// Its actual value is checked again by Read before a notification is sent.
func templateURL(value string) string {
	locations := variablePattern.FindAllStringIndex(value, -1)
	var result strings.Builder
	end := 0
	for _, location := range locations {
		start, stop := location[0], location[1]
		result.WriteString(value[end:start])
		replacement := "blip-env"
		if start == 0 {
			switch {
			case strings.HasPrefix(value[stop:], "://"):
				replacement = "https"
			case stop == len(value), strings.ContainsAny(value[stop:stop+1], "/?#"):
				replacement = "https://example.invalid"
			}
		} else if prefix := result.String(); strings.Contains(prefix, "://") {
			authority := strings.SplitN(prefix, "://", 2)[1]
			if !strings.ContainsAny(authority, "/?#") && strings.HasSuffix(authority, ":") {
				replacement = "1"
			}
		}
		result.WriteString(replacement)
		end = stop
	}
	result.WriteString(value[end:])
	return result.String()
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

// ValidateHeader checks a custom webhook header without expanding environment
// references. It also rejects headers managed by the HTTP sender.
func ValidateHeader(key, value string) error {
	if !validHeader(key, value) {
		return fmt.Errorf("destination contains an invalid or reserved header")
	}
	return validateReferences(value)
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
