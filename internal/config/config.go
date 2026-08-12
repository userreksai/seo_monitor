package config

import (
	"fmt"
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
	cfg := Config{
		MongoDBURI:                  env("MONGODB_URI", "mongodb://localhost:27017"),
		MongoDBDatabase:             env("MONGODB_DATABASE", "seo_monitor"),
		DomainsFile:                 env("DOMAINS_FILE", "domains.json"),
		CertificateDomainsFile:      env("CERTIFICATE_DOMAINS_FILE", "certificate_domains.json"),
		HTTPAddr:                    env("HTTP_ADDR", "127.0.0.1:10001"),
		APIToken:                    os.Getenv("API_TOKEN"),
		DefaultAdminUsername:        env("DEFAULT_ADMIN_USERNAME", "admin"),
		DefaultAdminPassword:        env("DEFAULT_ADMIN_PASSWORD", "admin1818"),
		AuthSessionTTL:              envDuration("AUTH_SESSION_TTL", 24*time.Hour),
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
	if len(cfg.DefaultAdminPassword) < 8 {
		return Config{}, fmt.Errorf("DEFAULT_ADMIN_PASSWORD 至少需要 8 个字符")
	}
	if cfg.AuthSessionTTL < 5*time.Minute || cfg.AuthSessionTTL > 30*24*time.Hour {
		return Config{}, fmt.Errorf("AUTH_SESSION_TTL 必须在 5 分钟到 30 天之间")
	}
	if cfg.ScrapeRetries < 1 || cfg.ScrapeRetries > 10 {
		return Config{}, fmt.Errorf("SCRAPE_RETRIES 必须在 1 到 10 之间")
	}
	if cfg.ScrapeMinDelay < 0 || cfg.ScrapeMaxDelay < cfg.ScrapeMinDelay {
		return Config{}, fmt.Errorf("抓取延迟配置无效")
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
