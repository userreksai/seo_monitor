package scraper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"time"

	"seo-monitor/internal/domainutil"
	"seo-monitor/internal/model"
)

// WeightFallback shares the Chinaz rate limiter and circuit with the independent
// supplemental queue, but never waits on that queue to save valid weights.
type WeightFallback struct {
	primary  *Aizhan
	fallback *ChinazSupplement
}

func NewWeightFallback(a *Aizhan, c *ChinazSupplement) *WeightFallback {
	return &WeightFallback{primary: a, fallback: c}
}

func (w *WeightFallback) cooldowns() (time.Time, time.Time) {
	w.primary.mu.Lock()
	a := w.primary.cooldown
	w.primary.mu.Unlock()
	w.fallback.c.rateMu.Lock()
	c := w.fallback.c.cooldownUntil
	w.fallback.c.rateMu.Unlock()
	return a, c
}

func (w *WeightFallback) WaitReady(ctx context.Context) error {
	for {
		a, c := w.cooldowns()
		if !a.After(time.Now()) || !c.After(time.Now()) {
			return ctx.Err()
		}
		if c.Before(a) {
			a = c
		}
		if err := aizhanWait(ctx, time.Until(a)); err != nil {
			return err
		}
	}
}

func (w *WeightFallback) Fetch(ctx context.Context, domain string) (model.Metric, error) {
	if err := w.WaitReady(ctx); err != nil {
		return model.Metric{}, err
	}
	// Both providers and all internal retries remain below stale-job recovery.
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	a, _ := w.cooldowns()
	primaryErr := fmt.Errorf("Aizhan cooling down until %s", a.UTC().Format(time.RFC3339))
	if !a.After(time.Now()) {
		m, err := w.primary.Fetch(ctx, domain)
		if err == nil {
			m.MarkWeights("aizhan")
			return m, nil
		}
		primaryErr = err
	}
	if ctx.Err() != nil {
		return model.Metric{}, ctx.Err()
	}
	_, c := w.cooldowns()
	if c.After(time.Now()) {
		return model.Metric{}, fmt.Errorf("%v; Chinaz cooling down until %s", primaryErr, c.UTC().Format(time.RFC3339))
	}
	w.primary.logger.Warn("Aizhan weights unavailable; trying Chinaz", "domain", domain, "error", primaryErr)
	fallbackCtx, stop := context.WithTimeout(ctx, 2*time.Minute)
	defer stop()
	m, err := w.fallback.c.fetchWeights(fallbackCtx, domain)
	err = w.fallback.recordResult(err)
	if err != nil {
		return model.Metric{}, fmt.Errorf("Aizhan failed: %v; Chinaz weights failed: %w", primaryErr, err)
	}
	m.MarkWeights("chinaz")
	m.CollectionRoute = "direct:chinaz-fallback"
	w.primary.logger.Info("Chinaz fallback weights collected", "domain", domain, "weight_source", m.WeightSource)
	return m, nil
}

// Fetch only primary SEO fields. APPPC/category/WHOIS remain separate jobs.
func (c *Chinaz) fetchWeights(ctx context.Context, domain string) (model.Metric, error) {
	domain, err := domainutil.Normalize(domain)
	if err != nil {
		return model.Metric{}, err
	}
	pageURL := c.baseURL + "/" + url.PathEscape(domain)
	body, err := c.readShared(ctx, pageURL, "")
	if err != nil {
		return model.Metric{}, err
	}
	m, err := Parse(body)
	if err != nil {
		return model.Metric{}, err
	}
	hash := sha256.New()
	hash.Write(body)
	// Pages with a public data key can contain placeholder badges. Read the
	// weight response whenever that key is present, even if page badges say zero.
	if key, keyErr := extractSecretKey(body); keyErr == nil {
		body, err = c.readShared(ctx, c.dataURL("/Rank.ashx", "rankdata", domain, key), pageURL)
		if err != nil {
			return model.Metric{}, err
		}
		hash.Write([]byte{0})
		hash.Write(body)
		// Do not allow page placeholders to fill omitted API weight fields.
		m.BaiduPCWeight, m.BaiduMobile, m.SogouWeight, m.BingWeight, m.So360Weight, m.ShenmaWeight = nil, nil, nil, nil, nil, nil
		if err = mergeRankResponse(body, &m); err != nil {
			return model.Metric{}, err
		}
	}
	if !m.HasBaiduWeights() {
		return model.Metric{}, errors.New("Chinaz Baidu weights incomplete")
	}
	m.SourceURL, m.RawSHA256, m.CollectedAt = pageURL, hex.EncodeToString(hash.Sum(nil)), time.Now().UTC()
	return m, nil
}

// Serialize and pace requests across weights fallback and supplemental jobs.
// Positive blocking evidence closes the shared circuit before releasing the slot.
func (c *Chinaz) readShared(ctx context.Context, target, referer string) ([]byte, error) {
	select {
	case c.requestSlot <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-c.requestSlot }()
	c.rateMu.Lock()
	until := c.cooldownUntil
	c.rateMu.Unlock()
	if until.After(time.Now()) {
		return nil, fmt.Errorf("Chinaz cooling down until %s", until.UTC().Format(time.RFC3339))
	}
	if err := c.waitForSlot(ctx); err != nil {
		return nil, err
	}
	body, _, err := c.fetchOnceWithReferer(ctx, target, referer)
	if err == nil && challengePage(body) {
		err = errSourceChallenge
	}
	if sourceBlocked(err) {
		c.rateMu.Lock()
		c.cooldownUntil = time.Now().Add(c.baseCooldown)
		var upstream *sourceHTTPError
		if errors.As(err, &upstream) && upstream.retryAfter.After(c.cooldownUntil) {
			c.cooldownUntil = upstream.retryAfter
		}
		c.rateMu.Unlock()
	}
	return body, err
}
