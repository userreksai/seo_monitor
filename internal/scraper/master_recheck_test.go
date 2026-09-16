package scraper

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type masterTransport func(*http.Request) (*http.Response, error)

func (f masterTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func masterResponse(status int, body string, inspect func(*http.Request)) masterTransport {
	return func(r *http.Request) (*http.Response, error) {
		if inspect != nil {
			inspect(r)
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	}
}

func agentForRecheck(t *testing.T, kind string) *Aizhan {
	t.Helper()
	fixture := aizhanFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kind == "busy" {
			w.WriteHeader(429)
			return
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		body := fixture
		status := 200
		message := ""
		switch kind {
		case "400":
			status = 400
			body = nil
			message = "Aizhan returned HTTP 400"
		case "403":
			status = 403
			body = nil
			message = "Aizhan returned HTTP 403"
		case "429":
			status = 429
			body = nil
			message = "Aizhan returned HTTP 429"
		case "timeout":
			status = 0
			body = nil
			message = "context deadline exceeded"
		case "empty":
			body = nil
		case "partial":
			body = []byte(`<input id="domain" value="www.baidu.com">`)
		case "challenge":
			body = []byte(`<input id="domain" value="www.baidu.com"><p>请完成安全验证</p>`)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"taskId": req["taskId"], "type": "seo", "target": req["target"], "status": "completed", "result": map[string]any{"available": message == "", "seo": map[string]any{"url": "https://www.aizhan.com/cha/www.baidu.com/", "statusCode": status, "body": body, "error": message}}})
	}))
	t.Cleanup(srv.Close)
	a, err := NewAizhan(Config{AgentURL: srv.URL, AgentToken: "secret-agent-token", Timeout: time.Second, Retries: 1, UserAgent: "seo-test"}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestMasterRecheckRecoversOrdinaryAgentFailures(t *testing.T) {
	for _, kind := range []string{"400", "timeout", "empty", "partial", "busy"} {
		t.Run(kind, func(t *testing.T) {
			a := agentForRecheck(t, kind)
			var calls atomic.Int32
			a.client.Transport = masterResponse(200, string(aizhanFixture(t)), func(r *http.Request) {
				calls.Add(1)
				if r.URL.String() != "https://www.aizhan.com/cha/www.baidu.com/" || r.Header.Get("Authorization") != "" || r.Header.Get("User-Agent") != "seo-test" {
					t.Error("master request URL/headers incorrect")
				}
			})
			m, err := a.Fetch(context.Background(), "www.baidu.com")
			if err != nil {
				t.Fatal(err)
			}
			if calls.Load() != 1 || m.CollectionRoute != "direct:master-recheck" || len(m.RawSHA256) != 64 || m.BaiduPCWeight == nil || !a.cooldown.IsZero() {
				t.Fatalf("fallback result invalid: %+v", m)
			}
		})
	}
}

func TestMasterRecheckNotCalledForSuccessOrExplicitBlocking(t *testing.T) {
	for _, kind := range []string{"success", "403", "429", "challenge"} {
		t.Run(kind, func(t *testing.T) {
			a := agentForRecheck(t, kind)
			a.client.Transport = masterResponse(200, "", func(*http.Request) { t.Error("unexpected master request") })
			_, err := a.Fetch(context.Background(), "www.baidu.com")
			if kind == "success" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || a.cooldown.IsZero() {
				t.Fatal("source cooldown lost")
			}
		})
	}
}

func TestMasterRecheckFailureIsBoundedAndKeepsBothErrors(t *testing.T) {
	for _, status := range []int{400, 503, 429} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			a := agentForRecheck(t, "400")
			a.cfg.Retries = 2
			var calls atomic.Int32
			a.client.Transport = masterResponse(status, "", func(*http.Request) { calls.Add(1) })
			_, err := a.Fetch(context.Background(), "www.baidu.com")
			if err == nil || calls.Load() != 1 || !strings.Contains(err.Error(), "Agent failed:") || !strings.Contains(err.Error(), "master recheck failed:") {
				t.Fatal(calls.Load(), err)
			}
			if (status == 429) == a.cooldown.IsZero() {
				t.Fatal("wrong cooldown classification")
			}
		})
	}
}

func TestMasterRecheckWaitsForGapAndCancels(t *testing.T) {
	a := agentForRecheck(t, "400")
	a.cfg.MinDelay = time.Minute
	a.cfg.MaxDelay = time.Minute
	var calls atomic.Int32
	a.client.Transport = masterResponse(200, string(aizhanFixture(t)), func(*http.Request) { calls.Add(1) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := a.Fetch(ctx, "www.baidu.com"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("recheck ignored normal gap or cancellation")
	}
}

func TestMasterRecheckRequiresValidWeightsAndDomain(t *testing.T) {
	for _, body := range []string{"", `<input id="domain" value="www.baidu.com">`, string(aizhanFixture(t))} {
		a := agentForRecheck(t, "400")
		if strings.Contains(body, "baidurank_br") {
			body = strings.ReplaceAll(body, "www.baidu.com", "wrong.com")
		}
		a.client.Transport = masterResponse(200, body, nil)
		m, err := a.Fetch(context.Background(), "www.baidu.com")
		if err == nil || m.BaiduPCWeight != nil || !strings.Contains(err.Error(), "master recheck failed") {
			t.Fatal("invalid master page accepted", err)
		}
	}
}

func TestMasterRecheckHasNormalRequestGap(t *testing.T) {
	a := agentForRecheck(t, "400")
	a.cfg.MinDelay = 30 * time.Millisecond
	a.cfg.MaxDelay = 30 * time.Millisecond
	started := time.Now()
	a.client.Transport = masterResponse(200, string(aizhanFixture(t)), func(*http.Request) {
		if time.Since(started) < 30*time.Millisecond {
			t.Error("master recheck bypassed pacing")
		}
	})
	if _, err := a.Fetch(context.Background(), "www.baidu.com"); err != nil {
		t.Fatal(err)
	}
}
