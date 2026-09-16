package model

import "net/url"

func (m Metric) HasBaiduWeights() bool {
	return validWeight(m.BaiduPCWeight) && validWeight(m.BaiduMobile)
}

func validWeight(v *int16) bool { return v != nil && *v >= 0 && *v <= 10 }

// Legacy provenance is inferred only from the original weights response URL,
// never from supplemental_source_url or the currently configured provider.
func (m Metric) LegacyWeightSource() string {
	if !m.HasBaiduWeights() {
		return ""
	}
	u, err := url.Parse(m.SourceURL)
	if err != nil {
		return ""
	}
	switch u.Hostname() {
	case "www.aizhan.com":
		return "aizhan"
	case "seo.chinaz.com":
		return "chinaz"
	}
	return ""
}

func (m *Metric) MarkWeights(source string) {
	m.WeightSource = source
	valid := m.HasBaiduWeights()
	m.WeightValid = &valid
}
