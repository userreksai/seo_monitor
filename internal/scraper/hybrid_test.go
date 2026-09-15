package scraper

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

func testHybrid(t *testing.T, handler http.HandlerFunc) *Hybrid {
	t.Helper()
	primaryBody := aizhanFixture(t)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(primaryBody) }))
	secondary := httptest.NewServer(handler)
	t.Cleanup(primary.Close)
	t.Cleanup(secondary.Close)
	h, err := NewHybrid(Config{BaseURL: primary.URL, Timeout: time.Second, Retries: 1}, time.Minute,
		Config{BaseURL: secondary.URL, DataBaseURL: secondary.URL, Timeout: time.Second, Retries: 1}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHybridOnlyCopiesRequestedFields(t *testing.T) {
	page, err := os.ReadFile("testdata/seo-result.html")
	if err != nil {
		t.Fatal(err)
	}
	page = []byte(strings.ReplaceAll(string(page), "注册人邮箱： <i>-</i>", "注册人邮箱： <i>owner@example.com</i>"))
	var requests atomic.Int32
	h := testHybrid(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/www.baidu.com" {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		_, _ = w.Write(page)
	})
	m, err := h.Fetch(context.Background(), "www.baidu.com")
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatal("page contained fields; extra endpoints should not be queried")
	}
	assertInt64(t, "APPPC", m.APPPCPCrank, 14586)
	if m.SiteCategory == nil || *m.SiteCategory != "医疗健康" || m.RegistrantName == nil || *m.RegistrantName != "godaddy.com, llc" || m.RegistrantEmail == nil || *m.RegistrantEmail != "owner@example.com" || m.ExpiresOn == nil || m.ExpiresOn.Format("2006-01-02") != "2027-03-24" {
		t.Fatalf("supplement missing: %+v", m)
	}
	assertInt16(t, "Baidu", m.BaiduPCWeight, 9)
	assertInt16(t, "PR", m.PRWeight, 9)
	assertInt64(t, "backlinks", m.BacklinkCount, 49020)
	assertInt64(t, "traffic", m.TrafficMin, 8512812)
	if m.ShenmaWeight != nil || m.DomainAgeText == nil || *m.DomainAgeText != "26年11月1日" {
		t.Fatal("overwrote Aizhan fields")
	}
	if m.SourceURL != h.aizhan.cfg.BaseURL+"/cha/www.baidu.com/" || m.SupplementalSourceURL != h.chinaz.baseURL+"/www.baidu.com" || len(m.SupplementalRawSHA256) != 64 || m.SupplementalCollectedAt == nil {
		t.Fatal("provenance missing")
	}
	raw, err := bson.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var doc bson.M
	if err = bson.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["supplemental_source_url"] != m.SupplementalSourceURL {
		t.Fatal("MongoDB source lost")
	}
}

func TestHybridSupplementDynamicFieldsWithoutChinazWeights(t *testing.T) {
	var requests atomic.Int32
	h := testHybrid(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.URL.Path {
		case "/www.baidu.com":
			_, _ = w.Write([]byte(`<table class="_chinaz-seo-newt"></table><script>var enkey="public-key";</script>`))
		case "/SiteAPPAndPC.ashx":
			_, _ = w.Write([]byte(`callback({"StateCode":1,"Result":{"WeekRank":"123","Pr":"1","ResLink":"{\"link\":1}"}})`))
		case "/GetTopRanked.ashx":
			if r.URL.Query().Get("action") != "GetSiteCategory" || r.Header.Get("Referer") == "" {
				t.Error("missing dynamic request parameters")
			}
			_, _ = w.Write([]byte(`callback({"StateCode":1,"Result":"查询工具"})`))
		default:
			t.Errorf("must not query weights: %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	m, err := h.Fetch(context.Background(), "www.baidu.com")
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 3 {
		t.Fatal("expected page plus two supplemental requests")
	}
	assertInt64(t, "APPPC", m.APPPCPCrank, 123)
	assertInt16(t, "PR", m.PRWeight, 9)
	assertInt64(t, "backlinks", m.BacklinkCount, 49020)
	if m.SiteCategory == nil || *m.SiteCategory != "查询工具" {
		t.Fatal("category missing")
	}
}

func TestHybridNoDataIsDifferentFromFailedRequest(t *testing.T) {
	for _, payload := range []string{`callback({"StateCode":0,"Result":null})`, `callback({"error":"captcha"})`, `<html>验证码</html>`} {
		t.Run(payload, func(t *testing.T) {
			h := testHybrid(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/www.baidu.com" {
					_, _ = w.Write([]byte(`<table class="_chinaz-seo-newt"></table><script>var enkey="public-key";</script>`))
					return
				}
				_, _ = w.Write([]byte(payload))
			})
			m, err := h.Fetch(context.Background(), "www.baidu.com")
			if strings.Contains(payload, `"StateCode":0`) {
				if err != nil || m.APPPCPCrank != nil || m.SiteCategory != nil || m.BaiduPCWeight == nil {
					t.Fatalf("explicit no data: %+v %v", m, err)
				}
			} else if err == nil || m.BaiduPCWeight != nil {
				t.Fatal("failure must retry job without writing partial result")
			}
		})
	}
}

func TestHybridChinazCooldownHonorsRetryAfter(t *testing.T) {
	var requests atomic.Int32
	h := testHybrid(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(429)
	})
	if _, err := h.Fetch(context.Background(), "www.baidu.com"); err == nil {
		t.Fatal("expected error")
	}
	if requests.Load() != 1 || time.Until(h.chinaz.cooldownUntil) < 119*time.Second || !h.aizhan.cooldown.IsZero() {
		t.Fatal("source cooldown not independent")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := h.WaitReady(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatal("cooldown made requests")
	}
}
