package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestDestinationStaticValidationAcceptsUnresolvedReferences(t *testing.T) {
	for _, address := range []string{
		"${BLIP_MISSING_URL}",
		"${BLIP_MISSING_BASE}/path?key=${BLIP_MISSING_KEY}",
		"https://${BLIP_MISSING_HOST}:${BLIP_MISSING_PORT}/path",
		"${BLIP_MISSING_SCHEME}://example.invalid/",
		"https://example.invalid/path?key=${BLIP_MISSING_KEY}",
	} {
		destination := Destination{ID: "example", Kind: "webhook", URL: address, Token: "${BLIP_MISSING_TOKEN}", Headers: map[string]string{"X-Key": "prefix ${BLIP_MISSING_HEADER}"}}
		if err := ValidateDestination(destination, false); err != nil {
			t.Fatalf("valid static reference rejected: %v", err)
		}
	}
}

func TestDestinationStaticValidationRejectsBadLiteralsAndReferences(t *testing.T) {
	for _, change := range []struct {
		name string
		edit func(*Destination)
	}{
		{"bad syntax", func(d *Destination) { d.Token = "${private-secret}" }},
		{"unclosed", func(d *Destination) { d.URL = "https://example.invalid/${private" }},
		{"bad first character", func(d *Destination) { d.Secret = "${0private}" }},
		{"nested", func(d *Destination) { d.Token = "${${private}}" }},
		{"header syntax", func(d *Destination) { d.Headers = map[string]string{"X-Key": "${private!}"} }},
		{"literal URL scheme", func(d *Destination) { d.URL = "ftp://${PRIVATE_HOST}/" }},
		{"literal URL port", func(d *Destination) { d.URL = "https://${PRIVATE_HOST}:70000/" }},
		{"literal URL userinfo", func(d *Destination) { d.URL = "https://private@${PRIVATE_HOST}/" }},
		{"literal URL fragment", func(d *Destination) { d.URL = "${PRIVATE_URL}#private" }},
		{"empty URL", func(d *Destination) { d.URL = "" }},
		{"literal token whitespace", func(d *Destination) { d.Token = "private ${PRIVATE_TOKEN}" }},
		{"gotify missing token", func(d *Destination) { d.Kind, d.Token = "gotify", "" }},
	} {
		t.Run(change.name, func(t *testing.T) {
			d := Destination{ID: "example", Kind: "webhook", URL: "https://example.invalid/"}
			change.edit(&d)
			if err := ValidateDestination(d, false); err == nil || strings.Contains(err.Error(), "private") {
				t.Fatalf("invalid candidate accepted or a value leaked: %v", err)
			}
		})
	}
}

func TestDestinationValidationNeverMutatesInputsOrLeaksExpandedSecrets(t *testing.T) {
	t.Setenv("BLIP_VALIDATE_URL", "https://example.invalid/")
	t.Setenv("BLIP_VALIDATE_HEADER", "private-header")
	t.Setenv("BLIP_VALIDATE_TOKEN", "private-token")
	d := Destination{ID: "example", Kind: "webhook", Enabled: true, URL: "${BLIP_VALIDATE_URL}", Token: "${BLIP_VALIDATE_TOKEN}", Headers: map[string]string{"X-Key": "${BLIP_VALIDATE_HEADER}"}}
	initial := cloneDestination(d)
	if err := ValidateDestination(d, true); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d, initial) {
		t.Fatal("resolving validation mutated source values")
	}
	t.Setenv("BLIP_VALIDATE_HEADER", "private-secret\n")
	if err := ValidateDestination(d, true); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("expanded secret accepted or leaked: %v", err)
	}
	if !reflect.DeepEqual(d, initial) {
		t.Fatal("failed validation mutated source values")
	}
}
