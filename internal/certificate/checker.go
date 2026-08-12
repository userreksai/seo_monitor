package certificate

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"

	"seo-monitor/internal/model"
)

type Checker interface {
	Check(context.Context, string) (model.Certificate, error)
}

type TLSChecker struct {
	timeout time.Duration
}

var errNonPublicCertificateTarget = errors.New("certificate target resolved to a non-public address")

// These ranges are not valid public certificate-monitoring targets. In
// addition to the usual private and loopback ranges, block shared, benchmark
// and documentation networks so a compromised administrator account cannot
// turn the checker into an internal network probe.
var nonPublicCertificatePrefixes = mustCertificatePrefixes(
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"192.168.0.0/16",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"224.0.0.0/4",
	"240.0.0.0/4",
	"::/128",
	"::1/128",
	"64:ff9b::/96",
	"64:ff9b:1::/48",
	"100::/64",
	"2001:2::/48",
	"2001:db8::/32",
	"2002::/16",
	"fc00::/7",
	"fe80::/10",
	"ff00::/8",
)

func NewTLSChecker(timeout time.Duration) *TLSChecker {
	return &TLSChecker{timeout: timeout}
}

func (c *TLSChecker) Check(ctx context.Context, domain string) (model.Certificate, error) {
	checkedAt := time.Now().UTC()
	result := model.Certificate{Domain: domain, CheckedAt: checkedAt, CheckSource: "master"}
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{
			Timeout: c.timeout,
			// ControlContext runs after name resolution for every candidate IP.
			// Validating the address at this point prevents DNS rebinding while
			// retaining the standard dialer's IPv4/IPv6 fallback behavior.
			ControlContext: func(_ context.Context, _, address string, _ syscall.RawConn) error {
				return validateCertificateDialAddress(address)
			},
		},
		Config: &tls.Config{
			ServerName: domain,
			// Monitoring must still read expired, self-signed, or hostname-mismatched
			// certificates. Hostname validity is evaluated and stored separately.
			InsecureSkipVerify: true, //nolint:gosec
		},
	}
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(domain, "443"))
	if err != nil {
		return result, fmt.Errorf("TLS connection failed: %w", err)
	}
	defer connection.Close()
	result.ResolvedAddr = connection.RemoteAddr().String()

	tlsConnection, ok := connection.(*tls.Conn)
	if !ok {
		return result, fmt.Errorf("unexpected TLS connection type")
	}
	state := tlsConnection.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		return result, fmt.Errorf("server returned no certificate")
	}

	leaf := state.PeerCertificates[0]
	validFrom := leaf.NotBefore.UTC()
	expiresAt := leaf.NotAfter.UTC()
	issuer := leaf.Issuer.CommonName
	if issuer == "" {
		issuer = leaf.Issuer.String()
	}
	subject := leaf.Subject.CommonName
	if subject == "" {
		subject = leaf.Subject.String()
	}
	result.Issuer = issuer
	result.Subject = subject
	result.SerialNumber = leaf.SerialNumber.Text(16)
	result.DNSNames = append([]string(nil), leaf.DNSNames...)
	result.ValidFrom = &validFrom
	result.ExpiresAt = &expiresAt
	result.HostnameValid = leaf.VerifyHostname(domain) == nil
	return result, nil
}

func validateCertificateDialAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("validate certificate target address: %w", err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !isPublicCertificateAddress(ip) {
		return errNonPublicCertificateTarget
	}
	return nil
}

func isPublicCertificateAddress(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsValid() || !address.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range nonPublicCertificatePrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func mustCertificatePrefixes(values ...string) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefixes = append(prefixes, netip.MustParsePrefix(value))
	}
	return prefixes
}
