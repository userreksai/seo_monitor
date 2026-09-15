package store

import (
	"sync"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"seo-monitor/internal/model"
)

func TestRecoveryExcludesLiveWorkerUntilItFinishes(t *testing.T) {
	st := &Store{inflight: &sync.Map{}}
	id := primitive.NewObjectID()
	st.inflight.Store(id, struct{}{})
	cutoff := time.Now().UTC().Add(-20 * time.Minute)
	filter := st.staleJobFilter(cutoff)
	if filter["status"] != "running" || filter["started_at"].(bson.M)["$lt"] != cutoff {
		t.Fatal(filter)
	}
	ids := filter["_id"].(bson.M)["$nin"].(bson.A)
	if len(ids) != 1 || ids[0] != id {
		t.Fatalf("live task can be reclaimed: %+v", filter)
	}
	st.ForgetClaim(id)
	if _, ok := st.staleJobFilter(cutoff)["_id"]; ok {
		t.Fatal("abandoned task excluded forever")
	}
}

func TestShutdownReleaseOnlyMatchesOriginalRunningAttempt(t *testing.T) {
	now := time.Now().UTC()
	started := now.Add(-time.Second)
	job := model.CollectionJob{ID: primitive.NewObjectID(), AttemptCount: 3, StartedAt: &started}
	filter, update := interruptedJobUpdate(job, now)
	if filter["_id"] != job.ID || filter["status"] != "running" || filter["attempt_count"] != 3 || filter["started_at"] != job.StartedAt {
		t.Fatal("release can overwrite completed or newer attempt", filter)
	}
	if update["$set"].(bson.M)["status"] != "queued" || update["$set"].(bson.M)["available_at"] != now {
		t.Fatal("release delayed resume", update)
	}
	if update["$inc"].(bson.M)["attempt_count"] != -1 {
		t.Fatal("restart consumes failure retry budget")
	}
	if _, ok := update["$unset"].(bson.M)["dedupe_key"]; ok {
		t.Fatal("release broke open task dedupe")
	}
}
