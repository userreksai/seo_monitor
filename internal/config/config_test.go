package config

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	_ = os.Setenv("DEFAULT_ADMIN_PASSWORD", "test-only-password")
	os.Exit(m.Run())
}

func TestCertificateRetentionDays(t *testing.T) {
	t.Setenv("CERTIFICATE_RETENTION_DAYS", "7")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CertificateRetentionDays != 7 {
		t.Fatalf("CertificateRetentionDays = %d, want 7", cfg.CertificateRetentionDays)
	}
}

func TestCertificateRetentionDaysRejectsZero(t *testing.T) {
	t.Setenv("CERTIFICATE_RETENTION_DAYS", "0")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid certificate retention error")
	}
}

func TestCertificateDomainsFile(t *testing.T) {
	t.Setenv("CERTIFICATE_DOMAINS_FILE", "certificates.json")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CertificateDomainsFile != "certificates.json" {
		t.Fatalf("CertificateDomainsFile = %q, want certificates.json", cfg.CertificateDomainsFile)
	}
}

func TestAuthenticationProtectionDefaults(t *testing.T) {
	t.Setenv("AUTH_LOGIN_IP_MAX_FAILURES", "")
	t.Setenv("AUTH_LOGIN_PAIR_MAX_FAILURES", "")
	t.Setenv("AUTH_LOGIN_FAILURE_WINDOW", "")
	t.Setenv("AUTH_LOGIN_LOCKOUT", "")
	t.Setenv("AUTH_TRUSTED_PROXY_CIDRS", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AuthLoginIPMaxFailures != 10 || cfg.AuthLoginPairMaxFailures != 5 {
		t.Fatalf("unexpected login limits: IP=%d pair=%d", cfg.AuthLoginIPMaxFailures, cfg.AuthLoginPairMaxFailures)
	}
	if cfg.AuthLoginFailureWindow != 15*time.Minute || cfg.AuthLoginLockout != 15*time.Minute {
		t.Fatalf("unexpected login durations: window=%s lockout=%s", cfg.AuthLoginFailureWindow, cfg.AuthLoginLockout)
	}
	if len(cfg.AuthTrustedProxyCIDRs) != 2 {
		t.Fatalf("trusted proxy count = %d, want 2", len(cfg.AuthTrustedProxyCIDRs))
	}
}

func TestAuthenticationProtectionValidation(t *testing.T) {
	t.Setenv("AUTH_LOGIN_IP_MAX_FAILURES", "4")
	t.Setenv("AUTH_LOGIN_PAIR_MAX_FAILURES", "5")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "AUTH_LOGIN_PAIR_MAX_FAILURES") {
		t.Fatalf("expected pair limit validation error, got %v", err)
	}
}

func TestTrustedProxyCIDRValidation(t *testing.T) {
	t.Setenv("AUTH_TRUSTED_PROXY_CIDRS", "not-a-network")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "AUTH_TRUSTED_PROXY_CIDRS") {
		t.Fatalf("expected trusted proxy validation error, got %v", err)
	}
}

func TestDefaultPasswordRejectsBcryptOverflow(t *testing.T) {
	t.Setenv("DEFAULT_ADMIN_PASSWORD", strings.Repeat("a", 73))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "72") {
		t.Fatalf("expected bcrypt length error, got %v", err)
	}
}

func TestDefaultPasswordRequiresTwelveBytes(t *testing.T) {
	t.Setenv("DEFAULT_ADMIN_PASSWORD", "short-pass")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "12") {
		t.Fatalf("expected minimum password length error, got %v", err)
	}
}

func TestAPITokenRequiresThirtyTwoBytesWhenEnabled(t *testing.T) {
	t.Setenv("API_TOKEN", "short-token")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "API_TOKEN") {
		t.Fatalf("expected API token length error, got %v", err)
	}

	t.Setenv("API_TOKEN", "")
	if _, err := Load(); err != nil {
		t.Fatalf("disabled API token should be accepted: %v", err)
	}
}

func TestCollectionRetryDelays(t *testing.T) {
	t.Setenv("COLLECTION_RETRY_DELAYS", "10m, 30m, 1h")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{10 * time.Minute, 30 * time.Minute, time.Hour}
	if !reflect.DeepEqual(cfg.CollectionRetryDelays, want) {
		t.Fatalf("CollectionRetryDelays = %v, want %v", cfg.CollectionRetryDelays, want)
	}
}

func TestCollectionRetryDelaysRejectsInvalidValue(t *testing.T) {
	t.Setenv("COLLECTION_RETRY_DELAYS", "10m,never")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid collection retry delay error")
	}
}
