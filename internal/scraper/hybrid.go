package scraper

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"seo-monitor/internal/domainutil"
	"seo-monitor/internal/model"
)

type sourceHTTPError struct {
	status     int
	retryAfter time.Time
}

func (e *sourceHTTPError) Error() string { return fmt.Sprintf("source returned HTTP %d", e.status) }

// Hybrid keeps Aizhan's metrics and copies only the five requested Chinaz fields.
// Either source's request failure retries the job without overwriting stored data.
type Hybrid struct {
	aizhan   *Aizhan
	chinaz   *Chinaz
	cooldown time.Duration
	slot     chan struct{}
}

func NewHybrid(primary Config, aizhanCooldown time.Duration, supplement Config, chinazCooldown time.Duration) (*Hybrid, error) {
	a, err := NewAizhan(primary, aizhanCooldown)
	if err != nil {
		return nil, err
	}
	if supplement.Timeout <= 0 || supplement.MinDelay < 0 || supplement.MaxDelay < supplement.MinDelay || chinazCooldown <= 0 {
		return nil, errors.New("invalid Chinaz supplement timeout, delay or cooldown")
	}
	c, err := NewChinaz(supplement)
	if err != nil {
		return nil, err
	}
	c.client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &Hybrid{aizhan: a, chinaz: c, cooldown: chinazCooldown, slot: make(chan struct{}, 1)}, nil
}

func (h *Hybrid) WaitReady(ctx context.Context) error {
	if err := h.aizhan.WaitReady(ctx); err != nil {
		return err
	}
	return h.chinaz.waitSupplementReady(ctx)
}

func (c *Chinaz) waitSupplementReady(ctx context.Context) error {
	c.rateMu.Lock()
	until := c.cooldownUntil
	c.rateMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay := time.Until(until); delay > 0 {
		return aizhanWait(ctx, delay)
	}
	return nil
}

func (h *Hybrid) Fetch(ctx context.Context, domain string) (model.Metric, error) {
	select {
	case h.slot <- struct{}{}:
	case <-ctx.Done():
		return model.Metric{}, ctx.Err()
	}
	defer func() { <-h.slot }()
	if err := h.WaitReady(ctx); err != nil {
		return model.Metric{}, err
	}
	ctx, stop := context.WithTimeout(ctx, 4*time.Minute)
	defer stop()
	metric, err := h.aizhan.Fetch(ctx, domain)
	if err != nil {
		return model.Metric{}, err
	}
	// Keep the complete job below the configured stale-job recovery window.
	supplementCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	extra, err := h.chinaz.fetchSupplement(supplementCtx, domain)
	if err != nil {
		if ctx.Err() != nil {
			return model.Metric{}, ctx.Err()
		}
		h.chinaz.rateMu.Lock()
		h.chinaz.supplementFailures++
		delay := h.cooldown
		for n := 1; n < h.chinaz.supplementFailures && delay < time.Hour; n++ {
			delay *= 2
		}
		if delay > time.Hour {
			delay = time.Hour
		}
		h.chinaz.cooldownUntil = time.Now().Add(delay)
		var upstream *sourceHTTPError
		if errors.As(err, &upstream) && upstream.retryAfter.After(h.chinaz.cooldownUntil) {
			h.chinaz.cooldownUntil = upstream.retryAfter
		}
		until := h.chinaz.cooldownUntil
		h.chinaz.rateMu.Unlock()
		return model.Metric{}, fmt.Errorf("Chinaz supplement failed (cooldown until %s): %w", until.UTC().Format(time.RFC3339), err)
	}
	h.chinaz.rateMu.Lock()
	h.chinaz.supplementFailures = 0
	h.chinaz.cooldownUntil = time.Time{}
	h.chinaz.rateMu.Unlock()
	metric.APPPCPCrank = extra.APPPCPCrank
	metric.SiteCategory = extra.SiteCategory
	metric.RegistrantName = extra.RegistrantName
	metric.RegistrantEmail = extra.RegistrantEmail
	metric.ExpiresOn = extra.ExpiresOn
	metric.SupplementalSourceURL = extra.SourceURL
	metric.SupplementalRawSHA256 = extra.RawSHA256
	metric.SupplementalCollectedAt = &extra.CollectedAt
	return metric, nil
}

// fetchSupplement intentionally never calls Rank.ashx and does not require
// Chinaz weights. Empty optional fields are valid; transport/parse failures are not.
func (c *Chinaz) fetchSupplement(ctx context.Context, domain string) (model.Metric, error) {
	domain, err := domainutil.Normalize(domain)
	if err != nil {
		return model.Metric{}, err
	}
	pageURL := c.baseURL + "/" + url.PathEscape(domain)
	read := func(target, referer string) ([]byte, error) {
		if err := c.waitForSlot(ctx); err != nil {
			return nil, err
		}
		body, _, err := c.fetchOnceWithReferer(ctx, target, referer)
		return body, err // Durable queue retries; no burst on source blocking.
	}
	body, err := read(pageURL, "")
	if err != nil {
		return model.Metric{}, err
	}
	metric, err := Parse(body)
	if err != nil {
		return model.Metric{}, err
	}
	hash := sha256.New()
	appendBody := func(b []byte) { _, _ = hash.Write(b); _, _ = hash.Write([]byte{0}) }
	appendBody(body)
	if metric.APPPCPCrank == nil || metric.SiteCategory == nil {
		key, err := extractSecretKey(body)
		if err != nil {
			return model.Metric{}, err
		}
		for _, item := range []struct {
			endpoint, action string
			needed           bool
			merge            func([]byte, *model.Metric) error
		}{
			{"/SiteAPPAndPC.ashx", "", metric.APPPCPCrank == nil, mergeAPPPCResponse},
			{"/GetTopRanked.ashx", "GetSiteCategory", metric.SiteCategory == nil, mergeCategoryResponse},
		} {
			if !item.needed {
				continue
			}
			data, err := read(c.dataURL(item.endpoint, item.action, domain, key), pageURL)
			if err != nil {
				return model.Metric{}, err
			}
			var status struct {
				StateCode *int `json:"StateCode"`
			}
			if err := decodeJSONP(data, &status); err != nil {
				return model.Metric{}, err
			}
			if status.StateCode == nil || (*status.StateCode != 0 && *status.StateCode != 1) {
				return model.Metric{}, errors.New("Chinaz supplement returned invalid status")
			}
			// A missing Result is malformed, unlike an explicit no-data response.
			var envelope map[string]json.RawMessage
			if err := decodeJSONP(data, &envelope); err != nil {
				return model.Metric{}, err
			}
			if _, ok := envelope["Result"]; !ok {
				return model.Metric{}, errors.New("Chinaz supplement missing Result")
			}
			if err := item.merge(data, &metric); err != nil {
				return model.Metric{}, err
			}
			appendBody(data)
		}
	}
	if err := ctx.Err(); err != nil {
		return model.Metric{}, err
	}
	metric.SourceURL, metric.RawSHA256, metric.CollectedAt = pageURL, hex.EncodeToString(hash.Sum(nil)), time.Now().UTC()
	return metric, nil
}
