package store

import (
	"context"
	"errors"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"seo-monitor/internal/model"
)

// Retain old values for inspection, but never notify on a failed refresh.
// Supplemental failures cannot invalidate successful primary weights.
func (s *Store) invalidateWeightAttempt(ctx context.Context, id primitive.ObjectID) error {
	if s.metricSource == "chinaz_supplement" {
		return nil
	}
	var job model.CollectionJob
	err := s.jobs.FindOne(ctx, bson.M{"_id": id}).Decode(&job)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = s.metrics.UpdateOne(ctx, bson.M{"domain": job.Domain, "snapshot_date": job.SnapshotDate}, bson.M{"$set": bson.M{"weight_valid": false}})
	return err
}

// Backfill provenance from the response URL and validity from the latest job.
// Without a matching job, validity stays unknown rather than guessing.
func (s *Store) InitializeWeightSources(ctx context.Context) (int64, error) {
	cursor, err := s.metrics.Find(ctx, bson.M{"weight_source": bson.M{"$exists": false}, "baidu_pc_weight": bson.M{"$exists": true}, "baidu_mobile_weight": bson.M{"$exists": true}})
	if err != nil {
		return 0, err
	}
	defer cursor.Close(ctx)
	var count int64
	for cursor.Next(ctx) {
		var m model.Metric
		if err := cursor.Decode(&m); err != nil {
			return count, err
		}
		source := m.LegacyWeightSource()
		if source == "" {
			continue
		}
		fields := bson.M{"weight_source": source}
		// A terminal successful job proves this historical day's refresh completed.
		// Failed/unfinished jobs may have left old values in place.
		if m.WeightValid == nil {
			var job model.CollectionJob
			jobErr := s.jobs.FindOne(ctx, bson.M{"domain_id": m.DomainID, "snapshot_date": m.SnapshotDate}, options.FindOne().SetSort(bson.D{{Key: "queued_at", Value: -1}, {Key: "_id", Value: -1}})).Decode(&job)
			if jobErr != nil && !errors.Is(jobErr, mongo.ErrNoDocuments) {
				return count, jobErr
			}
			if jobErr == nil {
				fields["weight_valid"] = job.Status == "succeeded"
			}
		}
		result, err := s.metrics.UpdateOne(ctx, bson.M{"_id": m.ID, "weight_source": bson.M{"$exists": false}, "weight_valid": m.WeightValid}, bson.M{"$set": fields})
		if err != nil {
			return count, err
		}
		count += result.ModifiedCount
	}
	return count, cursor.Err()
}
