package scraper

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"

	"seo-monitor/internal/model"
)

var (
	numberPattern     = regexp.MustCompile(`[0-9][0-9,]*`)
	imageRankPattern  = regexp.MustCompile(`(?i)(?:baidu|sogou|bing|360|shenma|pr)([0-9]+)\.png`)
	domainInfoPattern = regexp.MustCompile(`注册人/机构：\s*(.*?)\s*注册人邮箱：\s*(.*?)\s*域名年龄：\s*(.*)$`)
	agePattern        = regexp.MustCompile(`(?:(\d+)年)?(?:(\d+)个月)?(?:(\d+)天)?`)
	expiryPattern     = regexp.MustCompile(`过期时间为\s*(\d{4})年(\d{1,2})月(\d{1,2})日`)
	spacePattern      = regexp.MustCompile(`\s+`)
)

type Config struct {
	BaseURL          string
	UserAgent        string
	Timeout          time.Duration
	MinDelay         time.Duration
	MaxDelay         time.Duration
	Retries          int
	MaxResponseBytes int64
}

type Chinaz struct {
	baseURL          string
	userAgent        string
	client           *http.Client
	minDelay         time.Duration
	maxDelay         time.Duration
	retries          int
	maxResponseBytes int64
	rateMu           sync.Mutex
	nextRequest      time.Time
}

func NewChinaz(cfg Config) (*Chinaz, error) {
	base, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("invalid source base URL")
	}
	if cfg.Retries < 1 {
		cfg.Retries = 1
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = 3 * 1024 * 1024
	}
	return &Chinaz{
		baseURL:          base.String(),
		userAgent:        cfg.UserAgent,
		client:           &http.Client{Timeout: cfg.Timeout},
		minDelay:         cfg.MinDelay,
		maxDelay:         cfg.MaxDelay,
		retries:          cfg.Retries,
		maxResponseBytes: cfg.MaxResponseBytes,
	}, nil
}

func (c *Chinaz) Fetch(ctx context.Context, domain string) (model.Metric, error) {
	target := c.baseURL + "/" + url.PathEscape(domain)
	var lastErr error
	for attempt := 1; attempt <= c.retries; attempt++ {
		if err := c.waitForSlot(ctx); err != nil {
			return model.Metric{}, err
		}
		body, retry, err := c.fetchOnce(ctx, target)
		if err == nil {
			metric, parseErr := Parse(body)
			if parseErr != nil {
				return model.Metric{}, parseErr
			}
			metric.SourceURL = target
			sum := sha256.Sum256(body)
			metric.RawSHA256 = hex.EncodeToString(sum[:])
			metric.CollectedAt = time.Now().UTC()
			return metric, nil
		}
		lastErr = err
		if !retry || attempt == c.retries {
			break
		}
		backoff := time.Duration(attempt*attempt) * time.Second
		select {
		case <-ctx.Done():
			return model.Metric{}, ctx.Err()
		case <-time.After(backoff):
		}
	}
	return model.Metric{}, fmt.Errorf("采集失败: %w", lastErr)
}

func (c *Chinaz) fetchOnce(ctx context.Context, target string) ([]byte, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.6")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, true, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		retry := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		return nil, retry, fmt.Errorf("source returned HTTP %d", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, c.maxResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, true, err
	}
	if int64(len(body)) > c.maxResponseBytes {
		return nil, false, fmt.Errorf("source response exceeds %d bytes", c.maxResponseBytes)
	}
	return body, false, nil
}

func (c *Chinaz) waitForSlot(ctx context.Context) error {
	c.rateMu.Lock()
	now := time.Now()
	wait := time.Duration(0)
	if c.nextRequest.After(now) {
		wait = c.nextRequest.Sub(now)
	}
	delay := c.minDelay
	if c.maxDelay > c.minDelay {
		delay += time.Duration(rand.Int64N(int64(c.maxDelay-c.minDelay) + 1))
	}
	c.nextRequest = now.Add(wait).Add(delay)
	c.rateMu.Unlock()

	if wait <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}

// Parse extracts only the result table requested by the user. It deliberately
// fails when the table is absent so CAPTCHA/challenge pages are never stored as
// successful zero-value snapshots.
func Parse(body []byte) (model.Metric, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return model.Metric{}, fmt.Errorf("parse HTML: %w", err)
	}
	table := doc.Find("table._chinaz-seo-newt").First()
	if table.Length() == 0 {
		return model.Metric{}, errors.New("未找到 SEO 结果表，页面可能被限流、需要验证码或结构已变化")
	}

	metric := model.Metric{}
	trafficText := normalizeSpace(table.Find("span.webuv a").First().Text())
	metric.TrafficText = textPointer(trafficText)
	metric.TrafficMin, metric.TrafficMax = parseRange(trafficText)
	metric.BaiduPCWeight = parseRank(table.Find(".baidupcrank img").First())
	metric.BaiduMobile = parseRank(table.Find(".baidumobilerank img").First())
	metric.SogouWeight = parseRank(table.Find(".sogoupcrank img").First())
	metric.BingWeight = parseRank(table.Find(".bingrank img").First())
	metric.So360Weight = parseRank(table.Find(".haosoupcrank img").First())
	metric.ShenmaWeight = parseRank(table.Find(".smrank img").First())
	metric.PRWeight = parseRank(table.Find(".apppcpr img").First())
	metric.APPPCPCrank = parseInt(table.Find(".apppcarank").First().Text())
	metric.SiteCategory = textPointer(table.Find(".webrank i.color-63").First().Text())
	metric.BacklinkCount = parseInt(table.Find(".apppcreslink").First().Text())

	table.Find("tr").EachWithBreak(func(_ int, row *goquery.Selection) bool {
		cells := row.Find("td")
		if cells.Length() < 2 || !strings.Contains(normalizeSpace(cells.First().Text()), "域名信息") {
			return true
		}
		text := normalizeSpace(cells.Eq(1).Text())
		matches := domainInfoPattern.FindStringSubmatch(text)
		if len(matches) == 4 {
			metric.RegistrantName = textPointer(matches[1])
			metric.RegistrantEmail = textPointer(matches[2])
			parseAge(strings.TrimSpace(matches[3]), &metric)
		}
		return false
	})

	if metric.TrafficMin == nil && metric.BaiduPCWeight == nil && metric.APPPCPCrank == nil && metric.DomainAgeText == nil {
		return model.Metric{}, errors.New("SEO 结果表不含可识别数据，页面结构可能已变化")
	}
	return metric, nil
}

func parseRank(selection *goquery.Selection) *int16 {
	if selection.Length() == 0 {
		return nil
	}
	if rank, ok := selection.Attr("data-rank"); ok {
		if value, err := strconv.ParseInt(strings.TrimSpace(rank), 10, 16); err == nil {
			out := int16(value)
			return &out
		}
	}
	src, _ := selection.Attr("src")
	base := path.Base(src)
	matches := imageRankPattern.FindStringSubmatch(base)
	if len(matches) != 2 {
		return nil
	}
	value, err := strconv.ParseInt(matches[1], 10, 16)
	if err != nil {
		return nil
	}
	out := int16(value)
	return &out
}

func parseRange(text string) (*int64, *int64) {
	matches := numberPattern.FindAllString(text, -1)
	if len(matches) == 0 {
		return nil, nil
	}
	min := parseInt(matches[0])
	if len(matches) == 1 {
		return min, min
	}
	return min, parseInt(matches[1])
}

func parseInt(text string) *int64 {
	match := numberPattern.FindString(text)
	if match == "" {
		return nil
	}
	value, err := strconv.ParseInt(strings.ReplaceAll(match, ",", ""), 10, 64)
	if err != nil {
		return nil
	}
	return &value
}

func parseAge(text string, metric *model.Metric) {
	ageText := text
	if index := strings.IndexAny(ageText, "（("); index >= 0 {
		ageText = strings.TrimSpace(ageText[:index])
	}
	metric.DomainAgeText = textPointer(ageText)

	age := agePattern.FindStringSubmatch(ageText)
	if len(age) == 4 && age[0] != "" {
		years := atoi(age[1])
		months := atoi(age[2])
		days := atoi(age[3])
		total := int(float64(years)*365.2425 + float64(months)*30.436875 + float64(days))
		metric.DomainAgeDays = &total
	}

	expiry := expiryPattern.FindStringSubmatch(text)
	if len(expiry) == 4 {
		date := time.Date(atoi(expiry[1]), time.Month(atoi(expiry[2])), atoi(expiry[3]), 0, 0, 0, 0, time.UTC)
		metric.ExpiresOn = &date
	}
}

func atoi(value string) int {
	parsed, _ := strconv.Atoi(value)
	return parsed
}

func textPointer(text string) *string {
	value := strings.TrimSpace(text)
	if value == "" || value == "-" || value == "--" {
		return nil
	}
	return &value
}

func normalizeSpace(text string) string {
	return strings.TrimSpace(spacePattern.ReplaceAllString(text, " "))
}
