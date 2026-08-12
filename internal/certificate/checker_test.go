package certificate

import (
	"errors"
	"net/netip"
	"testing"
)

func TestIsPublicCertificateAddress(t *testing.T) {
	t.Parallel()

	tests := []struct {
		address string
		want    bool
	}{
		{address: "1.1.1.1", want: true},
		{address: "8.8.8.8", want: true},
		{address: "2606:4700:4700::1111", want: true},
		{address: "0.0.0.0", want: false},
		{address: "10.0.0.1", want: false},
		{address: "100.64.0.1", want: false},
		{address: "127.0.0.1", want: false},
		{address: "169.254.169.254", want: false},
		{address: "172.16.0.1", want: false},
		{address: "192.0.2.1", want: false},
		{address: "192.168.0.1", want: false},
		{address: "198.18.0.1", want: false},
		{address: "198.51.100.1", want: false},
		{address: "203.0.113.1", want: false},
		{address: "::1", want: false},
		{address: "::ffff:127.0.0.1", want: false},
		{address: "2001:db8::1", want: false},
		{address: "fc00::1", want: false},
		{address: "fe80::1", want: false},
	}

	for _, test := range tests {
		test := test
		t.Run(test.address, func(t *testing.T) {
			t.Parallel()
			if got := isPublicCertificateAddress(netip.MustParseAddr(test.address)); got != test.want {
				t.Fatalf("isPublicCertificateAddress(%s) = %t, want %t", test.address, got, test.want)
			}
		})
	}
}

func TestValidateCertificateDialAddress(t *testing.T) {
	t.Parallel()

	if err := validateCertificateDialAddress("1.1.1.1:443"); err != nil {
		t.Fatalf("public address rejected: %v", err)
	}
	if err := validateCertificateDialAddress("[2606:4700:4700::1111]:443"); err != nil {
		t.Fatalf("public IPv6 address rejected: %v", err)
	}
	if err := validateCertificateDialAddress("169.254.169.254:443"); !errors.Is(err, errNonPublicCertificateTarget) {
		t.Fatalf("metadata address error = %v, want %v", err, errNonPublicCertificateTarget)
	}
	if err := validateCertificateDialAddress("not-an-address"); err == nil {
		t.Fatal("malformed dial address was accepted")
	}
}
