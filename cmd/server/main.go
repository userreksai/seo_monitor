package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/robfig/cron/v3"

	"seo-monitor/internal/certificate"
	"seo-monitor/internal/collector"
	"seo-monitor/internal/config"
	"seo-monitor/internal/domainfile"
	"seo-monitor/internal/httpapi"
	"seo-monitor/internal/scraper"
	"seo-monitor/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	location, _ := time.LoadLocation(cfg.SnapshotTimezone)

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	connectCtx, cancel := context.WithTimeout(rootCtx, 15*time.Second)
	st, err := store.New(connectCtx, cfg.MongoDBURI, cfg.MongoDBDatabase)
	cancel()
	if err != nil {
		logger.Error("database startup failed", "error", err)
		os.Exit(1)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := st.Close(ctx); err != nil {
			logger.Error("close MongoDB", "error", err)
		}
	}()

	if cfg.EnsureIndexes {
		ctx, cancel := context.WithTimeout(rootCtx, 30*time.Second)
		err = st.EnsureIndexes(ctx)
		cancel()
		if err != nil {
			logger.Error("ensure MongoDB indexes", "error", err)
			os.Exit(1)
		}
	}

	cleanupExpiredData := func(trigger string) {
		cutoff := collector.RetentionCutoff(time.Now(), location, cfg.RetentionDays)
		ctx, cancel := context.WithTimeout(rootCtx, 2*time.Minute)
		cleaned, cleanupErr := st.CleanupBefore(ctx, cutoff)
		cancel()
		if cleanupErr != nil {
			logger.Error("clean expired MongoDB records", "trigger", trigger, "retention_days", cfg.RetentionDays,
				"cutoff", cutoff.Format("2006-01-02"), "metrics_deleted", cleaned.MetricsDeleted,
				"jobs_deleted", cleaned.JobsDeleted, "error", cleanupErr)
			return
		}
		logger.Info("expired MongoDB records cleaned", "trigger", trigger, "retention_days", cfg.RetentionDays,
			"cutoff", cutoff.Format("2006-01-02"), "metrics_deleted", cleaned.MetricsDeleted,
			"jobs_deleted", cleaned.JobsDeleted)
	}
	cleanupExpiredData("startup")

	authCtx, authCancel := context.WithTimeout(rootCtx, 30*time.Second)
	err = st.InitializeAuth(authCtx, cfg.DefaultAdminUsername, cfg.DefaultAdminPassword)
	authCancel()
	if err != nil {
		logger.Error("initialize authentication data", "error", err)
		os.Exit(1)
	}
	logger.Info("authentication data ready", "default_username", cfg.DefaultAdminUsername)
	if cfg.DefaultAdminUsername == "admin" && cfg.DefaultAdminPassword == "admin1818" {
		logger.Warn("insecure default administrator credentials are configured; change them before public deployment")
	}

	syncDomainsFile := func() error {
		domains, loadErr := domainfile.Load(cfg.DomainsFile)
		if errors.Is(loadErr, os.ErrNotExist) {
			logger.Warn("domains file not found; continuing with database domains", "file", cfg.DomainsFile)
			return nil
		}
		if loadErr != nil {
			return loadErr
		}

		created := 0
		existing := 0
		for _, domain := range domains {
			wasCreated, createErr := st.ActivateMetricDomain(rootCtx, domain)
			if createErr != nil {
				return createErr
			}
			if wasCreated {
				created++
			} else {
				existing++
			}
		}
		logger.Info("domains file synchronized", "file", cfg.DomainsFile, "total", len(domains), "created", created, "existing", existing)
		return nil
	}
	if err := syncDomainsFile(); err != nil {
		logger.Error("synchronize domains file", "file", cfg.DomainsFile, "error", err)
		os.Exit(1)
	}
	syncCertificateDomainsFile := func() error {
		domains, loadErr := domainfile.Load(cfg.CertificateDomainsFile)
		if loadErr != nil {
			return loadErr
		}
		if syncErr := st.SyncCertificateDomains(rootCtx, domains); syncErr != nil {
			return syncErr
		}
		logger.Info("certificate domains file synchronized", "file", cfg.CertificateDomainsFile, "total", len(domains))
		return nil
	}
	if err := syncCertificateDomainsFile(); err != nil {
		logger.Error("synchronize certificate domains file", "file", cfg.CertificateDomainsFile, "error", err)
		os.Exit(1)
	}

	if recovered, recoverErr := st.RecoverStaleJobs(rootCtx, cfg.StaleJobAfter); recoverErr != nil {
		logger.Error("recover stale jobs", "error", recoverErr)
	} else if recovered > 0 {
		logger.Info("recovered stale jobs", "count", recovered)
	}

	source, err := scraper.NewChinaz(scraper.Config{
		BaseURL: cfg.SourceBaseURL, DataBaseURL: cfg.SourceDataURL, UserAgent: cfg.UserAgent, Timeout: cfg.ScrapeTimeout,
		MinDelay: cfg.ScrapeMinDelay, MaxDelay: cfg.ScrapeMaxDelay, Retries: cfg.ScrapeRetries,
		MaxResponseBytes: cfg.MaxResponseBytes,
	})
	if err != nil {
		logger.Error("create scraper", "error", err)
		os.Exit(1)
	}
	workerService := collector.New(st, source, cfg.WorkerCount, cfg.JobPollInterval, logger)
	workerService.Start(rootCtx)
	certificateChecker, err := certificate.NewAgentFallbackChecker(
		certificate.NewTLSChecker(cfg.CertificateTimeout), cfg.CertificateAgentURLs,
		cfg.CertificateAgentToken, cfg.CertificateAgentTimeout, cfg.CertificateAgentConcurrency, logger,
	)
	if err != nil {
		logger.Error("create certificate checker", "error", err)
		os.Exit(1)
	}
	if len(cfg.CertificateAgentURLs) > 0 {
		logger.Info("certificate agent fallback enabled", "agents", len(cfg.CertificateAgentURLs),
			"timeout", cfg.CertificateAgentTimeout, "max_concurrent_per_agent", cfg.CertificateAgentConcurrency)
		if cfg.CertificateAgentToken == "" {
			logger.Warn("certificate Agent token is empty; configure CERTIFICATE_AGENT_TOKEN in production")
		}
	}
	certificateService := certificate.NewService(rootCtx, st,
		certificateChecker, cfg.CertificateWorkers, location, cfg.CertificateRetentionDays, cfg.CertificateDomainsFile, logger)
	certificateService.RefreshAsync()

	queueToday := func(requestedBy string) {
		if requestedBy == "scheduler" {
			cleanupExpiredData("scheduler")
			if syncErr := syncDomainsFile(); syncErr != nil {
				logger.Error("synchronize domains file before scheduled collection", "file", cfg.DomainsFile, "error", syncErr)
				return
			}
		}
		date := collector.SnapshotDate(time.Now(), location)
		count, queueErr := st.QueueAll(rootCtx, date, requestedBy, false)
		if queueErr != nil {
			logger.Error("queue daily collection", "requested_by", requestedBy, "error", queueErr)
			return
		}
		logger.Info("daily collection queued", "requested_by", requestedBy, "snapshot_date", date.Format("2006-01-02"), "count", count)
	}
	if cfg.QueueOnStart {
		queueToday("startup")
	}

	scheduler := cron.New(cron.WithLocation(location))
	if _, err := scheduler.AddFunc(cfg.CollectCron, func() { queueToday("scheduler") }); err != nil {
		logger.Error("invalid COLLECT_CRON", "value", cfg.CollectCron, "error", err)
		os.Exit(1)
	}
	if _, err := scheduler.AddFunc(cfg.CertificateCron, func() { certificateService.RefreshAsync() }); err != nil {
		logger.Error("invalid CERTIFICATE_CRON", "value", cfg.CertificateCron, "error", err)
		os.Exit(1)
	}
	scheduler.Start()
	defer scheduler.Stop()

	httpServer := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.New(st, certificateService, location, cfg.APIToken, cfg.AuthSessionTTL, cfg.AuthCookieSecure, httpapi.LoginProtectionConfig{
			IPMaxFailures: cfg.AuthLoginIPMaxFailures, PairMaxFailures: cfg.AuthLoginPairMaxFailures,
			FailureWindow: cfg.AuthLoginFailureWindow, Lockout: cfg.AuthLoginLockout,
			TrustedProxies: cfg.AuthTrustedProxyCIDRs,
		}, cfg.AllowedOrigins, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		logger.Info("HTTP server listening", "address", cfg.HTTPAddr, "database", cfg.MongoDBDatabase)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server failed", "error", err)
			stop()
		}
	}()

	<-rootCtx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP shutdown failed", "error", err)
	}
	logger.Info("shutdown complete")
}
