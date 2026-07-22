package certificate

import (
	"context"
	"log/slog"
	"strings"
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
}

func NewService(ctx context.Context, st *store.Store, checker Checker, workers int, logger *slog.Logger) *Service {
	return &Service{ctx: ctx, store: st, checker: checker, workers: workers, logger: logger}
}

// RefreshAsync starts a full certificate scan unless one is already running.
// It returns true only when a new scan was started.
func (s *Service) RefreshAsync() bool {
	if !s.refreshing.CompareAndSwap(false, true) {
		return false
	}
	go func() {
		defer s.refreshing.Store(false)
		s.refreshAll(s.ctx)
	}()
	return true
}

func (s *Service) refreshAll(ctx context.Context) {
	domains, err := s.store.ListDomains(ctx, false)
	if err != nil {
		s.logger.Error("list domains for certificate refresh", "error", err)
		return
	}
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
				item.DomainID = domain.ID
				item.Domain = domain.Domain
				if item.CheckedAt.IsZero() {
					item.CheckedAt = time.Now().UTC()
				}
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
				}
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
	s.logger.Info("certificate refresh completed", "domains", len(domains), "succeeded", succeeded.Load(),
		"failed", failed.Load(), "duration", time.Since(startedAt))
}
