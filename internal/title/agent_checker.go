package title

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"seo-monitor/internal/model"
)

const maxAgentResponseBytes = 64 << 10

type Checker interface {
	Check(context.Context, string) (model.TitleObservation, error)
}

type AgentChecker struct {
	agents []*agentClient
	next   atomic.Uint64
}

type agentClient struct {
	endpoint  string
	token     string
	timeout   time.Duration
	client    *http.Client
	semaphore chan struct{}
}

type agentTaskRequest struct {
	TaskID  string           `json:"taskId"`
	Type    string           `json:"type"`
	Target  string           `json:"target"`
	Options agentTaskOptions `json:"options"`
}

type agentTaskOptions struct {
	TimeoutMS int64 `json:"timeoutMs"`
}

type agentTaskResponse struct {
	TaskID string `json:"taskId"`
	Type   string `json:"type"`
	Status string `json:"status"`
	Agent  struct {
		Name string `json:"name"`
	} `json:"agent"`
	Result struct {
		Available bool        `json:"available"`
		Title     *agentTitle `json:"title"`
	} `json:"result"`
}

type agentTitle struct {
	Title       string    `json:"title"`
	FinalURL    string    `json:"finalUrl"`
	StatusCode  int       `json:"statusCode"`
	ContentType string    `json:"contentType"`
	CheckedAt   time.Time `json:"checkedAt"`
	Error       string    `json:"error"`
}

func NewAgentChecker(rawURLs []string, token string, timeout time.Duration, maxConcurrent int) (*AgentChecker, error) {
	if len(rawURLs) == 0 {
		return nil, errors.New("at least one title Agent URL is required")
	}
	if timeout < 500*time.Millisecond || timeout > time.Minute {
		return nil, errors.New("title Agent timeout must be between 500ms and 1m")
	}
	if maxConcurrent < 1 || maxConcurrent > 50 {
		return nil, errors.New("title Agent concurrency must be between 1 and 50")
	}

	checker := &AgentChecker{}
	for _, rawURL := range rawURLs {
		endpoint, err := normalizeAgentEndpoint(rawURL)
		if err != nil {
			return nil, err
		}
		checker.agents = append(checker.agents, &agentClient{
			endpoint:  endpoint,
			token:     strings.TrimSpace(token),
			timeout:   timeout,
			client:    &http.Client{Timeout: timeout + 5*time.Second},
			semaphore: make(chan struct{}, maxConcurrent),
		})
	}
	return checker, nil
}

func (c *AgentChecker) Check(ctx context.Context, domain string) (model.TitleObservation, error) {
	start := int(c.next.Add(1)-1) % len(c.agents)
	failures := make([]string, 0, len(c.agents))
	for offset := 0; offset < len(c.agents); offset++ {
		agent := c.agents[(start+offset)%len(c.agents)]
		observation, err := agent.Check(ctx, domain)
		if err == nil {
			return observation, nil
		}
		failures = append(failures, agent.endpoint+": "+strings.TrimSpace(err.Error()))
		if ctx.Err() != nil {
			return model.TitleObservation{}, ctx.Err()
		}
	}
	return model.TitleObservation{}, errors.New(strings.Join(failures, "; "))
}

func (c *agentClient) Check(ctx context.Context, domain string) (model.TitleObservation, error) {
	select {
	case c.semaphore <- struct{}{}:
		defer func() { <-c.semaphore }()
	case <-ctx.Done():
		return model.TitleObservation{}, ctx.Err()
	}

	taskID := fmt.Sprintf("title-%d", time.Now().UnixNano())
	payload, err := json.Marshal(agentTaskRequest{
		TaskID: taskID, Type: "title", Target: domain,
		Options: agentTaskOptions{TimeoutMS: c.timeout.Milliseconds()},
	})
	if err != nil {
		return model.TitleObservation{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return model.TitleObservation{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}

	response, err := c.client.Do(request)
	if err != nil {
		return model.TitleObservation{}, fmt.Errorf("request Agent: %w", err)
	}
	defer response.Body.Close()
	limited := io.LimitReader(response.Body, maxAgentResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return model.TitleObservation{}, fmt.Errorf("read Agent response: %w", err)
	}
	if len(body) > maxAgentResponseBytes {
		return model.TitleObservation{}, errors.New("Agent response is too large")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return model.TitleObservation{}, fmt.Errorf("Agent returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}

	var task agentTaskResponse
	if err := json.Unmarshal(body, &task); err != nil {
		return model.TitleObservation{}, fmt.Errorf("decode Agent response: %w", err)
	}
	if task.TaskID != taskID {
		return model.TitleObservation{}, errors.New("Agent task ID mismatch")
	}
	if task.Type != "title" || task.Status != "completed" {
		return model.TitleObservation{}, errors.New("Agent task type or status is invalid")
	}
	if !task.Result.Available || task.Result.Title == nil {
		message := "Agent reported title unavailable"
		if task.Result.Title != nil && strings.TrimSpace(task.Result.Title.Error) != "" {
			message = strings.TrimSpace(task.Result.Title.Error)
		}
		return model.TitleObservation{}, errors.New(message)
	}

	result := task.Result.Title
	if message := strings.TrimSpace(result.Error); message != "" {
		return model.TitleObservation{}, errors.New(message)
	}
	title := strings.Join(strings.Fields(result.Title), " ")
	if title == "" {
		return model.TitleObservation{}, errors.New("Agent returned an empty title")
	}
	if utf8.RuneCountInString(title) > 4096 {
		return model.TitleObservation{}, errors.New("Agent returned a title longer than 4096 characters")
	}
	if result.StatusCode < http.StatusOK || result.StatusCode >= http.StatusMultipleChoices {
		return model.TitleObservation{}, fmt.Errorf("target returned HTTP %d", result.StatusCode)
	}
	finalURL, err := url.Parse(result.FinalURL)
	if err != nil || finalURL.Hostname() == "" || (finalURL.Scheme != "http" && finalURL.Scheme != "https") {
		return model.TitleObservation{}, errors.New("Agent returned an invalid final URL")
	}
	checkedAt := result.CheckedAt.UTC()
	if result.CheckedAt.IsZero() {
		checkedAt = time.Now().UTC()
	}
	agentName := strings.TrimSpace(task.Agent.Name)
	if agentName == "" {
		agentName = c.endpoint
	}
	return model.TitleObservation{
		Title: title, FinalURL: finalURL.String(), StatusCode: result.StatusCode,
		ContentType: strings.TrimSpace(result.ContentType), CheckedAt: checkedAt,
		CheckSource: "agent:" + agentName,
	}, nil
}

func normalizeAgentEndpoint(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("invalid title Agent URL %q", raw)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("title Agent URL must not contain credentials, query, or fragment: %q", raw)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(parsed.Path, "/api/v1/tasks") {
		parsed.Path += "/api/v1/tasks"
	}
	return parsed.String(), nil
}
