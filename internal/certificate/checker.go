package certificate

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"seo-monitor/internal/model"
)

type Checker interface {
	Check(context.Context, string) (model.Certificate, error)
}

type TLSChecker struct {
	timeout time.Duration
}

func NewTLSChecker(timeout time.Duration) *TLSChecker {
	return &TLSChecker{timeout: timeout}
}

func (c *TLSChecker) Check(ctx context.Context, domain string) (model.Certificate, error) {
	checkedAt := time.Now().UTC()
	result := model.Certificate{Domain: domain, CheckedAt: checkedAt, CheckSource: "master"}
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: c.timeout},
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
