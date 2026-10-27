package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"seo-monitor/internal/model"
)

type exhaustedRecheckForTest struct{ cause error }

func (e exhaustedRecheckForTest) Error() string          { return "rechecks exhausted: " + e.cause.Error() }
func (e exhaustedRecheckForTest) Unwrap() error          { return e.cause }
func (e exhaustedRecheckForTest) RecheckExhausted() bool { return true }

func TestAizhanRecheckMongoIntegration(t *testing.T) {
	uri := os.Getenv("SEO_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("set SEO_TEST_MONGO_URI to a disposable MongoDB server")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	database := "seo_recheck_test_" + primitive.NewObjectID().Hex()
	var connections []*Store
	open := func() *Store {
		s, err := New(ctx, uri, database)
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, s)
		if err = s.EnsureIndexes(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err = s.EnableIndependentSources(ctx); err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := open()
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		if err := s.db.Drop(cleanup); err != nil {
			t.Error(err)
		}
		for _, connection := range connections {
			_ = connection.Close(cleanup)
		}
	})
	date := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	cause := errors.New("Aizhan returned empty weights")
	create := func(name string) model.Domain {
		domain, err := s.CreateDomain(ctx, name+".example.com", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		return domain
	}
	queue := func(st *Store, domain model.Domain, day time.Time, force bool) model.CollectionJob {
		job, added, err := st.queueDomain(ctx, domain.ID, day, "test", force)
		if err != nil || !added {
			t.Fatalf("queue added=%v err=%v", added, err)
		}
		return job
	}
	makeReady := func(job model.CollectionJob) {
		if _, err := s.jobs.UpdateOne(ctx, bson.M{"_id": job.ID}, bson.M{"$set": bson.M{"available_at": time.Now().UTC().Add(-time.Minute)}}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("crash_recovery_preserves_recheck_rounds", func(t *testing.T) {
		for _, test := range []struct {
			name     string
			attempt  *int
			active   bool
			chinaz   bool
			want     int
			recovery int64
		}{
			{name: "first_recheck", attempt: intPointer(2), want: 1, recovery: 1},
			{name: "second_recheck", attempt: intPointer(3), want: 2, recovery: 1},
			{name: "last_recheck", attempt: intPointer(4), want: 3, recovery: 1},
			{name: "zero_count", attempt: intPointer(0), want: 0, recovery: 1},
			{name: "legacy_missing_count", want: 0, recovery: 1},
			{name: "active_worker", attempt: intPointer(3), active: true, want: 3},
			{name: "chinaz_unchanged", attempt: intPointer(3), chinaz: true, want: 3, recovery: 1},
		} {
			t.Run(test.name, func(t *testing.T) {
				st := s
				if test.chinaz {
					st = s.ChinazWeightStore()
				}
				id := primitive.NewObjectID()
				doc := bson.M{
					"_id": id, "domain": "crashed.example.com", "snapshot_date": date,
					"status": "running", "started_at": time.Now().UTC().Add(-time.Hour), "queued_at": date,
				}
				if test.attempt != nil {
					doc["attempt_count"] = *test.attempt
				}
				if _, err := st.jobs.InsertOne(ctx, doc); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					st.ForgetClaim(id)
					if _, err := st.jobs.DeleteOne(ctx, bson.M{"_id": id}); err != nil {
						t.Error(err)
					}
				})
				if test.active {
					st.inflight.Store(id, struct{}{})
				}
				if count, err := st.RecoverStaleJobs(ctx, 20*time.Minute); err != nil || count != test.recovery {
					t.Fatalf("recovered = %d, want %d, error = %v", count, test.recovery, err)
				}
				// Running -> queued is atomic: another recovery must not deduct
				// the attempt a second time, nor reclaim this process's active work.
				if count, err := st.RecoverStaleJobs(ctx, 20*time.Minute); err != nil || count != 0 {
					t.Fatalf("repeated recovery changed %d jobs: %v", count, err)
				}
				var job model.CollectionJob
				if err := st.jobs.FindOne(ctx, bson.M{"_id": id}).Decode(&job); err != nil {
					t.Fatal(err)
				}
				if job.AttemptCount != test.want {
					t.Fatalf("recovered count = %d, want %d", job.AttemptCount, test.want)
				}
				if test.active {
					if job.Status != "running" {
						t.Fatal("active worker was reclaimed")
					}
					return
				}
				if job.Status != "queued" || job.StartedAt != nil {
					t.Fatalf("crashed task was not released: %+v", job)
				}
				claimed, err := st.ClaimNextJob(ctx)
				if err != nil || claimed.ID != id || claimed.AttemptCount != test.want+1 {
					t.Fatalf("resumed wrong round: %+v, %v", claimed, err)
				}
			})
		}
	})

	t.Run("durable_three_rounds_and_terminal_restart", func(t *testing.T) {
		domain := create("durable")
		original := queue(s, domain, date, false)
		for attempt, delay := range []time.Duration{time.Hour, 30 * time.Minute, 30 * time.Minute} {
			job, err := s.ClaimNextJob(ctx)
			if err != nil || job.ID != original.ID || job.AttemptCount != attempt+1 {
				t.Fatalf("attempt %d: %+v %v", attempt+1, job, err)
			}
			availableAt := time.Now().UTC().Add(delay).Truncate(time.Millisecond)
			if err = s.RetryJob(ctx, job.ID, cause, availableAt); err != nil {
				t.Fatal(err)
			}
			s.ForgetClaim(job.ID)
			// Reconnecting simulates a service restart: neither the deadline nor
			// the attempt budget may reset or allow an early request.
			s = open()
			if _, err = s.ClaimNextJob(ctx); !errors.Is(err, ErrNotFound) {
				t.Fatalf("future recheck claimed: %v", err)
			}
			var persisted model.CollectionJob
			if err = s.jobs.FindOne(ctx, bson.M{"_id": job.ID}).Decode(&persisted); err != nil {
				t.Fatal(err)
			}
			if persisted.AvailableAt == nil || !persisted.AvailableAt.Equal(availableAt) || persisted.AttemptCount != attempt+1 || persisted.DedupeKey == nil {
				t.Fatalf("backoff was not durable: %+v", persisted)
			}
			if _, added, err := s.queueDomain(ctx, domain.ID, date, "startup", false); err != nil || added {
				t.Fatalf("restart duplicated a waiting recheck: added=%v err=%v", added, err)
			}
			makeReady(job)
		}
		job, err := s.ClaimNextJob(ctx)
		if err != nil || job.ID != original.ID || job.AttemptCount != 4 {
			t.Fatalf("last recheck: %+v %v", job, err)
		}
		terminal := fmt.Errorf("wrapped policy: %w", exhaustedRecheckForTest{cause})
		if err = s.MarkJobFailed(ctx, job.ID, terminal); err != nil {
			t.Fatal(err)
		}
		s.ForgetClaim(job.ID)
		s = open()
		var persisted bson.M
		if err = s.jobs.FindOne(ctx, bson.M{"_id": job.ID}).Decode(&persisted); err != nil {
			t.Fatal(err)
		}
		if persisted["retry_exhausted"] != true || persisted["status"] != "failed" {
			t.Fatalf("terminal policy was not persisted: %+v", persisted)
		}
		if _, added, err := s.queueDomain(ctx, domain.ID, date, "startup", false); err != nil || added {
			t.Fatalf("restart reset exhausted budget: added=%v err=%v", added, err)
		}
		if _, err = s.ClaimNextJob(ctx); !errors.Is(err, ErrNotFound) {
			t.Fatalf("terminal failure was scheduled again: %v", err)
		}
		if count, err := s.metrics.CountDocuments(ctx, bson.M{"domain_id": domain.ID}); err != nil || count != 0 {
			t.Fatalf("failed empty data created a metric: count=%d err=%v", count, err)
		}
		manual := queue(s, domain, date, true)
		if manual.ID == original.ID || manual.AttemptCount != 0 {
			t.Fatal("manual request did not start a new attempt budget")
		}
		queue(s, domain, date.AddDate(0, 0, 1), false)
		// Isolate following scenarios from these deliberately queued tasks.
		if _, err := s.jobs.DeleteMany(ctx, bson.M{"domain_id": domain.ID}); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("old_failure_can_use_new_policy", func(t *testing.T) {
		domain := create("historical")
		old := queue(s, domain, date, false)
		if _, err := s.jobs.UpdateOne(ctx, bson.M{"_id": old.ID}, bson.M{"$set": bson.M{"attempt_count": 4}}); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkJobFailed(ctx, old.ID, cause); err != nil {
			t.Fatal(err)
		}
		fresh := queue(s, domain, date, false)
		if fresh.ID == old.ID {
			t.Fatal("historical failure did not receive a new policy run")
		}
	})

	t.Run("chinaz_keeps_existing_requeue_policy", func(t *testing.T) {
		domain := create("chinaz")
		child := s.ChinazWeightStore()
		old := queue(child, domain, date, false)
		if err := child.MarkJobFailed(ctx, old.ID, exhaustedRecheckForTest{cause}); err != nil {
			t.Fatal(err)
		}
		queue(child, domain, date, false)
	})

	t.Run("failed_recheck_preserves_both_source_values", func(t *testing.T) {
		domain := create("preserved")
		old := queue(s, domain, date, false)
		weight, other := int16(6), int16(3)
		metric := model.Metric{SourceURL: "https://www.aizhan.com/cha/" + domain.Domain + "/", RawSHA256: strings.Repeat("a", 64), BaiduPCWeight: &weight, BaiduMobile: &weight}
		metric.MarkWeights("aizhan")
		if err := s.SaveJobResult(ctx, old, metric); err != nil {
			t.Fatal(err)
		}
		child := s.ChinazWeightStore()
		otherJob := queue(child, domain, date, false)
		metric.SourceURL, metric.BaiduPCWeight, metric.BaiduMobile = "https://seo.chinaz.com/"+domain.Domain, &other, &other
		metric.MarkWeights("chinaz")
		if err := child.SaveJobResult(ctx, otherJob, metric); err != nil {
			t.Fatal(err)
		}
		newJob := queue(s, domain, date, true)
		if err := s.MarkJobFailed(ctx, newJob.ID, exhaustedRecheckForTest{cause}); err != nil {
			t.Fatal(err)
		}
		var got model.Metric
		if err := s.metrics.FindOne(ctx, bson.M{"domain_id": domain.ID, "snapshot_date": date}).Decode(&got); err != nil {
			t.Fatal(err)
		}
		aizhan, chinaz := got.WeightSnapshots["aizhan"], got.WeightSnapshots["chinaz"]
		if aizhan.Valid || aizhan.Metric == nil || aizhan.Metric.BaiduPCWeight == nil || *aizhan.Metric.BaiduPCWeight != weight || !chinaz.Valid || chinaz.Metric == nil || *chinaz.Metric.BaiduPCWeight != other {
			t.Fatalf("failed recheck changed source data: %+v", got)
		}
	})
}

func intPointer(value int) *int { return &value }
