package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	MongoDBURI       string
	MongoDBDatabase  string
	HTTPAddr         string
	APIToken         string
	AllowedOrigins   []string
	EnsureIndexes    bool
	SourceBaseURL    string
	UserAgent        string
	ScrapeTimeout    time.Duration
	ScrapeMinDelay   time.Duration
	ScrapeMaxDelay   time.Duration
	ScrapeRetries    int
	MaxResponseBytes int64
	WorkerCount      int
	JobPollInterval  time.Duration
	StaleJobAfter    time.Duration
	CollectCron      string
	SnapshotTimezone string
	QueueOnStart     bool
}

func Load() (Config, error) {
	cfg := Config{
		MongoDBURI:       env("MONGODB_URI", "mongodb://localhost:27017"),
		MongoDBDatabase:  env("MONGODB_DATABASE", "seo_monitor"),
		HTTPAddr:         env("HTTP_ADDR", "127.0.0.1:8080"),
		APIToken:         os.Getenv("API_TOKEN"),
		AllowedOrigins:   splitCSV(os.Getenv("CORS_ALLOWED_ORIGINS")),
		EnsureIndexes:    envBool("ENSURE_INDEXES", true),
		SourceBaseURL:    env("SOURCE_BASE_URL", "https://seo.chinaz.com"),
		UserAgent:        env("SCRAPE_USER_AGENT", "seo-monitor/1.0 (daily metrics collector; contact your administrator)"),
		ScrapeTimeout:    envDuration("SCRAPE_TIMEOUT", 25*time.Second),
		ScrapeMinDelay:   envDuration("SCRAPE_MIN_DELAY", 3*time.Second),
		ScrapeMaxDelay:   envDuration("SCRAPE_MAX_DELAY", 8*time.Second),
		ScrapeRetries:    envInt("SCRAPE_RETRIES", 3),
		MaxResponseBytes: int64(envInt("MAX_RESPONSE_BYTES", 3*1024*1024)),
		WorkerCount:      envInt("WORKER_COUNT", 1),
		JobPollInterval:  envDuration("JOB_POLL_INTERVAL", 2*time.Second),
		StaleJobAfter:    envDuration("STALE_JOB_AFTER", 20*time.Minute),
		CollectCron:      env("COLLECT_CRON", "15 2 * * *"),
		SnapshotTimezone: env("SNAPSHOT_TIMEZONE", "Asia/Shanghai"),
		QueueOnStart:     envBool("QUEUE_ON_START", true),
	}

	if cfg.WorkerCount < 1 || cfg.WorkerCount > 4 {
		return Config{}, fmt.Errorf("WORKER_COUNT 必须在 1 到 4 之间")
	}
	if cfg.ScrapeRetries < 1 || cfg.ScrapeRetries > 10 {
		return Config{}, fmt.Errorf("SCRAPE_RETRIES 必须在 1 到 10 之间")
	}
	if cfg.ScrapeMinDelay < 0 || cfg.ScrapeMaxDelay < cfg.ScrapeMinDelay {
		return Config{}, fmt.Errorf("抓取延迟配置无效")
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
