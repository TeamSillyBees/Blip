package config

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestDestinationRejectsEnvironmentReferencesInAllValues(t *testing.T) {
	t.Setenv("BLIP_REMOVED_REFERENCE", "private-secret")
	for _, change := range []struct {
		name string
		edit func(*Destination)
	}{
		{"URL", func(d *Destination) { d.URL = "${BLIP_REMOVED_REFERENCE}" }},
		{"URL component", func(d *Destination) { d.URL = "https://example.invalid/?key=${BLIP_REMOVED_REFERENCE}" }},
		{"token", func(d *Destination) { d.Token = "${BLIP_REMOVED_REFERENCE}" }},
		{"secret", func(d *Destination) { d.Secret = "${BLIP_REMOVED_REFERENCE}" }},
		{"header", func(d *Destination) { d.Headers = map[string]string{"X-Key": "${BLIP_REMOVED_REFERENCE}"} }},
		{"unfinished reference", func(d *Destination) { d.Token = "${BLIP_REMOVED_REFERENCE" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			d := Destination{ID: "example", Kind: "webhook", URL: "https://example.invalid/"}
			change.edit(&d)
			before := cloneDestination(d)
			err := ValidateDestination(d)
			if err == nil || !strings.Contains(err.Error(), "no longer supported") || strings.Contains(err.Error(), "private-secret") || strings.Contains(err.Error(), "BLIP_REMOVED_REFERENCE") {
				t.Fatalf("reference accepted or its value leaked: %v", err)
			}
			if !reflect.DeepEqual(d, before) {
				t.Fatal("failed validation modified a destination")
			}
		})
	}
}

func TestDestinationValidationRejectsBadLiterals(t *testing.T) {
	for _, change := range []struct {
		name string
		edit func(*Destination)
	}{
		{"URL scheme", func(d *Destination) { d.URL = "ftp://example.invalid/private" }},
		{"URL port", func(d *Destination) { d.URL = "https://example.invalid:70000/private" }},
		{"URL userinfo", func(d *Destination) { d.URL = "https://private@example.invalid/" }},
		{"URL fragment", func(d *Destination) { d.URL = "https://example.invalid/#private" }},
		{"empty URL", func(d *Destination) { d.URL = "" }},
		{"token whitespace", func(d *Destination) { d.Token = "private token" }},
		{"gotify missing token", func(d *Destination) { d.Kind, d.Token = "gotify", "" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			d := Destination{ID: "example", Kind: "webhook", URL: "https://example.invalid/"}
			change.edit(&d)
			if err := ValidateDestination(d); err == nil || strings.Contains(err.Error(), "private") {
				t.Fatalf("invalid candidate accepted or a value leaked: %v", err)
			}
		})
	}
}

func TestDestinationValidationPreservesLiteralCredentials(t *testing.T) {
	t.Setenv("BLIP_VALIDATE_TOKEN", "different-environment-value")
	d := Destination{ID: "example", Kind: "webhook", Enabled: true, URL: "https://example.invalid/", Token: "$BLIP_VALIDATE_TOKEN", Headers: map[string]string{"X-Key": "private-header"}}
	initial := cloneDestination(d)
	if err := ValidateDestination(d); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d, initial) {
		t.Fatal("validation mutated source values")
	}
}

func TestDefaultDeviceNameUsesHostname(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil {
		t.Skip("system hostname is unavailable")
	}
	if err := ValidateDeviceName(hostname); err != nil {
		t.Skip("system hostname cannot be used as a device name")
	}
	if got := DefaultDeviceName(); got != hostname {
		t.Fatalf("default device name is %q, expected hostname %q", got, hostname)
	}
}

func TestDeviceNameValidation(t *testing.T) {
	for _, name := range []string{"workstation-1", "实验室电脑", "Work laptop 💻", strings.Repeat("界", 128)} {
		if err := ValidateDeviceName(name); err != nil {
			t.Fatalf("valid device name rejected: %v", err)
		}
	}
	for _, name := range []string{"", " ", " leading", "trailing ", "line\nbreak", "tab\tname", "delete\x7f", "control\u0085", "line\u2028break", "paragraph\u2029break", string([]byte{0xff}), strings.Repeat("界", 129), "${DEVICE_NAME}"} {
		if err := ValidateDeviceName(name); err == nil {
			t.Fatal("invalid device name accepted")
		}
	}
}
