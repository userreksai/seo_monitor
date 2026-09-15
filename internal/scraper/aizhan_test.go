package scraper

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func aizhanFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/aizhan-result.html")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseAizhanRealResponse(t *testing.T) {
	m, err := ParseAizhan(aizhanFixture(t), "www.baidu.com")
	if err != nil {
		t.Fatal(err)
	}
	for name, pair := range map[string]struct {
		got  *int16
		want int16
	}{
		"pc": {m.BaiduPCWeight, 9}, "mobile": {m.BaiduMobile, 10}, "sogou": {m.SogouWeight, 2},
		"360": {m.So360Weight, 8}, "bing": {m.BingWeight, 4}, "pr": {m.PRWeight, 9},
	} {
		if pair.got == nil || *pair.got != pair.want {
			t.Errorf("%s: %v", name, pair.got)
		}
	}
	if m.TrafficMin == nil || *m.TrafficMin != 8512812 || m.TrafficMax == nil || *m.TrafficMax != 9835950 {
		t.Fatal("traffic mismatch")
	}
	if m.BacklinkCount == nil || *m.BacklinkCount != 49020 {
		t.Fatal("backlink mismatch")
	}
	if m.DomainAgeText == nil || *m.DomainAgeText != "26年11月1日" || m.DomainAgeDays == nil || *m.DomainAgeDays != 9832 {
		t.Fatalf("age mismatch: %+v", m)
	}
	if m.ShenmaWeight != nil || m.APPPCPCrank != nil || m.SiteCategory != nil || m.RegistrantName != nil || m.RegistrantEmail != nil || m.ExpiresOn != nil {
		t.Fatal("invented unsupported data")
	}
}

func TestParseAizhanRejectsMissingAndWrongData(t *testing.T) {
	fixture := string(aizhanFixture(t))
	for name, body := range map[string]string{
		"captcha":        `<html><h1>请完成安全验证</h1></html>`,
		"home":           `<html><a href="https://baidurank.aizhan.com"><img src="/images/br/9.png"></a></html>`,
		"wrong domain":   strings.ReplaceAll(fixture, `value="www.baidu.com"`, `value="www.example.com"`),
		"missing mobile": strings.ReplaceAll(fixture, `id="baidurank_mbr"`, `id="loading"`),
		"loading pc":     strings.ReplaceAll(fixture, `/br/9.png`, `/br/loading.png`),
		"invalid rank":   strings.ReplaceAll(fixture, `/br/9.png`, `/br/11.png`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseAizhan([]byte(body), "www.baidu.com"); err == nil {
				t.Fatal("expected validation failure")
			}
		})
	}
}

func TestParseAizhanZeroAndNumericUnits(t *testing.T) {
	body := strings.ReplaceAll(string(aizhanFixture(t)), "/br/9.png", "/br/0.png")
	body = strings.ReplaceAll(body, "8,512,812 ~ 9,835,950", "1.2万 ～ 2亿")
	m, err := ParseAizhan([]byte(body), "www.baidu.com")
	if err != nil || m.BaiduPCWeight == nil || *m.BaiduPCWeight != 0 || m.TrafficMin == nil || *m.TrafficMin != 12000 || m.TrafficMax == nil || *m.TrafficMax != 200000000 {
		t.Fatalf("zero/units: %+v, %v", m, err)
	}
	for _, v := range []string{"-", "--", "加载中", "-1", "20 ~ 10", "1.2", "1 ~ -2", "1~2~3"} {
		b := strings.ReplaceAll(body, "1.2万 ～ 2亿", v)
		m, err := ParseAizhan([]byte(b), "www.baidu.com")
		if err != nil || m.TrafficMin != nil || m.TrafficMax != nil || m.TrafficText != nil {
			t.Fatalf("invalid traffic %q persisted", v)
		}
	}
}

func newTestAizhan(t *testing.T, handler http.HandlerFunc) *Aizhan {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	a, err := NewAizhan(Config{BaseURL: srv.URL, Timeout: time.Second, Retries: 2}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAizhanSourceCooldownAndCancellation(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusTooManyRequests, http.StatusOK, http.StatusFound} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var count atomic.Int32
			a := newTestAizhan(t, func(w http.ResponseWriter, r *http.Request) {
				count.Add(1)
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(code)
				_, _ = w.Write([]byte("请完成验证码"))
			})
			if _, err := a.Fetch(context.Background(), "www.baidu.com"); err == nil {
				t.Fatal("expected failure")
			}
			if count.Load() != 1 {
				t.Fatal("must not immediately retry blocked/empty pages")
			}
			if time.Until(a.cooldown) < 119*time.Second {
				t.Fatal("Retry-After ignored")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if err := a.WaitReady(ctx); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cooldown cancellation: %v", err)
			}
			if _, err := a.Fetch(ctx, "www.example.com"); err == nil {
				t.Fatal("expected cancellation")
			}
			if count.Load() != 1 {
				t.Fatal("cooldown sent another request")
			}
		})
	}
}

func TestAizhanRetryAndMetadata(t *testing.T) {
	fixture := aizhanFixture(t)
	var count atomic.Int32
	a := newTestAizhan(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/cha/www.baidu.com/" {
			t.Errorf("path: %s", r.URL.Path)
		}
		if count.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write(fixture)
	})
	m, err := a.Fetch(context.Background(), "www.baidu.com")
	if err != nil {
		t.Fatal(err)
	}
	if count.Load() != 2 || len(m.RawSHA256) != 64 || m.SourceURL != a.cfg.BaseURL+"/cha/www.baidu.com/" || m.CollectedAt.IsZero() {
		t.Fatalf("metadata: %+v", m)
	}
	if a.failures != 0 || !a.cooldown.IsZero() {
		t.Fatal("success must reset circuit")
	}
}

func TestAizhanConcurrentRequestsAreSerialAndSpaced(t *testing.T) {
	fixture := aizhanFixture(t)
	var active atomic.Int32
	var mu sync.Mutex
	var starts []time.Time
	a := newTestAizhan(t, func(w http.ResponseWriter, r *http.Request) {
		if active.Add(1) != 1 {
			t.Error("overlapping requests")
		}
		defer active.Add(-1)
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		time.Sleep(10 * time.Millisecond)
		_, _ = w.Write(fixture)
	})
	a.cfg.MinDelay, a.cfg.MaxDelay = 25*time.Millisecond, 25*time.Millisecond
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.Fetch(context.Background(), "www.baidu.com"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	for i := 1; i < len(starts); i++ {
		if starts[i].Sub(starts[i-1]) < 30*time.Millisecond {
			t.Fatal("request spacing violated")
		}
	}
}

func TestAizhanExponentialCooldown(t *testing.T) {
	a := newTestAizhan(t, func(w http.ResponseWriter, r *http.Request) {})
	for _, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour} {
		a.fail(time.Time{})
		if got := time.Until(a.cooldown); got < want-time.Second || got > want {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestAizhanBoundedRetryAndResponse(t *testing.T) {
	var count atomic.Int32
	a := newTestAizhan(t, func(w http.ResponseWriter, r *http.Request) { count.Add(1); w.WriteHeader(503) })
	if _, err := a.Fetch(context.Background(), "www.baidu.com"); err == nil || count.Load() != 2 || a.cooldown.IsZero() {
		t.Fatal("unbounded retry or missing cooldown")
	}
	b := newTestAizhan(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(strings.Repeat("x", 64))) })
	b.cfg.MaxResponseBytes = 16
	if _, err := b.Fetch(context.Background(), "www.baidu.com"); err == nil {
		t.Fatal("oversized body accepted")
	}
}

func TestAizhanRetryAfter(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, value := range []string{"120", now.Add(2 * time.Minute).Format(http.TimeFormat)} {
		if got := parseRetryAfter(value, now); !got.Equal(now.Add(2 * time.Minute)) {
			t.Fatal(value, got)
		}
	}
	for _, value := range []string{"-1", "bogus", now.Add(-time.Hour).Format(http.TimeFormat)} {
		if !parseRetryAfter(value, now).IsZero() {
			t.Fatal(value)
		}
	}
}
