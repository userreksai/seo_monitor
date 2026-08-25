package scraper

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"seo-monitor/internal/model"
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

func TestFetchFallsBackToDataEndpointsForEmptyResultTable(t *testing.T) {
	dataServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Rank.ashx":
			_, _ = w.Write([]byte(`seoMonitorCallback({"StateCode":1,"Result":{"baiduPc":{"rank":0,"uv_min":0,"uv_max":0},"baiduMobile":{"rank":0,"uv_min":0,"uv_max":0},"sogouPc":{"rank":0,"uv_min":0,"uv_max":0},"bing":{"rank":0,"uv_min":0,"uv_max":0},"haosouPc":{"rank":0,"uv_min":0,"uv_max":0},"shenma":{"rank":0,"uv_min":0,"uv_max":0}}})`))
		case "/SiteAPPAndPC.ashx", "/GetTopRanked.ashx":
			_, _ = w.Write([]byte(`seoMonitorCallback({"StateCode":0,"Result":null})`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer dataServer.Close()

	pageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><table class="_chinaz-seo-newt"><tbody></tbody></table><script>var enkey = "public-key";</script></body></html>`))
	}))
	defer pageServer.Close()

	source, err := NewChinaz(Config{
		BaseURL:          pageServer.URL,
		DataBaseURL:      dataServer.URL,
		UserAgent:        "seo-monitor test",
		Timeout:          time.Second,
		Retries:          1,
		MaxResponseBytes: 1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}

	metric, err := source.Fetch(context.Background(), "77cn.com.cn")
	if err != nil {
		t.Fatal(err)
	}
	assertInt64(t, "traffic min", metric.TrafficMin, 0)
	assertInt64(t, "traffic max", metric.TrafficMax, 0)
	if metric.TrafficText == nil || *metric.TrafficText != "0 ~ 0" {
		t.Fatalf("traffic text = %v", metric.TrafficText)
	}
	assertInt16(t, "baidu pc", metric.BaiduPCWeight, 0)
	assertInt16(t, "baidu mobile", metric.BaiduMobile, 0)
	assertInt16(t, "sogou", metric.SogouWeight, 0)
	assertInt16(t, "bing", metric.BingWeight, 0)
	assertInt16(t, "360", metric.So360Weight, 0)
	assertInt16(t, "shenma", metric.ShenmaWeight, 0)
	if metric.PRWeight != nil || metric.SiteCategory != nil || metric.DomainAgeText != nil {
		t.Fatalf("unavailable fields must stay nil: %+v", metric)
	}
}

func TestFetchRejectsEmptyResultWhenRankFallbackIsLimited(t *testing.T) {
	dataServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "limited", http.StatusTooManyRequests)
	}))
	defer dataServer.Close()

	pageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><table class="_chinaz-seo-newt"><tbody></tbody></table><script>var enkey = "public-key";</script></body></html>`))
	}))
	defer pageServer.Close()

	source, err := NewChinaz(Config{
		BaseURL:          pageServer.URL,
		DataBaseURL:      dataServer.URL,
		UserAgent:        "seo-monitor test",
		Timeout:          time.Second,
		Retries:          1,
		MaxResponseBytes: 1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Fetch(context.Background(), "77cn.com.cn"); err == nil {
		t.Fatal("expected empty limited result to be retried by the collection queue")
	}
}

func TestMergeRankResponse(t *testing.T) {
	body := []byte(`seoMonitorCallback({"StateCode":1,"Result":{"baiduPc":{"rank":2,"uv_min":142,"uv_max":226},"baiduMobile":{"rank":4,"uv_min":1744,"uv_max":2786},"sogouPc":{"rank":0,"uv_min":0,"uv_max":0},"bing":{"rank":0,"uv_min":0,"uv_max":0},"haosouPc":{"rank":0,"uv_min":0,"uv_max":0},"shenma":{"rank":0,"uv_min":0,"uv_max":0}}})`)
	var metric model.Metric
	if err := mergeRankResponse(body, &metric); err != nil {
		t.Fatal(err)
	}
	assertInt64(t, "traffic min", metric.TrafficMin, 1886)
	assertInt64(t, "traffic max", metric.TrafficMax, 3012)
	if metric.TrafficText == nil || *metric.TrafficText != "1,886 ~ 3,012" {
		t.Fatalf("traffic text = %v", metric.TrafficText)
	}
	assertInt16(t, "baidu pc", metric.BaiduPCWeight, 2)
	assertInt16(t, "baidu mobile", metric.BaiduMobile, 4)
	assertInt16(t, "sogou", metric.SogouWeight, 0)
}

func TestMergeAPPPCResponse(t *testing.T) {
	body := []byte(`callback({"StateCode":1,"Result":{"WeekRank":"14586","Pr":"1","ResLink":"{\"link\":80}"}})`)
	var metric model.Metric
	if err := mergeAPPPCResponse(body, &metric); err != nil {
		t.Fatal(err)
	}
	assertInt64(t, "APPPC rank", metric.APPPCPCrank, 14586)
	assertInt16(t, "PR", metric.PRWeight, 1)
	assertInt64(t, "backlinks", metric.BacklinkCount, 80)
}

func TestMergeCategoryResponse(t *testing.T) {
	body := []byte(`callback({"StateCode":1,"Message":"成功","Result":"常用查询"})`)
	var metric model.Metric
	if err := mergeCategoryResponse(body, &metric); err != nil {
		t.Fatal(err)
	}
	if metric.SiteCategory == nil || *metric.SiteCategory != "常用查询" {
		t.Fatalf("site category = %v", metric.SiteCategory)
	}
}

func TestExtractSecretKey(t *testing.T) {
	key, err := extractSecretKey([]byte(`<script>var enkey = 'public-key';</script>`))
	if err != nil {
		t.Fatal(err)
	}
	if key != "public-key" {
		t.Fatalf("key = %q", key)
	}
}

func TestLiveFetch(t *testing.T) {
	domain := os.Getenv("CHINAZ_LIVE_TEST_DOMAIN")
	if domain == "" {
		t.Skip("set CHINAZ_LIVE_TEST_DOMAIN to run the upstream integration test")
	}
	source, err := NewChinaz(Config{
		BaseURL:          "https://seo.chinaz.com",
		DataBaseURL:      "https://othertool.chinaz.com",
		UserAgent:        "Mozilla/5.0 seo-monitor integration test",
		Timeout:          30 * time.Second,
		Retries:          1,
		MaxResponseBytes: 3 * 1024 * 1024,
	})
	if err != nil {
		t.Fatal(err)
	}
	metric, err := source.Fetch(context.Background(), domain)
	if err != nil {
		t.Fatal(err)
	}
	if metric.BaiduPCWeight == nil || metric.BaiduMobile == nil || metric.TrafficText == nil {
		t.Fatalf("incomplete live metric: %+v", metric)
	}
	if domain == "xingzuo360.cn" && (metric.SiteCategory == nil || *metric.SiteCategory != "常用查询") {
		t.Fatalf("live site category = %v", metric.SiteCategory)
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
