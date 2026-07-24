package config

import "testing"

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
