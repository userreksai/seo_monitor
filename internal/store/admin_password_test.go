package store

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateNewPassword(t *testing.T) {
	tests := []struct {
		name     string
		password string
		wantErr  bool
	}{
		{name: "minimum length", password: "123456789012"},
		{name: "spaces are preserved", password: "a secure password"},
		{name: "too short", password: "12345678901", wantErr: true},
		{name: "too long", password: strings.Repeat("a", maxPasswordBytes+1), wantErr: true},
		{name: "line break", password: "password\nnext", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateNewPassword(tt.password)
			if tt.wantErr && !errors.Is(err, ErrInvalidPassword) {
				t.Fatalf("validateNewPassword() error = %v, want ErrInvalidPassword", err)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validateNewPassword() unexpected error: %v", err)
			}
		})
	}
}

func TestValidateForcedPassword(t *testing.T) {
	tests := []struct {
		name     string
		password string
		wantErr  bool
	}{
		{name: "explicit legacy password", password: "admin1818"},
		{name: "forced minimum length", password: "12345678"},
		{name: "still too short", password: "1234567", wantErr: true},
		{name: "too long", password: strings.Repeat("a", maxPasswordBytes+1), wantErr: true},
		{name: "line break", password: "admin1818\n", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePassword(tt.password, forcedMinPasswordBytes)
			if tt.wantErr && !errors.Is(err, ErrInvalidPassword) {
				t.Fatalf("validatePassword() error = %v, want ErrInvalidPassword", err)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("validatePassword() unexpected error: %v", err)
			}
		})
	}
}
