package domainfile

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLoadSupportedFormats(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"array", `["123.com", "HTTPS://Example.com/path", "123.com"]`},
		{"object", `{"domains":["123.com","example.com"]}`},
		{"relaxed", "{\n123.com,\nexample.com,\n}"},
	}
	want := []string{"123.com", "example.com"}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			filename := filepath.Join(t.TempDir(), "domains.json")
			if err := os.WriteFile(filename, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := Load(filename)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Load() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestLoadRejectsInvalidDomain(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "domains.json")
	if err := os.WriteFile(filename, []byte(`["valid.com", "bad_domain"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filename); err == nil {
		t.Fatal("expected invalid domain error")
	}
}
