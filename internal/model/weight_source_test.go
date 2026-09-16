package model

import "testing"

func TestLegacyWeightProvenance(t *testing.T) {
	zero := int16(0)
	for _, tc := range []struct{ url, source string }{
		{"https://www.aizhan.com/cha/example.com/", "aizhan"},
		{"https://seo.chinaz.com/example.com", "chinaz"},
		{"https://www.aizhan.com.evil.test/example.com", ""},
		{"", ""},
	} {
		m := Metric{SourceURL: tc.url, BaiduPCWeight: &zero, BaiduMobile: &zero}
		if m.LegacyWeightSource() != tc.source {
			t.Fatalf("%s", tc.url)
		}
		m.BaiduMobile = nil
		if m.LegacyWeightSource() != "" {
			t.Fatal("supplement-only/partial metric acquired weight source")
		}
	}
}
