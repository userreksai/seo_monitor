package scraper

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

func TestSupplementTimeoutOnlyFailsCurrentDomain(t *testing.T) {
	page, err := os.ReadFile("testdata/seo-result.html")
	if err != nil {
		t.Fatal(err)
	}
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if count.Add(1) == 1 {
			<-r.Context().Done()
			return
		}
		_, _ = w.Write(page)
	}))
	defer server.Close()
	s, err := NewChinazSupplement(Config{BaseURL: server.URL, Timeout: 30 * time.Millisecond}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Fetch(context.Background(), "first.com"); err == nil {
		t.Fatal("timeout accepted")
	}
	if !s.c.cooldownUntil.IsZero() {
		t.Fatal("timeout blocked other domains")
	}
	if _, err = s.Fetch(context.Background(), "next.com"); err != nil {
		t.Fatal(err)
	}
}

func TestSupplementCooldownDoesNotBlockAizhan(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) }))
	defer srv.Close()
	s, err := NewChinazSupplement(Config{BaseURL: srv.URL, Timeout: time.Second}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Fetch(context.Background(), "blocked.com"); err == nil {
		t.Fatal("429 accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := s.WaitReady(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	fixture := aizhanFixture(t)
	a := newTestAizhan(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(fixture) })
	if _, err := a.Fetch(context.Background(), "www.baidu.com"); err != nil {
		t.Fatal(err)
	}
}

func TestOrdinaryAizhanFailureDoesNotBlockNextDomain(t *testing.T) {
	for _, code := range []int{200, 500, 503} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			fixture := aizhanFixture(t)
			a := newTestAizhan(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/cha/first.com/" {
					w.WriteHeader(code)
					return
				}
				_, _ = w.Write(fixture)
			})
			if _, err := a.Fetch(context.Background(), "first.com"); err == nil {
				t.Fatal("bad response accepted")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := a.Fetch(ctx, "www.baidu.com"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAgentCapacityDoesNotOpenAizhanCircuit(t *testing.T) {
	fixture := aizhanFixture(t)
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if count.Add(1) == 1 {
			w.WriteHeader(429)
			return
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(map[string]any{"taskId": req["taskId"], "type": "seo", "target": "www.baidu.com", "status": "completed", "result": map[string]any{"available": true, "seo": map[string]any{"url": "https://www.aizhan.com/cha/www.baidu.com/", "statusCode": 200, "body": fixture}}})
	}))
	defer srv.Close()
	a, err := NewAizhan(Config{AgentURL: srv.URL, AgentToken: "test", Timeout: time.Second}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	a.client.Transport = masterResponse(400, "", nil)
	if _, err := a.Fetch(context.Background(), "first.com"); err == nil {
		t.Fatal("busy accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := a.Fetch(ctx, "www.baidu.com"); err != nil {
		t.Fatal(err)
	}
}
