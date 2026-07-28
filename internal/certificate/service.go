package certificate

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"seo-monitor/internal/collector"
	"seo-monitor/internal/model"
	"seo-monitor/internal/store"
)

type Service struct {
	ctx           context.Context
	store         *store.Store
	checker       Checker
	workers       int
	location      *time.Location
	retentionDays int
	logger        *slog.Logger
	refreshing    atomic.Bool
	progressMu    sync.RWMutex
	progress      model.TaskProgress
}

func NewService(ctx context.Context, st *store.Store, checker Checker, workers int, location *time.Location, retentionDays int, logger *slog.Logger) *Service {
	return &Service{
		ctx: ctx, store: st, checker: checker, workers: workers,
		location: location, retentionDays: retentionDays, logger: logger,
	}
}

// RefreshAsync starts a full certificate scan unless one is already running.
// It returns true only when a new scan was started.
func (s *Service) RefreshAsync() bool {
	if !s.refreshing.CompareAndSwap(false, true) {
		return false
	}
	startedAt := time.Now().UTC()
	s.progressMu.Lock()
	s.progress = model.TaskProgress{Running: true, StartedAt: &startedAt}
	s.progressMu.Unlock()
	go func() {
		defer s.refreshing.Store(false)
		defer s.finishProgress()
		s.refreshAll(s.ctx)
	}()
	return true
}

// Progress returns a race-free snapshot for API polling.
func (s *Service) Progress() model.TaskProgress {
	s.progressMu.RLock()
	defer s.progressMu.RUnlock()
	return s.progress
}

func (s *Service) refreshAll(ctx context.Context) {
	domains, err := s.store.ListDomains(ctx, false)
	if err != nil {
		s.logger.Error("list domains for certificate refresh", "error", err)
		return
	}
	s.progressMu.Lock()
	s.progress.Total = int64(len(domains))
	s.progressMu.Unlock()
	startedAt := time.Now()
	jobs := make(chan model.Domain)
	var waitGroup sync.WaitGroup
	var succeeded atomic.Int64
	var failed atomic.Int64

	for workerID := 1; workerID <= s.workers; workerID++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for domain := range jobs {
				item, checkErr := s.checker.Check(ctx, domain.Domain)
				progressSucceeded := checkErr == nil
				item.DomainID = domain.ID
				item.Domain = domain.Domain
				if item.CheckedAt.IsZero() {
					item.CheckedAt = time.Now().UTC()
				}
				item.CheckDate = collector.SnapshotDate(item.CheckedAt, s.location)
				if checkErr != nil {
					message := strings.TrimSpace(checkErr.Error())
					if len(message) > 2000 {
						message = message[:2000]
					}
					item.ErrorMessage = &message
					failed.Add(1)
				} else {
					succeeded.Add(1)
				}
				if saveErr := s.store.SaveCertificate(ctx, item); saveErr != nil {
					s.logger.Error("save domain certificate", "domain", domain.Domain, "error", saveErr)
					progressSucceeded = false
				}
				s.recordProgress(progressSucceeded)
			}
		}()
	}

enqueue:
	for _, domain := range domains {
		select {
		case <-ctx.Done():
			break enqueue
		case jobs <- domain:
		}
	}
	close(jobs)
	waitGroup.Wait()
	cutoff := collector.RetentionCutoff(time.Now(), s.location, s.retentionDays)
	deleted, cleanupErr := s.store.CleanupCertificateHistory(ctx, cutoff, s.retentionDays)
	if cleanupErr != nil {
		s.logger.Error("cleanup certificate polling history", "cutoff", cutoff.Format("2006-01-02"),
			"retention_days", s.retentionDays, "error", cleanupErr)
	} else {
		s.logger.Info("certificate polling history cleaned", "cutoff", cutoff.Format("2006-01-02"),
			"retention_days", s.retentionDays, "deleted", deleted)
	}
	s.logger.Info("certificate refresh completed", "domains", len(domains), "succeeded", succeeded.Load(),
		"failed", failed.Load(), "duration", time.Since(startedAt))
}

func (s *Service) recordProgress(succeeded bool) {
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	s.progress.Completed++
	if succeeded {
		s.progress.Succeeded++
		return
	}
	s.progress.Failed++
}

func (s *Service) finishProgress() {
	finishedAt := time.Now().UTC()
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	s.progress.Running = false
	s.progress.FinishedAt = &finishedAt
}
