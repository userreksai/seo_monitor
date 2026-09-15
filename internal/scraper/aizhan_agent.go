package scraper

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type aizhanAgent struct {
	endpoint string
	token    string
	timeout  time.Duration
	maxBody  int64
	client   *http.Client
}

func newAizhanAgent(cfg Config) (*aizhanAgent, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.AgentURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid AIZHAN_AGENT_URL")
	}
	if strings.TrimSpace(cfg.AgentToken) == "" {
		return nil, errors.New("AIZHAN_AGENT_TOKEN or existing TITLE_AGENT_TOKEN/CERTIFICATE_AGENT_TOKEN is required")
	}
	if cfg.Timeout < 500*time.Millisecond {
		return nil, errors.New("Agent SCRAPE_TIMEOUT must be at least 500ms")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(u.Path, "/api/v1/tasks") {
		u.Path += "/api/v1/tasks"
	}
	return &aizhanAgent{endpoint: u.String(), token: strings.TrimSpace(cfg.AgentToken), timeout: cfg.Timeout, maxBody: cfg.MaxResponseBytes,
		client: &http.Client{Timeout: cfg.Timeout + 5*time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

type seoAgentResponse struct {
	TaskID string `json:"taskId"`
	Type   string `json:"type"`
	Target string `json:"target"`
	Status string `json:"status"`
	Result struct {
		Available bool `json:"available"`
		SEO       *struct {
			URL           string     `json:"url"`
			StatusCode    int        `json:"statusCode"`
			Body          []byte     `json:"body"`
			Error         string     `json:"error"`
			RetryAt       *time.Time `json:"retryAt"`
			SourceBlocked bool       `json:"sourceBlocked"`
		} `json:"seo"`
	} `json:"result"`
}

func (a *aizhanAgent) fetch(ctx context.Context, target string) ([]byte, bool, time.Time, error) {
	u, err := url.Parse(target)
	if err != nil || u.Scheme != "https" || u.Host != "www.aizhan.com" || u.RawQuery != "" || u.Fragment != "" {
		return nil, false, time.Time{}, errors.New("Agent mode requires SOURCE_BASE_URL=https://www.aizhan.com")
	}
	domain := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/cha/"), "/")
	if u.Path != "/cha/"+domain+"/" || strings.Contains(domain, "/") {
		return nil, false, time.Time{}, errors.New("invalid Aizhan query path")
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return nil, false, time.Time{}, err
	}
	id := "seo-" + hex.EncodeToString(random[:])
	payload, err := json.Marshal(map[string]any{"taskId": id, "type": "seo", "target": domain, "options": map[string]int64{"timeoutMs": a.timeout.Milliseconds()}})
	if err != nil {
		return nil, false, time.Time{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, false, time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, false, time.Time{}, fmt.Errorf("request SEO Agent: %w", err)
	}
	defer resp.Body.Close()
	after := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	if resp.StatusCode != http.StatusOK {
		// The Agent's own 429 is capacity pressure, not Aizhan blocking.
		return nil, false, time.Time{}, fmt.Errorf("SEO Agent returned HTTP %d; verify Agent capacity, type=seo support and shared token", resp.StatusCode)
	}
	// Body is base64 in JSON. Permit bounded encoding overhead, never unbounded HTML.
	limit := ((a.maxBody+2)/3)*4 + 65536
	if limit > 5<<20 {
		limit = 5 << 20
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, false, after, err
	}
	if int64(len(body)) > limit {
		return nil, false, after, errors.New("SEO Agent response too large")
	}
	var result seoAgentResponse
	if err = json.Unmarshal(body, &result); err != nil {
		return nil, false, after, fmt.Errorf("decode SEO Agent: %w", err)
	}
	if result.TaskID != id || result.Type != "seo" || result.Target != domain || result.Status != "completed" || result.Result.SEO == nil {
		return nil, false, after, errors.New("SEO Agent task identity/type/result mismatch")
	}
	r := result.Result.SEO
	if r.URL != target {
		return nil, false, after, errors.New("SEO Agent source URL mismatch")
	}
	if r.RetryAt != nil && r.RetryAt.After(after) {
		after = *r.RetryAt
	}
	if !result.Result.Available || r.Error != "" || r.StatusCode != http.StatusOK {
		message := r.Error
		if len(message) > 512 {
			message = message[:512]
		}
		cause := fmt.Errorf("SEO Agent upstream failed (HTTP %d): %s", r.StatusCode, message)
		if r.SourceBlocked || r.StatusCode == 403 || r.StatusCode == 429 {
			return nil, false, after, fmt.Errorf("%v: %w", cause, &sourceHTTPError{status: 429, retryAfter: after})
		}
		return nil, false, time.Time{}, cause
	}
	if len(r.Body) == 0 {
		return nil, false, after, errors.New("SEO Agent returned HTTP 200 with an empty body")
	}
	if int64(len(r.Body)) > a.maxBody {
		return nil, false, after, errors.New("SEO Agent page too large")
	}
	return r.Body, false, after, nil
}
