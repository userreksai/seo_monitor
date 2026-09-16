package scraper

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
	"seo-monitor/internal/domainutil"
	"seo-monitor/internal/model"
)

// Aizhan serializes requests, including retries. Use one collector process per
// outbound IP: this limiter deliberately does not claim distributed guarantees.
type Aizhan struct {
	cfg          Config
	client       *http.Client
	slot         chan struct{}
	mu           sync.Mutex
	next         time.Time
	cooldown     time.Time
	failures     int
	baseCooldown time.Duration
	agent        *aizhanAgent
	logger       *slog.Logger
}

func NewAizhan(cfg Config, cooldown time.Duration) (*Aizhan, error) {
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://www.aizhan.com"
	}
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("invalid Aizhan base URL")
	}
	if cfg.Timeout <= 0 || cfg.MinDelay < 0 || cfg.MaxDelay < cfg.MinDelay || cooldown <= 0 {
		return nil, errors.New("invalid Aizhan timeout, delay or cooldown")
	}
	if cfg.Retries < 1 {
		cfg.Retries = 1
	}
	// Long retries belong to the durable job queue, not an occupied worker.
	if cfg.Retries > 2 {
		cfg.Retries = 2
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = 3 * 1024 * 1024
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	var agent *aizhanAgent
	if cfg.AgentURL != "" {
		if cfg.BaseURL != "https://www.aizhan.com" {
			return nil, errors.New("Agent mode requires SOURCE_BASE_URL=https://www.aizhan.com")
		}
		agent, err = newAizhanAgent(cfg)
		if err != nil {
			return nil, err
		}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Aizhan{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }},
		slot: make(chan struct{}, 1), baseCooldown: cooldown, agent: agent, logger: logger}, nil
}

// WaitReady runs before a job is claimed, so a long source outage does not
// consume every queued job's retry budget or leave jobs running during cooldown.
func (a *Aizhan) WaitReady(ctx context.Context) error {
	for {
		a.mu.Lock()
		until := a.next
		if a.cooldown.After(until) {
			until = a.cooldown
		}
		a.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		if time.Until(until) <= 0 {
			return nil
		}
		if err := aizhanWait(ctx, time.Until(until)); err != nil {
			return err
		}
	}
}

func aizhanWait(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (a *Aizhan) delay() {
	d := a.cfg.MinDelay
	if a.cfg.MaxDelay > d {
		d += time.Duration(rand.Int64N(int64(a.cfg.MaxDelay - d)))
	}
	a.mu.Lock()
	a.next = time.Now().Add(d)
	a.mu.Unlock()
}

func (a *Aizhan) fail(retryAfter time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failures++
	d := a.baseCooldown
	for n := 1; n < a.failures && d < time.Hour; n++ {
		d *= 2
	}
	if d > time.Hour {
		d = time.Hour
	}
	a.cooldown = time.Now().Add(d)
	if retryAfter.After(a.cooldown) {
		a.cooldown = retryAfter
	}
}

func (a *Aizhan) Fetch(ctx context.Context, domain string) (model.Metric, error) {
	normalized, err := domainutil.Normalize(domain)
	if err != nil {
		return model.Metric{}, err
	}
	select {
	case a.slot <- struct{}{}:
	case <-ctx.Done():
		return model.Metric{}, ctx.Err()
	}
	defer func() { <-a.slot }()
	target := a.cfg.BaseURL + "/cha/" + url.PathEscape(normalized) + "/"
	attempts := a.cfg.Retries
	if a.agent != nil {
		// One Agent request, then at most one master recheck.
		attempts = 2
	}
	var agentErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if err := a.WaitReady(ctx); err != nil {
			return model.Metric{}, err
		}
		route := "direct"
		if a.agent != nil {
			route = "agent:" + a.agent.endpoint
		}
		if agentErr != nil {
			route = "direct:master-recheck"
		}
		a.logger.Info("Aizhan request started", "domain", normalized, "route", route, "attempt", attempt+1)
		var body []byte
		var retry bool
		var retryAfter time.Time
		if agentErr != nil {
			body, retry, retryAfter, err = a.fetchDirect(ctx, target)
		} else {
			body, retry, retryAfter, err = a.fetchOnce(ctx, target)
		}
		a.logger.Info("Aizhan response received", "domain", normalized, "route", route, "bytes", len(body), "error", err)
		a.delay() // Delay after completion also limits slow concurrent requests.
		if ctx.Err() != nil {
			return model.Metric{}, ctx.Err()
		}
		if err == nil {
			var metric model.Metric
			metric, err = ParseAizhan(body, normalized)
			if err != nil && challengePage(body) {
				err = errSourceChallenge
			}
			if err == nil {
				hash := sha256.Sum256(body)
				metric.Domain = normalized
				metric.CollectionRoute = route
				metric.SourceURL, metric.RawSHA256 = target, hex.EncodeToString(hash[:])
				metric.CollectedAt = time.Now().UTC()
				a.mu.Lock()
				a.failures = 0
				a.cooldown = time.Time{}
				a.mu.Unlock()
				return metric, nil
			}
		}
		if retryAfter.After(time.Now()) && (a.agent == nil || agentErr != nil) {
			err = fmt.Errorf("%v: %w", err, &sourceHTTPError{retryAfter: retryAfter})
		}
		// Respect explicit source blocking instead of switching exit IPs during
		// its cooldown. Ordinary request/validation failures get one recheck.
		if a.agent != nil && agentErr == nil && !sourceBlocked(err) {
			agentErr = err
			a.logger.Warn("Aizhan Agent failed; master recheck scheduled", "domain", normalized, "error", err)
			continue
		}
		if agentErr != nil {
			err = fmt.Errorf("Agent failed: %w; master recheck failed: %w", agentErr, err)
		}
		if !retry || attempt+1 == attempts {
			if !sourceBlocked(err) {
				return model.Metric{}, fmt.Errorf("Aizhan collection failed: %w", err)
			}
			a.fail(retryAfter)
			a.mu.Lock()
			resume := a.cooldown
			a.mu.Unlock()
			return model.Metric{}, fmt.Errorf("Aizhan collection failed (source cooldown until %s): %w", resume.UTC().Format(time.RFC3339), err)
		}
	}
	return model.Metric{}, errors.New("Aizhan collection failed")
}

func (a *Aizhan) fetchOnce(ctx context.Context, target string) ([]byte, bool, time.Time, error) {
	if a.agent != nil {
		return a.agent.fetch(ctx, target)
	}
	return a.fetchDirect(ctx, target)
}

func (a *Aizhan) fetchDirect(ctx context.Context, target string) ([]byte, bool, time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, false, time.Time{}, err
	}
	req.Header.Set("User-Agent", a.cfg.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, true, time.Time{}, err
	}
	defer resp.Body.Close()
	after := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	if resp.StatusCode != http.StatusOK {
		// 403/429 and explicit Retry-After immediately cool down the whole source.
		retry := resp.StatusCode >= 500 && after.IsZero()
		return nil, retry, after, &sourceHTTPError{status: resp.StatusCode, retryAfter: after}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, a.cfg.MaxResponseBytes+1))
	if err != nil {
		return nil, true, after, err
	}
	if int64(len(body)) > a.cfg.MaxResponseBytes {
		return nil, false, after, errors.New("Aizhan response too large")
	}
	if len(body) == 0 {
		return nil, false, after, errors.New("Aizhan returned HTTP 200 with an empty body")
	}
	return body, false, after, nil
}

func parseRetryAfter(value string, now time.Time) time.Time {
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && seconds >= 0 {
		// Avoid duration overflow from a malformed upstream header.
		if seconds > 7*24*60*60 {
			seconds = 7 * 24 * 60 * 60
		}
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return date
	}
	return time.Time{}
}

var aizhanRankImage = regexp.MustCompile(`/(?:br|mbr|sr|360|bing|sm|pr)/(10|[0-9])\.png$`)
var aizhanNumber = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)(万|亿)?$`)
var aizhanTraffic = regexp.MustCompile(`^([0-9][0-9,.]*(?:万|亿)?)(?:\s*[~～-]\s*([0-9][0-9,.]*(?:万|亿)?))?$`)
var aizhanAge = regexp.MustCompile(`^(?:(\d+)年)?(?:(\d+)月)?(?:(\d+)日)?$`)

// ParseAizhan reads result IDs observed in actual /cha/ HTML (2026-09-12).
// Navigation links and commented-out rank badges must never become metrics.
func ParseAizhan(body []byte, domain string) (model.Metric, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return model.Metric{}, err
	}
	actual, _ := doc.Find("input#domain").Attr("value")
	if !strings.EqualFold(strings.TrimSuffix(strings.TrimSpace(actual), "."), domain) {
		return model.Metric{}, errors.New("Aizhan result domain missing or mismatched; possible challenge/redirect")
	}
	m := model.Metric{
		BaiduPCWeight: aizhanRank(doc, "baidurank_br"), BaiduMobile: aizhanRank(doc, "baidurank_mbr"),
		SogouWeight: aizhanRank(doc, "sogou_pr"), So360Weight: aizhanRank(doc, "360_pr"),
		BingWeight: aizhanRank(doc, "bing_pr"), ShenmaWeight: aizhanRank(doc, "sm_pr"),
		PRWeight: aizhanRank(doc, "google_pr"), BacklinkCount: aizhanCount(doc.Find("#backlink").Text()),
	}
	// Both Baidu badges are required. A partial/loading page is not a successful
	// daily snapshot, even if it contains a PR icon or an old traffic estimate.
	if m.BaiduPCWeight == nil || m.BaiduMobile == nil {
		return model.Metric{}, errors.New("Aizhan Baidu weights incomplete; possible empty result, challenge or layout change")
	}
	traffic := normalizeSpace(doc.Find("#baidurank_ip").Text())
	parts := aizhanTraffic.FindStringSubmatch(traffic)
	if len(parts) == 3 {
		if parts[2] == "" {
			parts[2] = parts[1]
		}
		lo, hi := aizhanCount(parts[1]), aizhanCount(parts[2])
		if lo != nil && hi != nil && *lo <= *hi {
			m.TrafficText, m.TrafficMin, m.TrafficMax = &traffic, lo, hi
		}
	}
	age := strings.TrimSpace(doc.Find("#whois_created span").First().Text())
	if match := aizhanAge.FindStringSubmatch(age); age != "" && len(match) == 4 {
		m.DomainAgeText = &age
		days := int(float64(atoi(match[1]))*365.2425 + float64(atoi(match[2]))*30.436875 + float64(atoi(match[3])))
		m.DomainAgeDays = &days // Approximation, matching the existing Chinaz field.
	}
	return m, nil
}

func aizhanRank(doc *goquery.Document, id string) *int16 {
	src, ok := doc.Find(`[id="` + id + `"] img`).First().Attr("src")
	if !ok {
		return nil
	}
	u, err := url.Parse(src)
	if err != nil {
		return nil
	}
	matches := aizhanRankImage.FindStringSubmatch(u.Path)
	if len(matches) != 2 {
		return nil
	}
	n := int16(atoi(matches[1]))
	return &n
}

func aizhanCount(text string) *int64 {
	text = strings.ReplaceAll(strings.TrimSpace(text), ",", "")
	m := aizhanNumber.FindStringSubmatch(text)
	if len(m) != 3 {
		return nil
	}
	if m[2] == "" {
		n, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return nil
		}
		return &n
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return nil
	}
	if m[2] == "万" {
		n *= 1e4
	} else {
		n *= 1e8
	}
	if n >= 9223372036854775808.0 {
		return nil
	}
	v := int64(n)
	return &v
}
