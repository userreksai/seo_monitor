package httpapi

import (
	"strings"
	"testing"
)

func TestNormalizeDomainTag(t *testing.T) {
	t.Parallel()

	tag, err := normalizeDomainTag("  重点站点  ")
	if err != nil {
		t.Fatalf("normalizeDomainTag returned error: %v", err)
	}
	if tag != "重点站点" {
		t.Fatalf("normalizeDomainTag = %q, want %q", tag, "重点站点")
	}

	if _, err := normalizeDomainTag(strings.Repeat("签", 51)); err == nil {
		t.Fatal("normalizeDomainTag accepted a tag longer than 50 characters")
	}
}
