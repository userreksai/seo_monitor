package scraper

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAizhanViaAgent(t *testing.T) {
	fixture := aizhanFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/tasks" || r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("Agent request path/auth mismatch")
		}
		var req struct {
			TaskID string `json:"taskId"`
			Type   string `json:"type"`
			Target string `json:"target"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req.Type != "seo" || req.Target != "www.baidu.com" {
			t.Errorf("bad task %+v", req)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"taskId": req.TaskID, "type": "seo", "target": req.Target, "status": "completed", "result": map[string]any{"available": true, "seo": map[string]any{"url": "https://www.aizhan.com/cha/www.baidu.com/", "statusCode": 200, "body": fixture}}})
	}))
	defer server.Close()
	a, err := NewAizhan(Config{BaseURL: "https://www.aizhan.com", AgentURL: server.URL, AgentToken: "test-token", Timeout: time.Second}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	m, err := a.Fetch(context.Background(), "www.baidu.com")
	if err != nil {
		t.Fatal(err)
	}
	assertInt16(t, "pc", m.BaiduPCWeight, 9)
	if m.CollectionRoute != "agent:"+server.URL+"/api/v1/tasks" || m.SourceURL != "https://www.aizhan.com/cha/www.baidu.com/" || len(m.RawSHA256) != 64 {
		t.Fatalf("metadata %+v", m)
	}
}

func TestAizhanAgentValidationAndCooldown(t *testing.T) {
	fixture := aizhanFixture(t)
	for _, kind := range []string{"empty", "wrong_id", "wrong_type", "wrong_domain", "wrong_url", "oversized", "old_agent", "cooldown"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req map[string]any
				_ = json.NewDecoder(r.Body).Decode(&req)
				if kind == "old_agent" {
					w.WriteHeader(400)
					return
				}
				seo := map[string]any{"url": "https://www.aizhan.com/cha/www.baidu.com/", "statusCode": 200, "body": fixture}
				result := map[string]any{"available": true, "seo": seo}
				envelope := map[string]any{"taskId": req["taskId"], "type": "seo", "target": "www.baidu.com", "status": "completed", "result": result}
				switch kind {
				case "empty":
					seo["body"] = []byte{}
				case "wrong_id":
					envelope["taskId"] = "wrong"
				case "wrong_type":
					envelope["type"] = "title"
				case "wrong_domain":
					envelope["target"] = "other.com"
				case "wrong_url":
					seo["url"] = "https://other.com"
				case "oversized":
					seo["body"] = make([]byte, 5000)
				case "cooldown":
					result["available"] = false
					seo["error"] = "cooling down"
					seo["retryAt"] = time.Now().UTC().Add(2 * time.Hour)
				}
				_ = json.NewEncoder(w).Encode(envelope)
			}))
			defer server.Close()
			maxBody := int64(4096)
			if kind != "oversized" {
				maxBody = 3 << 20
			}
			a, err := NewAizhan(Config{BaseURL: "https://www.aizhan.com", AgentURL: server.URL, AgentToken: "test-token", Timeout: time.Second, MaxResponseBytes: maxBody}, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = a.Fetch(context.Background(), "www.baidu.com"); err == nil {
				t.Fatal("invalid response accepted")
			}
			if a.cooldown.IsZero() {
				t.Fatal("missing cooldown")
			}
			if kind == "cooldown" && time.Until(a.cooldown) < 119*time.Minute {
				t.Fatal("Agent retryAt lost")
			}
		})
	}
}

func TestAizhanEmptyDirectResponse(t *testing.T) {
	a := newTestAizhan(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	_, err := a.Fetch(context.Background(), "itgirls.cn")
	if err == nil || !strings.Contains(err.Error(), "empty body") {
		t.Fatalf("want actionable empty-body error: %v", err)
	}
}

func TestAizhanAgentConfiguration(t *testing.T) {
	for _, tc := range []Config{
		{BaseURL: "https://www.aizhan.com", AgentURL: "http://agent:8002", Timeout: time.Second},
		{BaseURL: "https://www.aizhan.com", AgentURL: "http://secret@agent:8002", AgentToken: "token", Timeout: time.Second},
		{BaseURL: "https://other.com", AgentURL: "http://agent:8002", AgentToken: "token", Timeout: time.Second},
	} {
		if _, err := NewAizhan(tc, time.Minute); err == nil {
			t.Fatal("invalid agent config accepted")
		}
	}
}
