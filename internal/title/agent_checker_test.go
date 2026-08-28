package title

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAgentCheckerValidatesAndReturnsTitle(t *testing.T) {
	now := time.Date(2026, 8, 28, 1, 0, 0, 0, time.UTC)
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer shared-token" {
			t.Error("missing bearer token")
		}
		var task agentTaskRequest
		if err := json.NewDecoder(request.Body).Decode(&task); err != nil {
			t.Fatal(err)
		}
		if task.Type != "title" || task.Target != "seo.chinaz.com" {
			t.Fatalf("unexpected task: %+v", task)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"taskId": task.TaskID, "type": "title", "status": "completed",
			"agent": map[string]string{"name": "edge-1"},
			"result": map[string]any{"available": true, "title": map[string]any{
				"title": " SEO综合查询  -  站长工具 ", "finalUrl": "https://seo.chinaz.com/",
				"statusCode": 200, "contentType": "text/html; charset=utf-8", "checkedAt": now,
			}},
		})
	}))
	defer agent.Close()

	checker, err := NewAgentChecker([]string{agent.URL}, "shared-token", 2*time.Second, 2)
	if err != nil {
		t.Fatal(err)
	}
	result, err := checker.Check(context.Background(), "seo.chinaz.com")
	if err != nil {
		t.Fatal(err)
	}
	if result.Title != "SEO综合查询 - 站长工具" || result.CheckSource != "agent:edge-1" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestAgentCheckerRejectsTaskMismatch(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"taskId": "wrong", "type": "title", "status": "completed",
			"result": map[string]any{"available": true},
		})
	}))
	defer agent.Close()
	checker, err := NewAgentChecker([]string{agent.URL}, "", time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := checker.Check(context.Background(), "example.com"); err == nil {
		t.Fatal("expected task mismatch error")
	}
}
