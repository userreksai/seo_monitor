package scraper

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	secretKeyPattern  = regexp.MustCompile(`(?m)\bvar\s+enkey\s*=\s*['"]([^'"]+)['"]`)
	domainInfoPattern = regexp.MustCompile(`注册人/机构：\s*(.*?)\s*注册人邮箱：\s*(.*?)\s*域名年龄：\s*(.*)$`)
	agePattern        = regexp.MustCompile(`(?:(\d+)年)?(?:(\d+)个月)?(?:(\d+)天)?`)
	expiryPattern     = regexp.MustCompile(`过期时间为\s*(\d{4})年(\d{1,2})月(\d{1,2})日`)
	spacePattern      = regexp.MustCompile(`\s+`)
)

type Config struct {
	BaseURL          string
	DataBaseURL      string
	UserAgent        string
	Timeout          time.Duration
	MinDelay         time.Duration
	MaxDelay         time.Duration
	Retries          int
	MaxResponseBytes int64
}

type Chinaz struct {
	baseURL          string
	dataBaseURL      string
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
	if strings.TrimSpace(cfg.DataBaseURL) == "" {
		cfg.DataBaseURL = "https://othertool.chinaz.com"
	}
	dataBase, err := url.Parse(strings.TrimRight(cfg.DataBaseURL, "/"))
	if err != nil || dataBase.Scheme == "" || dataBase.Host == "" {
		return nil, fmt.Errorf("invalid source data base URL")
	}
	if cfg.Retries < 1 {
		cfg.Retries = 1
	}
	if cfg.MaxResponseBytes <= 0 {
		cfg.MaxResponseBytes = 3 * 1024 * 1024
	}
	return &Chinaz{
		baseURL:          base.String(),
		dataBaseURL:      dataBase.String(),
		userAgent:        cfg.UserAgent,
		client:           &http.Client{Timeout: cfg.Timeout},
		minDelay:         cfg.MinDelay,
		maxDelay:         cfg.MaxDelay,
		retries:          cfg.Retries,
		maxResponseBytes: cfg.MaxResponseBytes,
	}, nil
}

func (c *Chinaz) Fetch(ctx context.Context, domain string) (model.Metric, error) {
	return c.fetchComplete(ctx, domain)
}

func (c *Chinaz) fetchComplete(ctx context.Context, domain string) (model.Metric, error) {
	pageURL := c.baseURL + "/" + url.PathEscape(domain)
	pageBody, err := c.fetchWithRetry(ctx, pageURL, "")
	if err != nil {
		return model.Metric{}, fmt.Errorf("fetch SEO page: %w", err)
	}
	metric, err := Parse(pageBody)
	if err != nil {
		return model.Metric{}, err
	}
	rawBodies := [][]byte{pageBody}
	if secretKey, keyErr := extractSecretKey(pageBody); keyErr == nil {
		if rankBody, fetchErr := c.fetchData(ctx, "/Rank.ashx", "rankdata", domain, secretKey, pageURL); fetchErr == nil {
			rawBodies = append(rawBodies, rankBody)
			// A valid SEO result page may legitimately have no rank result.
			// Keep the fields parsed from the page (or nil) in that case.
			_ = mergeRankResponse(rankBody, &metric)
		}

		if apppcBody, fetchErr := c.fetchData(ctx, "/SiteAPPAndPC.ashx", "", domain, secretKey, pageURL); fetchErr == nil {
			rawBodies = append(rawBodies, apppcBody)
			_ = mergeAPPPCResponse(apppcBody, &metric)
		}

		if categoryBody, fetchErr := c.fetchData(ctx, "/GetTopRanked.ashx", "GetSiteCategory", domain, secretKey, pageURL); fetchErr == nil {
			rawBodies = append(rawBodies, categoryBody)
			_ = mergeCategoryResponse(categoryBody, &metric)
		}
	}
	if err := ctx.Err(); err != nil {
		return model.Metric{}, err
	}

	metric.SourceURL = pageURL
	hasher := sha256.New()
	for _, raw := range rawBodies {
		_, _ = hasher.Write(raw)
		_, _ = hasher.Write([]byte{0})
	}
	metric.RawSHA256 = hex.EncodeToString(hasher.Sum(nil))
	metric.CollectedAt = time.Now().UTC()
	return metric, nil
}

func (c *Chinaz) fetchWithRetry(ctx context.Context, target, referer string) ([]byte, error) {
	var lastErr error
	for attempt := 1; attempt <= c.retries; attempt++ {
		if err := c.waitForSlot(ctx); err != nil {
			return nil, err
		}
		body, retry, err := c.fetchOnceWithReferer(ctx, target, referer)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if !retry || attempt == c.retries {
			break
		}
		backoff := time.Duration(attempt*attempt) * time.Second
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
	}
	return nil, fmt.Errorf("collection failed: %w", lastErr)
}

func (c *Chinaz) fetchOnceWithReferer(ctx context.Context, target, referer string) ([]byte, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.6")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}

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

func (c *Chinaz) fetchData(ctx context.Context, endpoint, action, domain, secretKey, referer string) ([]byte, error) {
	key, random := generateHostKey(domain)
	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	tokenBytes := md5.Sum([]byte(key + "Ch*z#N|a&i!O$" + timestamp))
	query := url.Values{
		"host":      {domain},
		"secretkey": {secretKey},
		"rd":        {strconv.Itoa(random)},
		"ts":        {timestamp},
		"token":     {hex.EncodeToString(tokenBytes[:])},
		"callback":  {"seoMonitorCallback"},
	}
	if action != "" {
		query.Set("action", action)
	}
	return c.fetchWithRetry(ctx, c.dataBaseURL+endpoint+"?"+query.Encode(), referer)
}

func generateHostKey(domain string) (string, int) {
	parts := strings.Split(domain, ".")
	for left, right := 0, len(parts)-1; left < right; left, right = left+1, right-1 {
		parts[left], parts[right] = parts[right], parts[left]
	}
	random := rand.IntN(900) + 100
	values := make([]string, 0, len(parts)+1)
	values = append(values, strconv.Itoa(random))
	for index, part := range parts {
		total := random
		for _, character := range part {
			total += int(character)
		}
		if index < len(parts)-1 {
			total += int('.')
		}
		values = append(values, strconv.Itoa(total))
	}
	return strings.Join(values, ","), random
}

func extractSecretKey(body []byte) (string, error) {
	matches := secretKeyPattern.FindSubmatch(body)
	if len(matches) != 2 || len(matches[1]) == 0 {
		return "", errors.New("SEO page does not contain the public data request key")
	}
	return string(matches[1]), nil
}

type rankDatum struct {
	Rank  int16 `json:"rank"`
	UVMin int64 `json:"uv_min"`
	UVMax int64 `json:"uv_max"`
}

type rankResponse struct {
	StateCode int `json:"StateCode"`
	Result    *struct {
		BaiduPC     rankDatum `json:"baiduPc"`
		BaiduMobile rankDatum `json:"baiduMobile"`
		SogouPC     rankDatum `json:"sogouPc"`
		Bing        rankDatum `json:"bing"`
		HaosouPC    rankDatum `json:"haosouPc"`
		Shenma      rankDatum `json:"shenma"`
	} `json:"Result"`
}

func mergeRankResponse(body []byte, metric *model.Metric) error {
	var response rankResponse
	if err := decodeJSONP(body, &response); err != nil {
		return fmt.Errorf("parse weight data: %w", err)
	}
	if response.StateCode == 0 || response.Result == nil {
		return errors.New("weight service returned no result")
	}
	result := response.Result
	metric.BaiduPCWeight = int16Pointer(result.BaiduPC.Rank)
	metric.BaiduMobile = int16Pointer(result.BaiduMobile.Rank)
	metric.SogouWeight = int16Pointer(result.SogouPC.Rank)
	metric.BingWeight = int16Pointer(result.Bing.Rank)
	metric.So360Weight = int16Pointer(result.HaosouPC.Rank)
	metric.ShenmaWeight = int16Pointer(result.Shenma.Rank)
	trafficMin := result.BaiduPC.UVMin + result.BaiduMobile.UVMin + result.SogouPC.UVMin + result.Bing.UVMin + result.HaosouPC.UVMin + result.Shenma.UVMin
	trafficMax := result.BaiduPC.UVMax + result.BaiduMobile.UVMax + result.SogouPC.UVMax + result.Bing.UVMax + result.HaosouPC.UVMax + result.Shenma.UVMax
	metric.TrafficMin = &trafficMin
	metric.TrafficMax = &trafficMax
	trafficText := formatInteger(trafficMin) + " ~ " + formatInteger(trafficMax)
	metric.TrafficText = &trafficText
	return nil
}

type apppcResponse struct {
	StateCode int `json:"StateCode"`
	Result    *struct {
		WeekRank json.RawMessage `json:"WeekRank"`
		PR       json.RawMessage `json:"Pr"`
		ResLink  string          `json:"ResLink"`
	} `json:"Result"`
}

func mergeAPPPCResponse(body []byte, metric *model.Metric) error {
	var response apppcResponse
	if err := decodeJSONP(body, &response); err != nil {
		return fmt.Errorf("parse APPPC data: %w", err)
	}
	// StateCode 0 is the normal "no APPPC ranking" response for many domains.
	if response.StateCode == 0 || response.Result == nil {
		return nil
	}
	if rank := parseJSONInteger(response.Result.WeekRank); rank != nil && *rank > 0 {
		metric.APPPCPCrank = rank
	}
	if pr := parseJSONInteger(response.Result.PR); pr != nil {
		value := int16(*pr)
		metric.PRWeight = &value
	}
	if response.Result.ResLink != "" && response.Result.ResLink != "[]" {
		var link struct {
			Count json.RawMessage `json:"link"`
		}
		if json.Unmarshal([]byte(response.Result.ResLink), &link) == nil {
			metric.BacklinkCount = parseJSONInteger(link.Count)
		}
	}
	return nil
}

type categoryResponse struct {
	StateCode int    `json:"StateCode"`
	Result    string `json:"Result"`
}

func mergeCategoryResponse(body []byte, metric *model.Metric) error {
	var response categoryResponse
	if err := decodeJSONP(body, &response); err != nil {
		return fmt.Errorf("parse site category: %w", err)
	}
	if response.StateCode != 0 {
		metric.SiteCategory = textPointer(response.Result)
	}
	return nil
}

func decodeJSONP(body []byte, target any) error {
	start := bytes.IndexByte(body, '{')
	end := bytes.LastIndexByte(body, '}')
	if start < 0 || end < start {
		return errors.New("invalid JSONP response")
	}
	return json.Unmarshal(body[start:end+1], target)
}

func parseJSONInteger(raw json.RawMessage) *int64 {
	value := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if value == "" || value == "null" || value == "--" {
		return nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return nil
	}
	return &parsed
}

func int16Pointer(value int16) *int16 {
	return &value
}

func formatInteger(value int64) string {
	digits := strconv.FormatInt(value, 10)
	for index := len(digits) - 3; index > 0; index -= 3 {
		digits = digits[:index] + "," + digits[index:]
	}
	return digits
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

// Parse extracts the SEO result table. The table itself is the validity marker:
// it rejects CAPTCHA/challenge pages, while an empty valid table is allowed so
// fetchComplete can populate traffic and weights from Chinaz's data endpoints.
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
