package store

import (
	"context"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"seo-monitor/internal/model"
)

// ForgetClaim removes a local execution from the recovery exclusion set only
// after its worker has finished all collection and persistence work.
func (s *Store) ForgetClaim(id primitive.ObjectID) {
	if s.inflight != nil {
		s.inflight.Delete(id)
	}
}

// Recovery may run repeatedly. Never reclaim this process's active workers,
// even if a configured scrape takes longer than the stale threshold. Deployment
// still requires one collector process (rate limiting is also process-local).
func (s *Store) staleJobFilter(cutoff time.Time) bson.M {
	filter := bson.M{"status": "running", "started_at": bson.M{"$lt": cutoff}}
	active := bson.A{}
	if s.inflight != nil {
		s.inflight.Range(func(key, _ any) bool { active = append(active, key); return true })
	}
	if len(active) > 0 {
		filter["_id"] = bson.M{"$nin": active}
	}
	return filter
}

func interruptedJobUpdate(job model.CollectionJob, now time.Time) (bson.M, bson.M) {
	filter := bson.M{"_id": job.ID, "status": "running", "attempt_count": job.AttemptCount, "started_at": job.StartedAt}
	update := bson.M{
		"$set":   bson.M{"status": "queued", "available_at": now, "error_message": "collection interrupted by service shutdown; queued for resume"},
		"$unset": bson.M{"started_at": "", "finished_at": ""},
	}
	// An administrative restart is not a failed source attempt.
	if job.AttemptCount > 0 {
		update["$inc"] = bson.M{"attempt_count": -1}
	}
	return filter, update
}

func (s *Store) ReleaseInterruptedJob(ctx context.Context, job model.CollectionJob) error {
	filter, update := interruptedJobUpdate(job, time.Now().UTC())
	_, err := s.jobs.UpdateOne(ctx, filter, update)
	return err
}
