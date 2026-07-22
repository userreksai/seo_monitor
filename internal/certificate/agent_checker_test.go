package certificate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"seo-monitor/internal/model"
)

type checkerFunc func(context.Context, string) (model.Certificate, error)

func (f checkerFunc) Check(ctx context.Context, domain string) (model.Certificate, error) {
	return f(ctx, domain)
}

func TestAgentFallbackCheckerUsesAgentAfterLocalFailure(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 22, 6, 0, 0, 0, time.UTC)
	expiresAt := now.Add(90 * 24 * time.Hour)
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/tasks" {
			t.Errorf("unexpected path %q", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer shared-token" {
			t.Errorf("unexpected authorization header")
		}
		var task agentTaskRequest
		if err := json.NewDecoder(request.Body).Decode(&task); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if task.Type != "certificate" || task.Target != "example.com" || task.Options.TimeoutMS != 2000 {
			t.Errorf("unexpected task: %+v", task)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"taskId": task.TaskID,
			"agent":  map[string]any{"name": "edge-1"},
			"result": map[string]any{
				"available": true,
				"certificate": map[string]any{
					"domain": "example.com", "issuer": "Test CA", "subject": "example.com",
					"serialNumber": "1234", "dnsNames": []string{"example.com"},
					"validFrom": now, "expiresAt": expiresAt, "checkedAt": now,
					"hostnameValid": true, "resolvedAddress": "192.0.2.1:443",
				},
			},
		})
	}))
	defer agent.Close()

	local := checkerFunc(func(_ context.Context, domain string) (model.Certificate, error) {
		return model.Certificate{Domain: domain, CheckedAt: now, CheckSource: "master"}, errors.New("DNS timeout")
	})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	checker, err := NewAgentFallbackChecker(local, []string{agent.URL}, "shared-token", 2*time.Second, 2, logger)
	if err != nil {
		t.Fatalf("NewAgentFallbackChecker: %v", err)
	}
	result, err := checker.Check(context.Background(), "example.com")
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.CheckSource != "agent:edge-1" || result.ResolvedAddr != "192.0.2.1:443" {
		t.Fatalf("unexpected source: %+v", result)
	}
	if result.ExpiresAt == nil || !result.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("unexpected expiry: %+v", result.ExpiresAt)
	}
}

func TestAgentFallbackCheckerKeepsLocalSuccess(t *testing.T) {
	t.Parallel()
	want := model.Certificate{Domain: "example.com", CheckSource: "master"}
	local := checkerFunc(func(context.Context, string) (model.Certificate, error) { return want, nil })
	checker, err := NewAgentFallbackChecker(local, nil, "", time.Second, 0, nil)
	if err != nil {
		t.Fatalf("NewAgentFallbackChecker: %v", err)
	}
	got, err := checker.Check(context.Background(), "example.com")
	if err != nil || got.CheckSource != want.CheckSource {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestNormalizeAgentEndpoint(t *testing.T) {
	t.Parallel()
	got, err := normalizeAgentEndpoint("https://agent.example/base/")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://agent.example/base/api/v1/tasks" {
		t.Fatalf("endpoint=%q", got)
	}
	if _, err := normalizeAgentEndpoint("file:///tmp/agent"); err == nil {
		t.Fatal("expected invalid URL error")
	}
}

func TestAgentFallbackCheckerRejectsTooShortTimeout(t *testing.T) {
	t.Parallel()
	local := checkerFunc(func(context.Context, string) (model.Certificate, error) {
		return model.Certificate{}, nil
	})
	if _, err := NewAgentFallbackChecker(local, []string{"https://agent.example"}, "", 100*time.Millisecond, 1, nil); err == nil {
		t.Fatal("expected timeout validation error")
	}
}
