package collector

import (
	"context"
	"log/slog"
	"time"
)

type staleJobStore interface {
	RecoverStaleJobs(context.Context, time.Duration) (int64, error)
}

// RunRecovery repeats after startup so jobs that were too young to recover on
// boot can become eligible later. Each sweep is bounded and cancelable.
func RunRecovery(ctx context.Context, st staleJobStore, staleAfter, interval time.Duration, logger *slog.Logger) {
	for {
		if ctx.Err() != nil {
			return
		}
		checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		count, err := st.RecoverStaleJobs(checkCtx, staleAfter)
		cancel()
		if err != nil && ctx.Err() == nil {
			logger.Error("recover stale jobs", "error", err)
		}
		if count > 0 {
			logger.Info("recovered stale jobs", "count", count)
		}
		if !wait(ctx, interval) {
			return
		}
	}
}
