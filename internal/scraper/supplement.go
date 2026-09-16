package scraper

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"seo-monitor/internal/model"
)

var errSourceChallenge = errors.New("source returned an explicit access challenge")

// Only positive blocking evidence opens a source circuit. Empty responses,
// timeouts, malformed pages and ordinary 5xx errors retry just the current job.
func sourceBlocked(err error) bool {
	var status *sourceHTTPError
	return errors.Is(err, errSourceChallenge) || (errors.As(err, &status) &&
		(status.status == 403 || status.status == 429 || status.retryAfter.After(time.Now())))
}

func challengePage(body []byte) bool {
	text := strings.ToLower(strings.TrimSpace(string(body)))
	// Normal SEO pages can contain hidden login captcha widgets; only treat
	// a small challenge document with explicit wording as a blocked response.
	if len(body) > 16384 {
		return false
	}
	for _, marker := range []string{"访问过于频繁", "请求过于频繁", "访问已被拦截", "请完成安全验证", "verify you are human", "cf-chl-", `"error":"captcha"`} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

type ChinazSupplement struct {
	c        *Chinaz
	cooldown time.Duration
}

func NewChinazSupplement(cfg Config, cooldown time.Duration) (*ChinazSupplement, error) {
	if cfg.Timeout <= 0 || cfg.MinDelay < 0 || cfg.MaxDelay < cfg.MinDelay || cooldown <= 0 {
		return nil, errors.New("invalid supplement configuration")
	}
	c, err := NewChinaz(cfg)
	if err != nil {
		return nil, err
	}
	c.client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	c.baseCooldown = cooldown
	return &ChinazSupplement{c: c, cooldown: cooldown}, nil
}

func (s *ChinazSupplement) WaitReady(ctx context.Context) error { return s.c.waitSupplementReady(ctx) }

func (s *ChinazSupplement) Fetch(ctx context.Context, domain string) (model.Metric, error) {
	if err := s.WaitReady(ctx); err != nil {
		return model.Metric{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	m, err := s.c.fetchSupplement(ctx, domain)
	return m, s.recordResult(err)
}

func (s *ChinazSupplement) recordResult(err error) error {
	if err == nil {
		s.c.rateMu.Lock()
		s.c.supplementFailures = 0
		s.c.rateMu.Unlock()
		return nil
	}
	if !sourceBlocked(err) {
		return err
	}
	s.c.rateMu.Lock()
	s.c.supplementFailures++
	delay := s.cooldown
	for n := 1; n < s.c.supplementFailures && delay < time.Hour; n++ {
		delay *= 2
	}
	if delay > time.Hour {
		delay = time.Hour
	}
	s.c.cooldownUntil = time.Now().Add(delay)
	var upstream *sourceHTTPError
	if errors.As(err, &upstream) && upstream.retryAfter.After(s.c.cooldownUntil) {
		s.c.cooldownUntil = upstream.retryAfter
	}
	until := s.c.cooldownUntil
	s.c.rateMu.Unlock()
	return fmt.Errorf("Chinaz source cooling down until %s: %w", until.UTC().Format(time.RFC3339), err)
}
