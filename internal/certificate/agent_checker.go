package certificate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"seo-monitor/internal/model"
)

const maxAgentResponseBytes = 128 << 10

type AgentFallbackChecker struct {
	local  Checker
	agents []*certificateAgentClient
	next   atomic.Uint64
	logger *slog.Logger
}

type certificateAgentClient struct {
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
	Agent  struct {
		Name string `json:"name"`
	} `json:"agent"`
	Result struct {
		Available   bool              `json:"available"`
		Certificate *agentCertificate `json:"certificate"`
	} `json:"result"`
}

type agentCertificate struct {
	Domain          string     `json:"domain"`
	Issuer          string     `json:"issuer"`
	Subject         string     `json:"subject"`
	SerialNumber    string     `json:"serialNumber"`
	DNSNames        []string   `json:"dnsNames"`
	ValidFrom       *time.Time `json:"validFrom"`
	ExpiresAt       *time.Time `json:"expiresAt"`
	CheckedAt       time.Time  `json:"checkedAt"`
	HostnameValid   bool       `json:"hostnameValid"`
	ResolvedAddress string     `json:"resolvedAddress"`
	Error           string     `json:"error"`
}

func NewAgentFallbackChecker(local Checker, rawURLs []string, token string, timeout time.Duration, maxConcurrent int, logger *slog.Logger) (Checker, error) {
	if local == nil {
		return nil, fmt.Errorf("local certificate checker is required")
	}
	if len(rawURLs) == 0 {
		return local, nil
	}
	if timeout < 500*time.Millisecond {
		return nil, fmt.Errorf("certificate agent timeout must be at least 500ms")
	}
	if maxConcurrent < 1 {
		return nil, fmt.Errorf("certificate agent concurrency must be at least 1")
	}
	if logger == nil {
		logger = slog.Default()
	}
	checker := &AgentFallbackChecker{local: local, logger: logger}
	for _, rawURL := range rawURLs {
		endpoint, err := normalizeAgentEndpoint(rawURL)
		if err != nil {
			return nil, err
		}
		checker.agents = append(checker.agents, &certificateAgentClient{
			endpoint:  endpoint,
			token:     token,
			timeout:   timeout,
			client:    &http.Client{Timeout: timeout + 5*time.Second},
			semaphore: make(chan struct{}, maxConcurrent),
		})
	}
	return checker, nil
}

func normalizeAgentEndpoint(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", fmt.Errorf("invalid certificate agent URL %q", raw)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("certificate agent URL must not contain credentials, query, or fragment: %q", raw)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(parsed.Path, "/api/v1/tasks") {
		parsed.Path += "/api/v1/tasks"
	}
	return parsed.String(), nil
}

func (c *AgentFallbackChecker) Check(ctx context.Context, domain string) (model.Certificate, error) {
	localResult, localErr := c.local.Check(ctx, domain)
	if localErr == nil {
		return localResult, nil
	}

	errorsByNode := []string{"master: " + strings.TrimSpace(localErr.Error())}
	start := int(c.next.Add(1)-1) % len(c.agents)
	for offset := 0; offset < len(c.agents); offset++ {
		agent := c.agents[(start+offset)%len(c.agents)]
		result, agentName, err := agent.Check(ctx, domain)
		if err == nil {
			if agentName == "" {
				agentName = agent.endpoint
			}
			result.CheckSource = "agent:" + agentName
			c.logger.Info("certificate resolved through agent", "domain", domain, "agent", agentName,
				"master_error", localErr.Error())
			return result, nil
		}
		errorsByNode = append(errorsByNode, agent.endpoint+": "+strings.TrimSpace(err.Error()))
	}
	return localResult, errors.New(strings.Join(errorsByNode, "; "))
}

func (c *certificateAgentClient) Check(ctx context.Context, domain string) (model.Certificate, string, error) {
	select {
	case c.semaphore <- struct{}{}:
		defer func() { <-c.semaphore }()
	case <-ctx.Done():
		return model.Certificate{}, "", ctx.Err()
	}
	taskID := fmt.Sprintf("certificate-%d", time.Now().UnixNano())
	payload, err := json.Marshal(agentTaskRequest{
		TaskID: taskID,
		Type:   "certificate",
		Target: domain,
		Options: agentTaskOptions{
			TimeoutMS: c.timeout.Milliseconds(),
		},
	})
	if err != nil {
		return model.Certificate{}, "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return model.Certificate{}, "", err
	}
	request.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}

	response, err := c.client.Do(request)
	if err != nil {
		return model.Certificate{}, "", fmt.Errorf("request failed: %w", err)
	}
	defer response.Body.Close()
	body := io.LimitReader(response.Body, maxAgentResponseBytes)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(body)
		return model.Certificate{}, "", fmt.Errorf("HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	var result agentTaskResponse
	if err := json.NewDecoder(body).Decode(&result); err != nil {
		return model.Certificate{}, "", fmt.Errorf("decode response: %w", err)
	}
	if result.TaskID != taskID {
		return model.Certificate{}, result.Agent.Name, fmt.Errorf("task ID mismatch")
	}
	if result.Result.Certificate == nil {
		return model.Certificate{}, result.Agent.Name, fmt.Errorf("certificate result is missing")
	}
	certificate := result.Result.Certificate
	if !result.Result.Available || strings.TrimSpace(certificate.Error) != "" {
		message := strings.TrimSpace(certificate.Error)
		if message == "" {
			message = "agent reported certificate unavailable"
		}
		return model.Certificate{}, result.Agent.Name, errors.New(message)
	}
	checkedAt := certificate.CheckedAt
	if checkedAt.IsZero() {
		checkedAt = time.Now().UTC()
	}
	return model.Certificate{
		Domain:        domain,
		Issuer:        certificate.Issuer,
		Subject:       certificate.Subject,
		SerialNumber:  certificate.SerialNumber,
		DNSNames:      append([]string(nil), certificate.DNSNames...),
		ValidFrom:     certificate.ValidFrom,
		ExpiresAt:     certificate.ExpiresAt,
		CheckedAt:     checkedAt,
		HostnameValid: certificate.HostnameValid,
		ResolvedAddr:  certificate.ResolvedAddress,
	}, result.Agent.Name, nil
}
