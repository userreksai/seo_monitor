package collector

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"seo-monitor/internal/model"
)

type recoveryProbe struct {
	calls     atomic.Int32
	recovered chan struct{}
}

func (s *recoveryProbe) RecoverStaleJobs(ctx context.Context, age time.Duration) (int64, error) {
	if _, ok := ctx.Deadline(); !ok {
		panic("unbounded recovery query")
	}
	if age != 20*time.Minute {
		panic("stale threshold changed")
	}
	if s.calls.Add(1) == 2 {
		close(s.recovered)
		return 1, nil
	}
	return 0, nil
}

func TestRecoveryRepeatsAfterNoJobsEligibleAtStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := &recoveryProbe{recovered: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunRecovery(ctx, st, 20*time.Minute, time.Millisecond, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	select {
	case <-st.recovered:
	case <-time.After(time.Second):
		t.Fatal("startup-only recovery stranded job")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("recovery did not stop")
	}
}

type interruptedStore struct {
	queuedStore
	released     chan model.CollectionJob
	forgotten    chan primitive.ObjectID
	allowRelease chan struct{}
}

func (s *interruptedStore) ReleaseInterruptedJob(ctx context.Context, job model.CollectionJob) error {
	if ctx.Err() != nil {
		panic("release used canceled worker context")
	}
	if _, ok := ctx.Deadline(); !ok {
		panic("release context unbounded")
	}
	s.released <- job
	select {
	case <-s.allowRelease:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *interruptedStore) ForgetClaim(id primitive.ObjectID) { s.forgotten <- id }

type interruptedSource struct{ entered chan struct{} }

func (s interruptedSource) Fetch(ctx context.Context, _ string) (model.Metric, error) {
	close(s.entered)
	<-ctx.Done()
	return model.Metric{}, ctx.Err()
}

func TestShutdownRequeuesClaimBeforeWorkerWaitReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	job := model.CollectionJob{ID: primitive.NewObjectID(), Domain: "example.com", AttemptCount: 1}
	st := &interruptedStore{queuedStore: queuedStore{jobs: []model.CollectionJob{job}}, released: make(chan model.CollectionJob, 1), forgotten: make(chan primitive.ObjectID, 1), allowRelease: make(chan struct{})}
	source := interruptedSource{entered: make(chan struct{})}
	s := New(st, source, 1, time.Millisecond, []time.Duration{time.Minute}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.Start(ctx)
	select {
	case <-source.entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	cancel()
	select {
	case got := <-st.released:
		if got.ID != job.ID {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("interrupted claim not released")
	}
	done := make(chan struct{})
	go func() { s.Wait(); close(done) }()
	select {
	case <-done:
		t.Fatal("shutdown did not wait for release")
	default:
	}
	close(st.allowRelease)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker never stopped")
	}
	select {
	case got := <-st.forgotten:
		if got != job.ID {
			t.Fatal(got)
		}
	default:
		t.Fatal("local claim not cleared")
	}
}
