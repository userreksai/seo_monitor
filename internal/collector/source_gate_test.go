package collector

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"seo-monitor/internal/model"
)

type coolingSource struct {
	called bool
	cancel context.CancelFunc
}

func (s *coolingSource) WaitReady(ctx context.Context) error {
	s.called = true
	s.cancel()
	<-ctx.Done()
	return ctx.Err()
}
func (s *coolingSource) Fetch(context.Context, string) (model.Metric, error) {
	panic("must not fetch during cooldown")
}

func TestSourceCooldownWaitsBeforeClaim(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source := &coolingSource{cancel: cancel}
	// A nil store will panic if the worker claims before the source is ready.
	service := New(nil, source, 1, time.Millisecond, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	service.runWorker(ctx, 1)
	if !source.called {
		t.Fatal("source gate was skipped")
	}
}
