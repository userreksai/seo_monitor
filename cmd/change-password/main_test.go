package main

import (
	"strings"
	"testing"
)

func TestReadPasswordPreservesSpaces(t *testing.T) {
	got, err := readPassword(strings.NewReader("  new password  \r\n"))
	if err != nil {
		t.Fatalf("readPassword() error: %v", err)
	}
	if want := "  new password  "; got != want {
		t.Fatalf("readPassword() = %q, want %q", got, want)
	}
}

func TestReadPasswordAcceptsFinalLineWithoutNewline(t *testing.T) {
	got, err := readPassword(strings.NewReader("new password"))
	if err != nil {
		t.Fatalf("readPassword() error: %v", err)
	}
	if want := "new password"; got != want {
		t.Fatalf("readPassword() = %q, want %q", got, want)
	}
}
