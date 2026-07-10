package domainutil

import "testing"

func TestNormalize(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{" QIYESHANGPU.COM. ", "qiyeshangpu.com"},
		{"https://www.Example.com/path", "www.example.com"},
		{"例子.测试", "xn--fsqu00a.xn--0zwm56d"},
	}
	for _, test := range tests {
		got, err := Normalize(test.input)
		if err != nil {
			t.Fatalf("Normalize(%q): %v", test.input, err)
		}
		if got != test.want {
			t.Fatalf("Normalize(%q) = %q, want %q", test.input, got, test.want)
		}
	}
}

func TestNormalizeRejectsInvalid(t *testing.T) {
	for _, input := range []string{"", "localhost", "127.0.0.1", "bad_domain.com", "-bad.com"} {
		if _, err := Normalize(input); err == nil {
			t.Errorf("Normalize(%q) unexpectedly succeeded", input)
		}
	}
}
