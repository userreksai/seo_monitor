package collector

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"seo-monitor/internal/model"
	"seo-monitor/internal/store"
)

type Scraper interface {
	Fetch(context.Context, string) (model.Metric, error)
}

type jobStore interface {
	ClaimNextJob(context.Context) (model.CollectionJob, error)
	SaveJobResult(context.Context, model.CollectionJob, model.Metric) error
	RetryJob(context.Context, primitive.ObjectID, error, time.Time) error
	MarkJobFailed(context.Context, primitive.ObjectID, error) error
	ReleaseInterruptedJob(context.Context, model.CollectionJob) error
	ForgetClaim(primitive.ObjectID)
}

type Service struct {
	wg           sync.WaitGroup
	store        jobStore
	scraper      Scraper
	workers      int
	pollInterval time.Duration
	retryDelays  []time.Duration
	logger       *slog.Logger
}

func New(st jobStore, scraper Scraper, workers int, pollInterval time.Duration, retryDelays []time.Duration, logger *slog.Logger) *Service {
	return &Service{store: st, scraper: scraper, workers: workers, pollInterval: pollInterval, retryDelays: retryDelays, logger: logger}
}

func (s *Service) Start(ctx context.Context) {
	for workerID := 1; workerID <= s.workers; workerID++ {
		s.wg.Add(1)
		go func() { defer s.wg.Done(); s.runWorker(ctx, workerID) }()
	}
}

func (s *Service) Wait() { s.wg.Wait() }

func (s *Service) runWorker(ctx context.Context, workerID int) {
	s.logger.Info("collector worker started", "worker", workerID)
	for {
		if ctx.Err() != nil {
			return
		}
		if gate, ok := s.scraper.(interface{ WaitReady(context.Context) error }); ok {
			if err := gate.WaitReady(ctx); err != nil {
				return
			}
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
		s.collectJob(ctx, job)
	}
}

func (s *Service) collectJob(ctx context.Context, job model.CollectionJob) {
	defer s.store.ForgetClaim(job.ID)
	defer func() {
		if ctx.Err() == nil {
			return
		}
		// The process context is canceled during systemctl restart. Use a short
		// independent context so the release reaches MongoDB before shutdown.
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := s.store.ReleaseInterruptedJob(releaseCtx, job); err != nil {
			s.logger.Error("release interrupted collection job", "job_id", job.ID.Hex(), "error", err)
		} else {
			s.logger.Info("interrupted collection job released", "job_id", job.ID.Hex(), "domain", job.Domain)
		}
	}()
	metric, err := s.scraper.Fetch(ctx, job.Domain)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		s.logger.Warn("domain collection failed", "job_id", job.ID.Hex(), "domain", job.Domain, "error", err)
		if delay, retry := retryDelay(job.AttemptCount, s.retryDelays); retry {
			availableAt := time.Now().UTC().Add(delay)
			if retryErr := s.store.RetryJob(ctx, job.ID, err, availableAt); retryErr != nil {
				s.logger.Error("schedule domain retry", "job_id", job.ID.Hex(), "error", retryErr)
			} else {
				s.logger.Info("domain retry scheduled", "job_id", job.ID.Hex(), "domain", job.Domain,
					"attempt", job.AttemptCount, "delay", delay, "available_at", availableAt)
			}
			return
		}
		if markErr := s.store.MarkJobFailed(ctx, job.ID, err); markErr != nil {
			s.logger.Error("mark job failed", "job_id", job.ID.Hex(), "error", markErr)
		}
		return
	}
	if err := s.store.SaveJobResult(ctx, job, metric); err != nil {
		if ctx.Err() != nil {
			return
		}
		s.logger.Error("save domain metric", "job_id", job.ID.Hex(), "domain", job.Domain, "error", err)
		if markErr := s.store.MarkJobFailed(ctx, job.ID, err); markErr != nil {
			s.logger.Error("mark save failure", "job_id", job.ID.Hex(), "error", markErr)
		}
		return
	}
	s.logger.Info("domain collection succeeded", "job_id", job.ID.Hex(), "domain", job.Domain)
}

func retryDelay(attempt int, delays []time.Duration) (time.Duration, bool) {
	if attempt < 1 || attempt > len(delays) {
		return 0, false
	}
	return delays[attempt-1], true
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
