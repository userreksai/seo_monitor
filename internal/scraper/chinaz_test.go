package scraper

import (
	"os"
	"testing"
	"time"
)

func TestParseSEOResult(t *testing.T) {
	body, err := os.ReadFile("testdata/seo-result.html")
	if err != nil {
		t.Fatal(err)
	}
	metric, err := Parse(body)
	if err != nil {
		t.Fatal(err)
	}
	assertInt64(t, "traffic min", metric.TrafficMin, 319)
	assertInt64(t, "traffic max", metric.TrafficMax, 509)
	if metric.TrafficText == nil || *metric.TrafficText != "319 ~ 509" {
		t.Fatalf("traffic text = %v", metric.TrafficText)
	}
	assertInt16(t, "baidu pc", metric.BaiduPCWeight, 2)
	assertInt16(t, "baidu mobile", metric.BaiduMobile, 2)
	assertInt16(t, "sogou", metric.SogouWeight, 0)
	assertInt16(t, "bing", metric.BingWeight, 0)
	assertInt16(t, "360", metric.So360Weight, 0)
	assertInt16(t, "shenma", metric.ShenmaWeight, 0)
	assertInt16(t, "pr", metric.PRWeight, 1)
	assertInt64(t, "apppc", metric.APPPCPCrank, 14586)
	assertInt64(t, "backlinks", metric.BacklinkCount, 80)
	if metric.SiteCategory == nil || *metric.SiteCategory != "医疗健康" {
		t.Fatalf("site category = %v", metric.SiteCategory)
	}
	if metric.RegistrantName == nil || *metric.RegistrantName != "godaddy.com, llc" {
		t.Fatalf("registrant = %v", metric.RegistrantName)
	}
	if metric.RegistrantEmail != nil {
		t.Fatalf("placeholder email should be nil, got %v", *metric.RegistrantEmail)
	}
	if metric.DomainAgeText == nil || *metric.DomainAgeText != "17年3个月16天" {
		t.Fatalf("domain age = %v", metric.DomainAgeText)
	}
	if metric.DomainAgeDays == nil || *metric.DomainAgeDays != 6316 {
		t.Fatalf("domain age days = %v", metric.DomainAgeDays)
	}
	wantExpiry := time.Date(2027, 3, 24, 0, 0, 0, 0, time.UTC)
	if metric.ExpiresOn == nil || !metric.ExpiresOn.Equal(wantExpiry) {
		t.Fatalf("expiry = %v", metric.ExpiresOn)
	}
}

func TestParseRejectsChallengePage(t *testing.T) {
	if _, err := Parse([]byte("<html><body>verify</body></html>")); err == nil {
		t.Fatal("expected missing result table error")
	}
}

func assertInt64(t *testing.T, name string, got *int64, want int64) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %v, want %d", name, got, want)
	}
}

func assertInt16(t *testing.T, name string, got *int16, want int16) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %v, want %d", name, got, want)
	}
}
