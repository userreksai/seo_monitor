package scraper

import (
	"context"
	"net/http"
	"net/http/httptest"
	"seo-monitor/internal/model"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const chinazZero = `<table class="_chinaz-seo-newt"><tr><td class="baidupcrank"><img data-rank="0"></td><td class="baidumobilerank"><img data-rank="0"></td></tr></table>`

func fallbackChinaz(t *testing.T, handler http.HandlerFunc) *ChinazSupplement {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c, err := NewChinazSupplement(Config{BaseURL: server.URL, DataBaseURL: server.URL, Timeout: time.Second}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestWeightFallbackAfterAgentAndMasterFailure(t *testing.T) {
	a := agentForRecheck(t, "400")
	var master, chinaz atomic.Int32
	a.client.Transport = masterResponse(200, "", func(*http.Request) { master.Add(1) })
	c := fallbackChinaz(t, func(w http.ResponseWriter, r *http.Request) { chinaz.Add(1); w.Write([]byte(chinazZero)) })
	m, err := NewWeightFallback(a, c).Fetch(context.Background(), "www.baidu.com")
	if err != nil {
		t.Fatal(err)
	}
	if master.Load() != 1 || chinaz.Load() != 1 || m.WeightSource != "chinaz" || m.WeightValid == nil || !*m.WeightValid || *m.BaiduPCWeight != 0 || m.CollectionRoute != "direct:chinaz-fallback" {
		t.Fatalf("wrong fallback: %+v", m)
	}
}

func TestWeightFallbackPrimarySuccessAndIndependentCooldown(t *testing.T) {
	a := agentForRecheck(t, "ok")
	c := fallbackChinaz(t, func(w http.ResponseWriter, r *http.Request) { t.Error("Chinaz must not be requested") })
	c.c.cooldownUntil = time.Now().Add(time.Hour)
	m, err := NewWeightFallback(a, c).Fetch(context.Background(), "www.baidu.com")
	if err != nil || m.WeightSource != "aizhan" || m.WeightValid == nil || !*m.WeightValid {
		t.Fatalf("%+v %v", m, err)
	}
}

func TestWeightFallbackDuringAizhanCooldown(t *testing.T) {
	a := agentForRecheck(t, "ok")
	a.cooldown = time.Now().Add(time.Hour)
	c := fallbackChinaz(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(chinazZero)) })
	m, err := NewWeightFallback(a, c).Fetch(context.Background(), "www.baidu.com")
	if err != nil || m.WeightSource != "chinaz" {
		t.Fatalf("%+v %v", m, err)
	}
}

func TestWeightFallbackMissingWeightsIsFailure(t *testing.T) {
	a := agentForRecheck(t, "400")
	a.client.Transport = masterResponse(200, "", nil)
	c := fallbackChinaz(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Rank.ashx" {
			w.Write([]byte(`{"StateCode":1,"Result":{}}`))
			return
		}
		w.Write([]byte(`<script>var enkey='key'</script><table class="_chinaz-seo-newt"></table>`))
	})
	m, err := NewWeightFallback(a, c).Fetch(context.Background(), "www.baidu.com")
	if err == nil || m.WeightSource != "" || m.BaiduPCWeight != nil {
		t.Fatalf("fabricated weights: %+v %v", m, err)
	}
}

func TestRankResponseMissingOptionalWeightsAndRealZero(t *testing.T) {
	var m model.Metric
	err := mergeRankResponse([]byte(`{"StateCode":1,"Result":{"baiduPc":{"rank":0},"baiduMobile":{"rank":0}}}`), &m)
	if err != nil || !m.HasBaiduWeights() || m.SogouWeight != nil {
		t.Fatalf("%+v %v", m, err)
	}
	m = model.Metric{}
	if mergeRankResponse([]byte(`{"StateCode":1,"Result":{"baiduPc":{"rank":0}}}`), &m) == nil || m.BaiduMobile != nil {
		t.Fatal("missing mobile fabricated")
	}
}

func TestChinazSharedCircuitAndConcurrency(t *testing.T) {
	var active, maxActive, hits atomic.Int32
	c := fallbackChinaz(t, func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		hits.Add(1)
		for old := maxActive.Load(); n > old; old = maxActive.Load() {
			if maxActive.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		w.Write([]byte(chinazZero))
	})
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.c.readShared(context.Background(), c.c.baseURL, ""); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if maxActive.Load() != 1 || hits.Load() != 5 {
		t.Fatal("requests overlapped")
	}
	c.c.client.Transport = masterResponse(429, "", nil)
	if _, err := c.c.readShared(context.Background(), c.c.baseURL, ""); !sourceBlocked(err) {
		t.Fatal(err)
	}
	c.c.client.Transport = masterResponse(200, chinazZero, func(*http.Request) { t.Error("cooldown ignored") })
	if _, err := c.c.readShared(context.Background(), c.c.baseURL, ""); err == nil {
		t.Fatal("cooldown ignored")
	}
}

func TestWeightFallbackDynamicResponseReplacesPagePlaceholders(t *testing.T) {
	for _, tc := range []struct {
		body    string
		success bool
	}{
		{`{"StateCode":1,"Result":{"baiduPc":{"rank":4},"baiduMobile":{"rank":2}}}`, true},
		{`{"StateCode":1,"Result":{}}`, false},
	} {
		c := fallbackChinaz(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/Rank.ashx" {
				w.Write([]byte(tc.body))
				return
			}
			w.Write([]byte(`<script>var enkey='key'</script>` + chinazZero))
		})
		m, err := c.c.fetchWeights(context.Background(), "example.com")
		if tc.success {
			if err != nil || m.BaiduPCWeight == nil || *m.BaiduPCWeight != 4 {
				t.Fatalf("%+v %v", m, err)
			}
		} else if err == nil {
			t.Fatal("page zero placeholders accepted")
		}
	}
}

func TestWeightFallbackBothCooldownsRespectCancellation(t *testing.T) {
	a := agentForRecheck(t, "ok")
	a.cooldown = time.Now().Add(time.Hour)
	c := fallbackChinaz(t, func(w http.ResponseWriter, r *http.Request) { t.Error("both circuits closed") })
	c.c.cooldownUntil = time.Now().Add(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if NewWeightFallback(a, c).WaitReady(ctx) == nil {
		t.Fatal("cancellation ignored")
	}
}
