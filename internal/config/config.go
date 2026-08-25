package config

import (
	"fmt"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	MongoDBURI                  string
	MongoDBDatabase             string
	DomainsFile                 string
	CertificateDomainsFile      string
	HTTPAddr                    string
	APIToken                    string
	DefaultAdminUsername        string
	DefaultAdminPassword        string
	AuthSessionTTL              time.Duration
	AuthCookieSecure            bool
	AuthLoginIPMaxFailures      int
	AuthLoginPairMaxFailures    int
	AuthLoginFailureWindow      time.Duration
	AuthLoginLockout            time.Duration
	AuthTrustedProxyCIDRs       []netip.Prefix
	AllowedOrigins              []string
	EnsureIndexes               bool
	SourceBaseURL               string
	SourceDataURL               string
	UserAgent                   string
	ScrapeTimeout               time.Duration
	ScrapeMinDelay              time.Duration
	ScrapeMaxDelay              time.Duration
	ScrapeRetries               int
	MaxResponseBytes            int64
	WorkerCount                 int
	JobPollInterval             time.Duration
	CollectionRetryDelays       []time.Duration
	StaleJobAfter               time.Duration
	RetentionDays               int
	CollectCron                 string
	SnapshotTimezone            string
	QueueOnStart                bool
	CertificateWorkers          int
	CertificateTimeout          time.Duration
	CertificateCron             string
	CertificateRetentionDays    int
	CertificateAgentURLs        []string
	CertificateAgentToken       string
	CertificateAgentTimeout     time.Duration
	CertificateAgentConcurrency int
}

func Load() (Config, error) {
	trustedProxyCIDRs, err := envPrefixes("AUTH_TRUSTED_PROXY_CIDRS", "127.0.0.1/32,::1/128")
	if err != nil {
		return Config{}, err
	}
	retryDelays, err := durationList(env("COLLECTION_RETRY_DELAYS", "10m,30m,1h"))
	if err != nil {
		return Config{}, fmt.Errorf("COLLECTION_RETRY_DELAYS 无效: %w", err)
	}
	cfg := Config{
		MongoDBURI:                  env("MONGODB_URI", "mongodb://localhost:27017"),
		MongoDBDatabase:             env("MONGODB_DATABASE", "seo_monitor"),
		DomainsFile:                 env("DOMAINS_FILE", "domains.json"),
		CertificateDomainsFile:      env("CERTIFICATE_DOMAINS_FILE", "certificate_domains.json"),
		HTTPAddr:                    env("HTTP_ADDR", "127.0.0.1:10001"),
		APIToken:                    os.Getenv("API_TOKEN"),
		DefaultAdminUsername:        env("DEFAULT_ADMIN_USERNAME", "admin"),
		DefaultAdminPassword:        strings.TrimSpace(os.Getenv("DEFAULT_ADMIN_PASSWORD")),
		AuthSessionTTL:              envDuration("AUTH_SESSION_TTL", 8*time.Hour),
		AuthCookieSecure:            envBool("AUTH_COOKIE_SECURE", true),
		AuthLoginIPMaxFailures:      envInt("AUTH_LOGIN_IP_MAX_FAILURES", 10),
		AuthLoginPairMaxFailures:    envInt("AUTH_LOGIN_PAIR_MAX_FAILURES", 5),
		AuthLoginFailureWindow:      envDuration("AUTH_LOGIN_FAILURE_WINDOW", 15*time.Minute),
		AuthLoginLockout:            envDuration("AUTH_LOGIN_LOCKOUT", 15*time.Minute),
		AuthTrustedProxyCIDRs:       trustedProxyCIDRs,
		AllowedOrigins:              splitCSV(os.Getenv("CORS_ALLOWED_ORIGINS")),
		EnsureIndexes:               envBool("ENSURE_INDEXES", true),
		SourceBaseURL:               env("SOURCE_BASE_URL", "https://seo.chinaz.com"),
		SourceDataURL:               env("SOURCE_DATA_URL", "https://othertool.chinaz.com"),
		UserAgent:                   env("SCRAPE_USER_AGENT", "seo-monitor/1.0 (daily metrics collector; contact your administrator)"),
		ScrapeTimeout:               envDuration("SCRAPE_TIMEOUT", 25*time.Second),
		ScrapeMinDelay:              envDuration("SCRAPE_MIN_DELAY", 3*time.Second),
		ScrapeMaxDelay:              envDuration("SCRAPE_MAX_DELAY", 8*time.Second),
		ScrapeRetries:               envInt("SCRAPE_RETRIES", 3),
		MaxResponseBytes:            int64(envInt("MAX_RESPONSE_BYTES", 3*1024*1024)),
		WorkerCount:                 envInt("WORKER_COUNT", 1),
		JobPollInterval:             envDuration("JOB_POLL_INTERVAL", 2*time.Second),
		CollectionRetryDelays:       retryDelays,
		StaleJobAfter:               envDuration("STALE_JOB_AFTER", 20*time.Minute),
		RetentionDays:               envInt("RETENTION_DAYS", 60),
		CollectCron:                 env("COLLECT_CRON", "15 2 * * *"),
		SnapshotTimezone:            env("SNAPSHOT_TIMEZONE", "Asia/Shanghai"),
		QueueOnStart:                envBool("QUEUE_ON_START", true),
		CertificateWorkers:          envInt("CERTIFICATE_WORKERS", 10),
		CertificateTimeout:          envDuration("CERTIFICATE_TIMEOUT", 8*time.Second),
		CertificateCron:             env("CERTIFICATE_CRON", "45 3 * * *"),
		CertificateRetentionDays:    envInt("CERTIFICATE_RETENTION_DAYS", 7),
		CertificateAgentURLs:        splitCSV(os.Getenv("CERTIFICATE_AGENT_URLS")),
		CertificateAgentToken:       strings.TrimSpace(os.Getenv("CERTIFICATE_AGENT_TOKEN")),
		CertificateAgentTimeout:     envDuration("CERTIFICATE_AGENT_TIMEOUT", 15*time.Second),
		CertificateAgentConcurrency: envInt("CERTIFICATE_AGENT_MAX_CONCURRENT", 4),
	}

	if cfg.WorkerCount < 1 || cfg.WorkerCount > 4 {
		return Config{}, fmt.Errorf("WORKER_COUNT 必须在 1 到 4 之间")
	}
	if strings.TrimSpace(cfg.DefaultAdminUsername) == "" {
		return Config{}, fmt.Errorf("DEFAULT_ADMIN_USERNAME 不能为空")
	}
	if len(cfg.DefaultAdminPassword) < 12 {
		return Config{}, fmt.Errorf("DEFAULT_ADMIN_PASSWORD 至少需要 12 字节")
	}
	if len(cfg.DefaultAdminPassword) > 72 {
		return Config{}, fmt.Errorf("DEFAULT_ADMIN_PASSWORD 不能超过 72 字节（bcrypt 限制）")
	}
	if cfg.APIToken != "" && len(cfg.APIToken) < 32 {
		return Config{}, fmt.Errorf("API_TOKEN 启用时至少需要 32 字节；不使用时请留空")
	}
	if cfg.AuthSessionTTL < 5*time.Minute || cfg.AuthSessionTTL > 30*24*time.Hour {
		return Config{}, fmt.Errorf("AUTH_SESSION_TTL 必须在 5 分钟到 30 天之间")
	}
	if cfg.AuthLoginIPMaxFailures < 1 || cfg.AuthLoginIPMaxFailures > 100 {
		return Config{}, fmt.Errorf("AUTH_LOGIN_IP_MAX_FAILURES 必须在 1 到 100 之间")
	}
	if cfg.AuthLoginPairMaxFailures < 1 || cfg.AuthLoginPairMaxFailures > cfg.AuthLoginIPMaxFailures {
		return Config{}, fmt.Errorf("AUTH_LOGIN_PAIR_MAX_FAILURES 必须在 1 到 AUTH_LOGIN_IP_MAX_FAILURES 之间")
	}
	if cfg.AuthLoginFailureWindow < time.Minute || cfg.AuthLoginFailureWindow > 24*time.Hour {
		return Config{}, fmt.Errorf("AUTH_LOGIN_FAILURE_WINDOW 必须在 1 分钟到 24 小时之间")
	}
	if cfg.AuthLoginLockout < time.Minute || cfg.AuthLoginLockout > 24*time.Hour {
		return Config{}, fmt.Errorf("AUTH_LOGIN_LOCKOUT 必须在 1 分钟到 24 小时之间")
	}
	if cfg.ScrapeRetries < 1 || cfg.ScrapeRetries > 10 {
		return Config{}, fmt.Errorf("SCRAPE_RETRIES 必须在 1 到 10 之间")
	}
	if cfg.ScrapeMinDelay < 0 || cfg.ScrapeMaxDelay < cfg.ScrapeMinDelay {
		return Config{}, fmt.Errorf("抓取延迟配置无效")
	}
	if len(cfg.CollectionRetryDelays) > 10 {
		return Config{}, fmt.Errorf("COLLECTION_RETRY_DELAYS 最多允许 10 个重试间隔")
	}
	if cfg.RetentionDays < 1 || cfg.RetentionDays > 3650 {
		return Config{}, fmt.Errorf("RETENTION_DAYS 必须在 1 到 3650 之间")
	}
	if cfg.CertificateWorkers < 1 || cfg.CertificateWorkers > 50 {
		return Config{}, fmt.Errorf("CERTIFICATE_WORKERS 必须在 1 到 50 之间")
	}
	if cfg.CertificateTimeout <= 0 || cfg.CertificateTimeout > time.Minute {
		return Config{}, fmt.Errorf("CERTIFICATE_TIMEOUT 必须大于 0 且不超过 1m")
	}
	if cfg.CertificateRetentionDays < 1 || cfg.CertificateRetentionDays > 365 {
		return Config{}, fmt.Errorf("CERTIFICATE_RETENTION_DAYS 必须在 1 到 365 之间")
	}
	if cfg.CertificateAgentTimeout <= 0 || cfg.CertificateAgentTimeout > time.Minute {
		return Config{}, fmt.Errorf("CERTIFICATE_AGENT_TIMEOUT 必须大于 0 且不超过 1m")
	}
	if cfg.CertificateAgentConcurrency < 1 || cfg.CertificateAgentConcurrency > 50 {
		return Config{}, fmt.Errorf("CERTIFICATE_AGENT_MAX_CONCURRENT 必须在 1 到 50 之间")
	}
	if _, err := time.LoadLocation(cfg.SnapshotTimezone); err != nil {
		return Config{}, fmt.Errorf("SNAPSHOT_TIMEZONE 无效: %w", err)
	}
	return cfg, nil
}

func durationList(value string) ([]time.Duration, error) {
	parts := strings.Split(value, ",")
	durations := make([]time.Duration, 0, len(parts))
	for _, part := range parts {
		duration, err := time.ParseDuration(strings.TrimSpace(part))
		if err != nil || duration <= 0 {
			return nil, fmt.Errorf("重试间隔 %q 必须是大于 0 的 Go duration", part)
		}
		durations = append(durations, duration)
	}
	return durations, nil
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func splitCSV(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func envPrefixes(key, fallback string) ([]netip.Prefix, error) {
	values := splitCSV(os.Getenv(key))
	if len(values) == 0 {
		values = splitCSV(fallback)
	}
	prefixes := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			address, addressErr := netip.ParseAddr(value)
			if addressErr != nil {
				return nil, fmt.Errorf("%s 包含无效的 IP/CIDR %q", key, value)
			}
			address = address.Unmap()
			prefix = netip.PrefixFrom(address, address.BitLen())
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}
