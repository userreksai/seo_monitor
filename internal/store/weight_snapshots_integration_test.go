package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
	"seo-monitor/internal/model"
)

// An explicitly supplied test server is required. Each run creates and drops
// only its own randomly named database, never the application's database.
func TestDualSourceMongoIntegration(t *testing.T) {
	uri := os.Getenv("SEO_TEST_MONGO_URI")
	if uri == "" {
		t.Skip("set SEO_TEST_MONGO_URI to a disposable MongoDB server")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	s, err := New(ctx, uri, "seo_dual_test_"+primitive.NewObjectID().Hex())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if err := s.db.Drop(cleanup); err != nil {
			t.Error(err)
		}
		_ = s.Close(cleanup)
	})
	// Match the existing production validator's required provenance fields.
	err = s.db.CreateCollection(ctx, "domain_daily_metrics", options.CreateCollection().SetValidator(bson.M{"$jsonSchema": bson.M{
		"bsonType": "object", "required": bson.A{"domain_id", "domain", "snapshot_date", "collected_at", "source_url", "raw_sha256"},
		"properties": bson.M{"weight_source": bson.M{"enum": bson.A{"aizhan", "chinaz"}}, "weight_valid": bson.M{"bsonType": "bool"}, "raw_sha256": bson.M{"bsonType": "string", "pattern": "^[a-f0-9]{64}$"}},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.EnsureIndexes(ctx); err != nil {
		t.Fatal(err)
	}
	supplement, err := s.EnableIndependentSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	date := time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC)
	metric := func(source string, weight int16) model.Metric {
		m := model.Metric{CollectedAt: date.Add(time.Hour), SourceURL: "https://" + source + ".com/example.com", RawSHA256: strings.Repeat("a", 64), BaiduPCWeight: &weight, BaiduMobile: &weight}
		m.MarkWeights(source)
		return m
	}
	load := func(job model.CollectionJob) model.Metric {
		var m model.Metric
		if err := s.metrics.FindOne(ctx, bson.M{"domain": job.Domain, "snapshot_date": job.SnapshotDate}).Decode(&m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	for _, first := range []string{"aizhan", "chinaz"} {
		t.Run("completion_order_"+first, func(t *testing.T) {
			d, err := s.CreateDomain(ctx, first+".example.com", nil, "")
			if err != nil {
				t.Fatal(err)
			}
			job, _, err := s.QueueDomain(ctx, d.ID, date, "manual", false)
			if err != nil {
				t.Fatal(err)
			}
			jobs := map[string]model.CollectionJob{"aizhan": job}
			for source, child := range map[string]*Store{"chinaz": s.chinazWeights, "chinaz_supplement": supplement} {
				var childJob model.CollectionJob
				if err := child.jobs.FindOne(ctx, bson.M{"domain_id": d.ID, "snapshot_date": date}).Decode(&childJob); err != nil {
					t.Fatal(err)
				}
				jobs[source] = childJob
			}
			category := "$literal-category"
			sup := metric("chinaz", 0)
			sup.SiteCategory = &category
			if err = supplement.SaveJobResult(ctx, jobs["chinaz_supplement"], sup); err != nil {
				t.Fatal(err)
			}
			order := []string{first, "aizhan"}
			if first == "aizhan" {
				order[1] = "chinaz"
			}
			for _, source := range order {
				st, weight := s, int16(5)
				if source == "chinaz" {
					st, weight = s.chinazWeights, 0
				}
				if err = st.SaveJobResult(ctx, jobs[source], metric(source, weight)); err != nil {
					t.Fatal(err)
				}
			}
			m := load(job)
			if m.WeightSource != "aizhan" || *m.BaiduPCWeight != 5 || *m.WeightSnapshots["chinaz"].Metric.BaiduPCWeight != 0 || *m.SiteCategory != category {
				t.Fatalf("lost source or supplement: %+v", m)
			}
			if err = s.MarkJobFailed(ctx, job.ID, errors.New("$aizhan-timeout")); err != nil {
				t.Fatal(err)
			}
			m = load(job)
			if m.WeightSource != "chinaz" || !*m.WeightValid || *m.BaiduPCWeight != 0 || m.WeightSnapshots["aizhan"].Valid || *m.WeightSnapshots["aizhan"].Metric.BaiduPCWeight != 5 {
				t.Fatal("failure lost data or failed to select Chinaz")
			}
			items, _, err := s.SearchLatest(ctx, "domain", d.Domain, "", "", "", 1, 10)
			if err != nil || len(items) != 1 || items[0].Collection == nil || items[0].WeightCollections["chinaz"] == nil || items[0].Collection.Status != "succeeded" || items[0].WeightCollections["aizhan"].Status != "failed" {
				t.Fatalf("latest lookup failed: %v %+v", err, items)
			}
			if err = s.SaveJobResult(ctx, job, metric("aizhan", 4)); err != nil {
				t.Fatal(err)
			}
			m = load(job)
			if m.WeightSource != "aizhan" || m.WeightSnapshots["aizhan"].ErrorMessage != "" || !m.WeightSnapshots["chinaz"].Valid {
				t.Fatal("source recovery failed")
			}
			// A failure in the other source never invalidates Aizhan.
			failure, _ := weightFailureUpdate("chinaz", time.Now(), "timeout")
			if _, err = s.metrics.UpdateOne(ctx, bson.M{"_id": m.ID}, failure); err != nil {
				t.Fatal(err)
			}
			m = load(job)
			if m.WeightSource != "aizhan" || !*m.WeightValid || m.WeightSnapshots["chinaz"].Valid {
				t.Fatal("cross-source invalidation")
			}
			if err = s.MarkJobFailed(ctx, job.ID, errors.New("timeout")); err != nil {
				t.Fatal(err)
			}
			m = load(job)
			if *m.WeightValid || *m.BaiduPCWeight != 4 || m.WeightSnapshots["chinaz"].Metric == nil {
				t.Fatal("both failed must preserve data but be invalid")
			}
		})
	}
	t.Run("legacy_migration", func(t *testing.T) {
		legacy := metric("chinaz", 2)
		legacy.DomainID = primitive.NewObjectID()
		legacy.Domain = "legacy.example.com"
		legacy.SnapshotDate = date
		result, err := s.metrics.InsertOne(ctx, legacy)
		if err != nil {
			t.Fatal(err)
		}
		count, err := s.InitializeWeightSnapshots(ctx)
		if err != nil || count != 1 {
			t.Fatalf("migration: %d %v", count, err)
		}
		count, err = s.InitializeWeightSnapshots(ctx)
		if err != nil || count != 0 {
			t.Fatalf("migration not idempotent: %d %v", count, err)
		}
		var m model.Metric
		if err = s.metrics.FindOne(ctx, bson.M{"_id": result.InsertedID}).Decode(&m); err != nil {
			t.Fatal(err)
		}
		if len(m.WeightSnapshots) != 1 || *m.WeightSnapshots["chinaz"].Metric.BaiduPCWeight != 2 {
			t.Fatal("invented or lost historical source")
		}
		// In-write bootstrap must work even if background migration has not run.
		legacy.Domain = "unmigrated.example.com"
		if _, err = s.metrics.InsertOne(ctx, legacy); err != nil {
			t.Fatal(err)
		}
		job := model.CollectionJob{DomainID: legacy.DomainID, Domain: legacy.Domain, SnapshotDate: date}
		if err = s.SaveJobResult(ctx, job, metric("aizhan", 6)); err != nil {
			t.Fatal(err)
		}
		m = load(job)
		if *m.WeightSnapshots["chinaz"].Metric.BaiduPCWeight != 2 || *m.BaiduPCWeight != 6 {
			t.Fatal("bootstrap overwrote legacy source")
		}
		legacy.Domain = "unlabeled.example.com"
		legacy.SourceURL = "https://seo.chinaz.com/unlabeled.example.com"
		legacy.WeightSource, legacy.WeightValid = "", nil
		if _, err = s.metrics.InsertOne(ctx, legacy); err != nil {
			t.Fatal(err)
		}
		job.Domain = legacy.Domain
		if err = s.SaveJobResult(ctx, job, metric("aizhan", 6)); err != nil {
			t.Fatal(err)
		}
		m = load(job)
		if m.WeightSnapshots["chinaz"].Metric == nil || *m.WeightSnapshots["chinaz"].Metric.BaiduPCWeight != 2 || m.WeightSnapshots["chinaz"].Valid {
			t.Fatal("unlabeled history lost or assigned unverified validity")
		}
	})
	t.Run("concurrent_insert", func(t *testing.T) {
		for i := 0; i < 20; i++ {
			job := model.CollectionJob{DomainID: primitive.NewObjectID(), Domain: fmt.Sprintf("race-%d.example.com", i), SnapshotDate: date}
			var wg sync.WaitGroup
			errs := make(chan error, 3)
			for _, st := range []*Store{s, s.chinazWeights, supplement} {
				wg.Add(1)
				go func(st *Store) {
					defer wg.Done()
					source := st.metricSource
					if source == "chinaz_supplement" {
						source = "chinaz"
					}
					errs <- st.SaveJobResult(ctx, job, metric(source, 3))
				}(st)
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			m := load(job)
			if len(m.WeightSnapshots) != 2 || m.WeightSource != "aizhan" || m.SupplementalCollectedAt == nil {
				t.Fatal("concurrent write lost data")
			}
		}
	})
	t.Run("independent_queue_progress", func(t *testing.T) {
		p, err := s.CollectionProgress(ctx, date)
		if err != nil {
			t.Fatal(err)
		}
		if p.Total != 4 || p.Sources["aizhan"].Total != 2 || p.Sources["chinaz"].Total != 2 || p.Supplement == nil {
			t.Fatalf("bad progress: %+v", p)
		}
	})
}
