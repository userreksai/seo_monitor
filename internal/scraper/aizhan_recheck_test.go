package scraper

import (
	"context"
	"errors"
	"math"
	"net/http"
	"testing"
	"time"
)

func TestAizhanRecheckPacesFirstRequestAndResetsNextJob(t *testing.T) {
	a := newTestAizhan(t, func(http.ResponseWriter, *http.Request) {
		t.Error("unexpected network request")
	})
	const baseDelay = 10 * time.Millisecond
	a.cfg.MinDelay, a.cfg.MaxDelay = baseDelay, baseDelay
	var requestedAt time.Time
	a.client.Transport = masterResponse(200, string(aizhanFixture(t)), func(*http.Request) {
		requestedAt = time.Now()
	})
	previousMultiplier := time.Duration(0)
	for _, attempt := range []int{1, 2, 3, 4, 1, 1} {
		finished := a.lastFinished
		multiplier := aizhanAttemptMultiplier(attempt)
		if _, err := a.FetchAttempt(context.Background(), "www.baidu.com", attempt); err != nil {
			t.Fatal(err)
		}
		if !finished.IsZero() {
			// Neither a later retry nor a new normal job may shorten the gap
			// already scheduled by its predecessor.
			want := max(previousMultiplier, multiplier) * baseDelay
			if elapsed := requestedAt.Sub(finished); elapsed < want {
				t.Fatalf("attempt %d requested after %v, want at least %v", attempt, elapsed, want)
			}
		}
		if gap := a.next.Sub(a.lastFinished); gap != multiplier*baseDelay {
			t.Fatalf("attempt %d left gap %v, want %v", attempt, gap, multiplier*baseDelay)
		}
		previousMultiplier = multiplier
	}
	if a.cfg.MinDelay != baseDelay || a.cfg.MaxDelay != baseDelay {
		t.Fatal("recheck changed the base delay configuration")
	}
}

func TestAizhanRecheckPacesTransportRetries(t *testing.T) {
	a := newTestAizhan(t, func(http.ResponseWriter, *http.Request) {
		t.Error("unexpected network request")
	})
	const baseDelay = 10 * time.Millisecond
	a.cfg.MinDelay, a.cfg.MaxDelay = baseDelay, baseDelay
	calls := 0
	a.client.Transport = masterTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return masterResponse(503, "", nil)(r)
		}
		if elapsed := time.Since(a.lastFinished); elapsed < 4*baseDelay {
			t.Errorf("transport retry waited %v, want at least %v", elapsed, 4*baseDelay)
		}
		return masterResponse(200, string(aizhanFixture(t)), nil)(r)
	})
	if _, err := a.FetchAttempt(context.Background(), "www.baidu.com", 3); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("requests = %d, want 2", calls)
	}
}

func TestAizhanRecheckPacesAgentMasterRecheck(t *testing.T) {
	a := agentForRecheck(t, "empty")
	const baseDelay = 10 * time.Millisecond
	a.cfg.MinDelay, a.cfg.MaxDelay = baseDelay, baseDelay
	calls := 0
	a.client.Transport = masterResponse(200, string(aizhanFixture(t)), func(*http.Request) {
		calls++
		if elapsed := time.Since(a.lastFinished); elapsed < 8*baseDelay {
			t.Errorf("master recheck waited %v, want at least %v", elapsed, 8*baseDelay)
		}
	})
	metric, err := a.FetchAttempt(context.Background(), "www.baidu.com", 4)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || metric.CollectionRoute != "direct:master-recheck" {
		t.Fatalf("master recheck calls = %d, route = %q", calls, metric.CollectionRoute)
	}
}

func TestAizhanRecheckWaitCancelsWithoutSendingRequest(t *testing.T) {
	for _, cooldown := range []bool{false, true} {
		t.Run(map[bool]string{false: "scaled gap", true: "source cooldown"}[cooldown], func(t *testing.T) {
			a := newTestAizhan(t, func(http.ResponseWriter, *http.Request) {
				t.Error("unexpected network request")
			})
			const baseDelay = 10 * time.Millisecond
			a.cfg.MinDelay, a.cfg.MaxDelay = baseDelay, baseDelay
			calls := 0
			a.client.Transport = masterResponse(200, string(aizhanFixture(t)), func(*http.Request) { calls++ })
			if _, err := a.Fetch(context.Background(), "www.baidu.com"); err != nil {
				t.Fatal(err)
			}
			if cooldown {
				a.cooldown = time.Now().Add(time.Minute)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if _, err := a.FetchAttempt(ctx, "www.baidu.com", 4); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("canceled recheck sent a request: total = %d", calls)
			}
			if cooldown && time.Until(a.cooldown) < 59*time.Second {
				t.Fatal("recheck shortened the source cooldown")
			}
		})
	}
}

func TestAizhanRecheckMultiplierIsBounded(t *testing.T) {
	for _, test := range []struct {
		attempt int
		want    time.Duration
	}{{math.MinInt, 1}, {0, 1}, {1, 1}, {2, 2}, {3, 4}, {4, 8}, {5, 8}, {math.MaxInt, 8}} {
		if got := aizhanAttemptMultiplier(test.attempt); got != test.want {
			t.Fatalf("multiplier(%d) = %v, want %v", test.attempt, got, test.want)
		}
	}
	if got := scaledAizhanDelay(time.Duration(math.MaxInt64), 8); got != time.Duration(math.MaxInt64) {
		t.Fatalf("large delay overflowed: %v", got)
	}
}
