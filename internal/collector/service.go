package collector

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"seo-monitor/internal/model"
	"seo-monitor/internal/store"
)

type Scraper interface {
	Fetch(context.Context, string) (model.Metric, error)
}

type Service struct {
	store        *store.Store
	scraper      Scraper
	workers      int
	pollInterval time.Duration
	logger       *slog.Logger
}

func New(st *store.Store, scraper Scraper, workers int, pollInterval time.Duration, logger *slog.Logger) *Service {
	return &Service{store: st, scraper: scraper, workers: workers, pollInterval: pollInterval, logger: logger}
}

func (s *Service) Start(ctx context.Context) {
	for workerID := 1; workerID <= s.workers; workerID++ {
		go s.runWorker(ctx, workerID)
	}
}

func (s *Service) runWorker(ctx context.Context, workerID int) {
	s.logger.Info("collector worker started", "worker", workerID)
	for {
		if ctx.Err() != nil {
			return
		}
		job, err := s.store.ClaimNextJob(ctx)
		if errors.Is(err, store.ErrNotFound) {
			if !wait(ctx, s.pollInterval) {
				return
			}
			continue
		}
		if err != nil {
			s.logger.Error("claim collection job", "worker", workerID, "error", err)
			if !wait(ctx, s.pollInterval) {
				return
			}
			continue
		}

		s.logger.Info("collecting domain", "worker", workerID, "job_id", job.ID.Hex(), "domain", job.Domain)
		metric, err := s.scraper.Fetch(ctx, job.Domain)
		if err != nil {
			s.logger.Warn("domain collection failed", "job_id", job.ID.Hex(), "domain", job.Domain, "error", err)
			if markErr := s.store.MarkJobFailed(ctx, job.ID, err); markErr != nil {
				s.logger.Error("mark job failed", "job_id", job.ID.Hex(), "error", markErr)
			}
			continue
		}
		if err := s.store.SaveJobResult(ctx, job, metric); err != nil {
			s.logger.Error("save domain metric", "job_id", job.ID.Hex(), "domain", job.Domain, "error", err)
			if markErr := s.store.MarkJobFailed(ctx, job.ID, err); markErr != nil {
				s.logger.Error("mark save failure", "job_id", job.ID.Hex(), "error", markErr)
			}
			continue
		}
		s.logger.Info("domain collection succeeded", "job_id", job.ID.Hex(), "domain", job.Domain)
	}
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// SnapshotDate stores a calendar date as UTC midnight. The scheduling timezone
// chooses the date, while the BSON value remains stable and easy to query.
func SnapshotDate(now time.Time, location *time.Location) time.Time {
	year, month, day := now.In(location).Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// RetentionCutoff returns the oldest snapshot date that must be retained.
// Cleanup deletes dates strictly before this value, so retentionDays=60 keeps
// today plus the preceding 60 calendar dates.
func RetentionCutoff(now time.Time, location *time.Location, retentionDays int) time.Time {
	return SnapshotDate(now, location).AddDate(0, 0, -retentionDays)
}
