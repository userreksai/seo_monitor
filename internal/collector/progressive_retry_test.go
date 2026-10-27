package collector

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"seo-monitor/internal/model"
)

type retrySchedule struct {
	id          primitive.ObjectID
	cause       error
	availableAt time.Time
}

type retryLifecycleStore struct {
	retried   []retrySchedule
	failed    []error
	saved     []model.Metric
	released  []model.CollectionJob
	forgotten []primitive.ObjectID
}

func (*retryLifecycleStore) ClaimNextJob(context.Context) (model.CollectionJob, error) {
	panic("tests invoke one already-claimed job at a time")
}

func (s *retryLifecycleStore) RetryJob(_ context.Context, id primitive.ObjectID, cause error, availableAt time.Time) error {
	s.retried = append(s.retried, retrySchedule{id: id, cause: cause, availableAt: availableAt})
	return nil
}

func (s *retryLifecycleStore) MarkJobFailed(_ context.Context, _ primitive.ObjectID, cause error) error {
	s.failed = append(s.failed, cause)
	return nil
}

func (s *retryLifecycleStore) SaveJobResult(_ context.Context, _ model.CollectionJob, metric model.Metric) error {
	s.saved = append(s.saved, metric)
	return nil
}

func (s *retryLifecycleStore) ReleaseInterruptedJob(ctx context.Context, job model.CollectionJob) error {
	if ctx.Err() != nil {
		panic("release used canceled worker context")
	}
	if _, ok := ctx.Deadline(); !ok {
		panic("release query has no deadline")
	}
	s.released = append(s.released, job)
	return nil
}

func (s *retryLifecycleStore) ForgetClaim(id primitive.ObjectID) {
	s.forgotten = append(s.forgotten, id)
}

type attemptRecordingSource struct {
	attempts []int
	domains  []string
	fetch    func(context.Context, string, int) (model.Metric, error)
}

func (*attemptRecordingSource) Fetch(context.Context, string) (model.Metric, error) {
	panic("must use persisted attempt count for progressive retries")
}

func (s *attemptRecordingSource) FetchAttempt(ctx context.Context, domain string, attempt int) (model.Metric, error) {
	s.attempts = append(s.attempts, attempt)
	s.domains = append(s.domains, domain)
	return s.fetch(ctx, domain, attempt)
}

func newRetryLifecycleService(st *retryLifecycleStore, source Scraper, delays []time.Duration) *Service {
	return New(st, source, 1, time.Millisecond, delays, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestProgressiveRetriesScheduleThreeRoundsThenIgnoreFailure(t *testing.T) {
	cause := errors.New("Aizhan returned an empty page")
	source := &attemptRecordingSource{fetch: func(context.Context, string, int) (model.Metric, error) {
		return model.Metric{}, cause
	}}
	st := &retryLifecycleStore{}
	delays := []time.Duration{time.Hour, 30 * time.Minute, 30 * time.Minute}
	service := newRetryLifecycleService(st, source, delays)
	job := model.CollectionJob{ID: primitive.NewObjectID(), Domain: "example.com"}

	for attempt := 1; attempt <= 4; attempt++ {
		// ClaimNextJob increments the durable count before collection. Keep the
		// same job identity across all rounds, as MongoDB RetryJob does.
		job.AttemptCount = attempt
		before := time.Now().UTC()
		service.collectJob(context.Background(), job)
		after := time.Now().UTC()
		if len(st.saved) != 0 {
			t.Fatal("a failed or empty response must never create a metric record")
		}
		if attempt <= 3 {
			if len(st.failed) != 0 || len(st.retried) != attempt {
				t.Fatalf("attempt %d ended before all three rechecks: %+v", attempt, st)
			}
			retry := st.retried[attempt-1]
			if retry.id != job.ID || retry.cause != cause {
				t.Fatal("retry lost original job or collection error")
			}
			if retry.availableAt.Before(before.Add(delays[attempt-1])) || retry.availableAt.After(after.Add(delays[attempt-1])) {
				t.Fatalf("attempt %d available_at = %s, want failure time + %s", attempt, retry.availableAt, delays[attempt-1])
			}
		}
	}
	if len(st.retried) != 3 || len(st.failed) != 1 {
		t.Fatalf("exhausted job must end after exactly three rechecks: %+v", st)
	}
	terminal := st.failed[0]
	if !strings.Contains(terminal.Error(), "复测 3 次后仍失败，已忽略本轮采集") || !errors.Is(terminal, cause) {
		t.Fatalf("terminal error lost exhaustion message or original cause: %v", terminal)
	}
	var marker interface{ RecheckExhausted() bool }
	if !errors.As(terminal, &marker) || !marker.RecheckExhausted() {
		t.Fatal("terminal error must let the store persist exhausted status")
	}
	if !reflect.DeepEqual(source.attempts, []int{1, 2, 3, 4}) || !reflect.DeepEqual(source.domains, []string{job.Domain, job.Domain, job.Domain, job.Domain}) {
		t.Fatalf("scraper did not receive persisted attempts/domain: %+v", source)
	}
	if len(st.released) != 0 || len(st.forgotten) != 4 {
		t.Fatal("normal completion must forget claims without releasing them")
	}
}

func TestProgressiveRetrySuccessSavesActualMetricAndStopsRetrying(t *testing.T) {
	cause := errors.New("temporary empty page")
	weight := int16(3)
	metric := model.Metric{BaiduPCWeight: &weight, BaiduMobile: &weight, WeightSource: "aizhan"}
	source := &attemptRecordingSource{fetch: func(_ context.Context, _ string, attempt int) (model.Metric, error) {
		if attempt == 1 {
			return model.Metric{}, cause
		}
		return metric, nil
	}}
	st := &retryLifecycleStore{}
	service := newRetryLifecycleService(st, source, []time.Duration{time.Hour, 30 * time.Minute, 30 * time.Minute})
	job := model.CollectionJob{ID: primitive.NewObjectID(), Domain: "example.com", AttemptCount: 1}
	service.collectJob(context.Background(), job)
	job.AttemptCount = 2
	service.collectJob(context.Background(), job)
	if len(st.retried) != 1 || len(st.failed) != 0 || !reflect.DeepEqual(st.saved, []model.Metric{metric}) {
		t.Fatalf("successful retry must save only its real result and schedule no more retries: %+v", st)
	}
}

func TestProgressiveRetryCancellationReleasesClaimWithoutConsumingFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := &attemptRecordingSource{fetch: func(ctx context.Context, _ string, _ int) (model.Metric, error) {
		cancel()
		return model.Metric{}, ctx.Err()
	}}
	st := &retryLifecycleStore{}
	service := newRetryLifecycleService(st, source, []time.Duration{time.Hour, 30 * time.Minute, 30 * time.Minute})
	job := model.CollectionJob{ID: primitive.NewObjectID(), Domain: "example.com", AttemptCount: 4}
	service.collectJob(ctx, job)
	if len(st.retried) != 0 || len(st.failed) != 0 || len(st.saved) != 0 {
		t.Fatalf("shutdown must not record a scrape failure or empty metric: %+v", st)
	}
	if !reflect.DeepEqual(st.released, []model.CollectionJob{job}) || !reflect.DeepEqual(st.forgotten, []primitive.ObjectID{job.ID}) {
		t.Fatal("canceled progressive attempt was not released before forgetting its claim")
	}
}

type legacyFailureSource struct{ cause error }

func (s legacyFailureSource) Fetch(context.Context, string) (model.Metric, error) {
	return model.Metric{}, s.cause
}

func TestLegacyCollectionFailureKeepsOriginalError(t *testing.T) {
	cause := errors.New("Chinaz unavailable")
	st := &retryLifecycleStore{}
	service := newRetryLifecycleService(st, legacyFailureSource{cause}, []time.Duration{time.Minute})
	service.collectJob(context.Background(), model.CollectionJob{ID: primitive.NewObjectID(), Domain: "example.com", AttemptCount: 2})
	if len(st.failed) != 1 || st.failed[0] != cause || len(st.retried) != 0 {
		t.Fatalf("ordinary scraper failure changed: %+v", st)
	}
}

func TestProgressiveExhaustionUsesConfiguredRoundCount(t *testing.T) {
	cause := errors.New("empty response")
	source := &attemptRecordingSource{fetch: func(context.Context, string, int) (model.Metric, error) { return model.Metric{}, cause }}
	st := &retryLifecycleStore{}
	service := newRetryLifecycleService(st, source, []time.Duration{time.Hour, 30 * time.Minute})
	service.collectJob(context.Background(), model.CollectionJob{ID: primitive.NewObjectID(), Domain: "example.com", AttemptCount: 3})
	if len(st.failed) != 1 || !strings.Contains(st.failed[0].Error(), "复测 2 次后仍失败") {
		t.Fatalf("exhaustion did not use configured rounds: %+v", st.failed)
	}
}
