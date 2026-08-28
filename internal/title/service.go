package title

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"seo-monitor/internal/model"
	"seo-monitor/internal/store"
)

type Service struct {
	ctx        context.Context
	store      *store.Store
	checker    Checker
	workers    int
	logger     *slog.Logger
	refreshing atomic.Bool
	progressMu sync.RWMutex
	progress   model.TaskProgress
}

func NewService(ctx context.Context, st *store.Store, checker Checker, workers int, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{ctx: ctx, store: st, checker: checker, workers: workers, logger: logger}
}

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

func (s *Service) Progress() model.TaskProgress {
	s.progressMu.RLock()
	defer s.progressMu.RUnlock()
	return s.progress
}

func (s *Service) refreshAll(ctx context.Context) {
	domains, err := s.store.ListDomains(ctx, false)
	if err != nil {
		s.logger.Error("list domains for title refresh", "error", err)
		return
	}
	s.progressMu.Lock()
	s.progress.Total = int64(len(domains))
	s.progressMu.Unlock()

	startedAt := time.Now()
	jobs := make(chan model.Domain)
	var waitGroup sync.WaitGroup
	var changed atomic.Int64
	for workerID := 1; workerID <= s.workers; workerID++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for domain := range jobs {
				attemptedAt := time.Now().UTC()
				observation, checkErr := s.checker.Check(ctx, domain.Domain)
				if checkErr != nil {
					if saveErr := s.store.SaveDomainTitleFailure(ctx, domain, attemptedAt, checkErr); saveErr != nil {
						s.logger.Error("save title check failure", "domain", domain.Domain, "error", saveErr)
					}
					s.logger.Warn("domain title check failed", "domain", domain.Domain, "error", checkErr)
					s.recordProgress(false)
					continue
				}
				wasChanged, saveErr := s.store.SaveDomainTitle(ctx, domain, observation)
				if saveErr != nil {
					s.logger.Error("save domain title", "domain", domain.Domain, "error", saveErr)
					s.recordProgress(false)
					continue
				}
				if wasChanged {
					changed.Add(1)
					s.logger.Info("domain title changed", "domain", domain.Domain, "title", observation.Title)
				}
				s.recordProgress(true)
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
	progress := s.Progress()
	s.logger.Info("title refresh completed", "domains", len(domains), "succeeded", progress.Succeeded,
		"failed", progress.Failed, "changed", changed.Load(), "duration", time.Since(startedAt))
}

func (s *Service) recordProgress(succeeded bool) {
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	s.progress.Completed++
	if succeeded {
		s.progress.Succeeded++
	} else {
		s.progress.Failed++
	}
}

func (s *Service) finishProgress() {
	finishedAt := time.Now().UTC()
	s.progressMu.Lock()
	defer s.progressMu.Unlock()
	s.progress.Running = false
	s.progress.FinishedAt = &finishedAt
}
