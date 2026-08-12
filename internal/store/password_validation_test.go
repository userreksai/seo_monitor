package store

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateNewPassword(t *testing.T) {
	t.Parallel()

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

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := validateNewPassword(test.password)
			if test.wantErr && !errors.Is(err, ErrInvalidPassword) {
				t.Fatalf("validateNewPassword() error = %v, want ErrInvalidPassword", err)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("validateNewPassword() unexpected error: %v", err)
			}
		})
	}
}
