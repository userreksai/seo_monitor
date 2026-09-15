package collector

import (
	"context"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"io"
	"log/slog"
	"seo-monitor/internal/model"
	"seo-monitor/internal/store"
	"testing"
	"time"
)

type queuedStore struct {
	jobs    []model.CollectionJob
	saved   chan string
	retried chan string
}

func (s *queuedStore) ClaimNextJob(context.Context) (model.CollectionJob, error) {
	if len(s.jobs) == 0 {
		return model.CollectionJob{}, store.ErrNotFound
	}
	j := s.jobs[0]
	s.jobs = s.jobs[1:]
	return j, nil
}
func (s *queuedStore) SaveJobResult(_ context.Context, j model.CollectionJob, _ model.Metric) error {
	s.saved <- j.Domain
	return nil
}
func (s *queuedStore) RetryJob(_ context.Context, _ primitive.ObjectID, _ error, _ time.Time) error {
	s.retried <- "retry"
	return nil
}
func (s *queuedStore) MarkJobFailed(context.Context, primitive.ObjectID, error) error {
	panic("unexpected terminal failure")
}

type ordinaryFailureSource struct{}

func (ordinaryFailureSource) Fetch(_ context.Context, domain string) (model.Metric, error) {
	if domain == "timeout.com" {
		return model.Metric{}, context.DeadlineExceeded
	}
	return model.Metric{}, nil
}

type blockedSource struct{ entered chan struct{} }

func (s blockedSource) WaitReady(ctx context.Context) error {
	close(s.entered)
	<-ctx.Done()
	return ctx.Err()
}
func (blockedSource) Fetch(context.Context, string) (model.Metric, error) {
	panic("blocked source fetched")
}

func TestIndependentWorkersContinueWhileOtherSourceCools(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	blocked := blockedSource{entered: make(chan struct{})}
	blockedDone := make(chan struct{})
	go func() {
		defer close(blockedDone)
		New(nil, blocked, 1, time.Millisecond, nil, logger).runWorker(ctx, 1)
	}()
	<-blocked.entered
	st := &queuedStore{jobs: []model.CollectionJob{{Domain: "timeout.com", AttemptCount: 1}, {Domain: "success.com", AttemptCount: 1}}, saved: make(chan string, 1), retried: make(chan string, 1)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		New(st, ordinaryFailureSource{}, 1, time.Millisecond, []time.Duration{time.Minute}, logger).runWorker(ctx, 1)
	}()
	select {
	case <-st.retried:
	case <-time.After(time.Second):
		t.Fatal("timeout not retried")
	}
	select {
	case domain := <-st.saved:
		if domain != "success.com" {
			t.Fatal(domain)
		}
	case <-time.After(time.Second):
		t.Fatal("other source or domain blocked progress")
	}
	cancel()
	<-done
	<-blockedDone
}
