package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"

	"seo-monitor/internal/model"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

type Store struct {
	client  *mongo.Client
	db      *mongo.Database
	domains *mongo.Collection
	metrics *mongo.Collection
	jobs    *mongo.Collection
}

func New(ctx context.Context, uri, database string) (*Store, error) {
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri).SetAppName("seo-monitor"))
	if err != nil {
		return nil, fmt.Errorf("connect MongoDB: %w", err)
	}
	if err := client.Ping(ctx, readpref.Primary()); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, fmt.Errorf("ping MongoDB: %w", err)
	}
	db := client.Database(database)
	return &Store{
		client:  client,
		db:      db,
		domains: db.Collection("domains"),
		metrics: db.Collection("domain_daily_metrics"),
		jobs:    db.Collection("collection_jobs"),
	}, nil
}

func (s *Store) Close(ctx context.Context) error {
	return s.client.Disconnect(ctx)
}

func (s *Store) Health(ctx context.Context) error {
	return s.client.Ping(ctx, readpref.Primary())
}

// EnsureIndexes is safe to call on every startup. Collection validators live in
// scripts/mongo-init.js so operators can review and apply them explicitly.
func (s *Store) EnsureIndexes(ctx context.Context) error {
	_, err := s.domains.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "domain", Value: 1}}, Options: options.Index().SetName("uq_domains_domain").SetUnique(true)},
		{Keys: bson.D{{Key: "active", Value: 1}, {Key: "created_at", Value: -1}}, Options: options.Index().SetName("ix_domains_active_created")},
	})
	if err != nil {
		return fmt.Errorf("create domain indexes: %w", err)
	}

	_, err = s.metrics.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "domain", Value: 1}, {Key: "snapshot_date", Value: 1}}, Options: options.Index().SetName("uq_metrics_domain_date").SetUnique(true)},
		{Keys: bson.D{{Key: "domain_id", Value: 1}, {Key: "snapshot_date", Value: -1}}, Options: options.Index().SetName("ix_metrics_domain_id_date")},
		{Keys: bson.D{{Key: "snapshot_date", Value: -1}}, Options: options.Index().SetName("ix_metrics_date")},
	})
	if err != nil {
		return fmt.Errorf("create metric indexes: %w", err)
	}

	_, err = s.jobs.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "queued_at", Value: 1}}, Options: options.Index().SetName("ix_jobs_status_queue")},
		{Keys: bson.D{{Key: "domain_id", Value: 1}, {Key: "snapshot_date", Value: -1}}, Options: options.Index().SetName("ix_jobs_domain_date")},
		{Keys: bson.D{{Key: "dedupe_key", Value: 1}}, Options: options.Index().SetName("uq_jobs_open").SetUnique(true).SetSparse(true)},
	})
	if err != nil {
		return fmt.Errorf("create job indexes: %w", err)
	}
	return nil
}

func (s *Store) CreateDomain(ctx context.Context, domain string, displayName *string) (model.Domain, error) {
	now := time.Now().UTC()
	item := model.Domain{
		ID:          primitive.NewObjectID(),
		Domain:      domain,
		DisplayName: displayName,
		Active:      true,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if _, err := s.domains.InsertOne(ctx, item); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return model.Domain{}, ErrConflict
		}
		return model.Domain{}, err
	}
	return item, nil
}

func (s *Store) ListDomains(ctx context.Context, includeArchived bool) ([]model.Domain, error) {
	filter := bson.M{}
	if !includeArchived {
		filter["active"] = true
	}
	cursor, err := s.domains.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var items []model.Domain
	if err := cursor.All(ctx, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Store) GetDomain(ctx context.Context, id primitive.ObjectID) (model.Domain, error) {
	var item model.Domain
	err := s.domains.FindOne(ctx, bson.M{"_id": id}).Decode(&item)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return model.Domain{}, ErrNotFound
	}
	return item, err
}

func (s *Store) UpdateDomain(ctx context.Context, id primitive.ObjectID, patch model.DomainPatch) (model.Domain, error) {
	set := bson.M{"updated_at": time.Now().UTC()}
	unset := bson.M{}
	if patch.HasDisplayName {
		if patch.DisplayName == "" {
			unset["display_name"] = ""
		} else {
			set["display_name"] = patch.DisplayName
		}
	}
	if patch.HasActive {
		set["active"] = patch.Active
		if patch.Active {
			unset["archived_at"] = ""
		} else {
			set["archived_at"] = time.Now().UTC()
		}
	}
	update := bson.M{"$set": set}
	if len(unset) > 0 {
		update["$unset"] = unset
	}
	var item model.Domain
	err := s.domains.FindOneAndUpdate(ctx, bson.M{"_id": id}, update,
		options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&item)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return model.Domain{}, ErrNotFound
	}
	return item, err
}

func (s *Store) ArchiveDomain(ctx context.Context, id primitive.ObjectID) error {
	now := time.Now().UTC()
	result, err := s.domains.UpdateOne(ctx, bson.M{"_id": id}, bson.M{"$set": bson.M{
		"active": false, "archived_at": now, "updated_at": now,
	}})
	if err != nil {
		return err
	}
	if result.MatchedCount == 0 {
		return ErrNotFound
	}
	_, _ = s.jobs.UpdateMany(ctx, bson.M{"domain_id": id, "status": "queued"}, bson.M{
		"$set":   bson.M{"status": "canceled", "finished_at": now},
		"$unset": bson.M{"dedupe_key": ""},
	})
	return nil
}

func (s *Store) QueueAll(ctx context.Context, snapshotDate time.Time, requestedBy string) (int, error) {
	domains, err := s.ListDomains(ctx, false)
	if err != nil {
		return 0, err
	}
	queued := 0
	for _, domain := range domains {
		_, added, queueErr := s.QueueDomain(ctx, domain.ID, snapshotDate, requestedBy, false)
		if queueErr != nil {
			return queued, queueErr
		}
		if added {
			queued++
		}
	}
	return queued, nil
}

func (s *Store) QueueDomain(ctx context.Context, id primitive.ObjectID, snapshotDate time.Time, requestedBy string, force bool) (model.CollectionJob, bool, error) {
	domain, err := s.GetDomain(ctx, id)
	if err != nil {
		return model.CollectionJob{}, false, err
	}
	if !domain.Active {
		return model.CollectionJob{}, false, fmt.Errorf("域名已归档")
	}
	if !force {
		count, countErr := s.jobs.CountDocuments(ctx, bson.M{
			"domain_id": id, "snapshot_date": snapshotDate, "status": "succeeded",
		}, options.Count().SetLimit(1))
		if countErr != nil {
			return model.CollectionJob{}, false, countErr
		}
		if count > 0 {
			return model.CollectionJob{}, false, nil
		}
	}

	now := time.Now().UTC()
	key := id.Hex() + ":" + snapshotDate.Format("2006-01-02")
	job := model.CollectionJob{
		ID:           primitive.NewObjectID(),
		DomainID:     id,
		Domain:       domain.Domain,
		SnapshotDate: snapshotDate,
		Status:       "queued",
		RequestedBy:  requestedBy,
		QueuedAt:     now,
		DedupeKey:    &key,
	}
	if _, err := s.jobs.InsertOne(ctx, job); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return model.CollectionJob{}, false, nil
		}
		return model.CollectionJob{}, false, err
	}
	return job, true, nil
}

func (s *Store) ClaimNextJob(ctx context.Context) (model.CollectionJob, error) {
	now := time.Now().UTC()
	update := bson.M{
		"$set": bson.M{"status": "running", "started_at": now},
		"$inc": bson.M{"attempt_count": 1},
	}
	var job model.CollectionJob
	err := s.jobs.FindOneAndUpdate(ctx, bson.M{"status": "queued"}, update,
		options.FindOneAndUpdate().SetSort(bson.D{{Key: "queued_at", Value: 1}}).SetReturnDocument(options.After),
	).Decode(&job)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return model.CollectionJob{}, ErrNotFound
	}
	return job, err
}

func (s *Store) SaveJobResult(ctx context.Context, job model.CollectionJob, metric model.Metric) error {
	metric.ID = primitive.NilObjectID
	metric.DomainID = job.DomainID
	metric.Domain = job.Domain
	metric.SnapshotDate = job.SnapshotDate
	if metric.CollectedAt.IsZero() {
		metric.CollectedAt = time.Now().UTC()
	}
	_, err := s.metrics.ReplaceOne(ctx,
		bson.M{"domain": job.Domain, "snapshot_date": job.SnapshotDate}, metric,
		options.Replace().SetUpsert(true))
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	_, err = s.jobs.UpdateOne(ctx, bson.M{"_id": job.ID}, bson.M{
		"$set":   bson.M{"status": "succeeded", "finished_at": now},
		"$unset": bson.M{"dedupe_key": "", "error_message": ""},
	})
	return err
}

func (s *Store) MarkJobFailed(ctx context.Context, id primitive.ObjectID, cause error) error {
	message := cause.Error()
	if len(message) > 2000 {
		message = message[:2000]
	}
	_, err := s.jobs.UpdateOne(ctx, bson.M{"_id": id}, bson.M{
		"$set":   bson.M{"status": "failed", "finished_at": time.Now().UTC(), "error_message": message},
		"$unset": bson.M{"dedupe_key": ""},
	})
	return err
}

func (s *Store) RecoverStaleJobs(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-olderThan)
	result, err := s.jobs.UpdateMany(ctx, bson.M{"status": "running", "started_at": bson.M{"$lt": cutoff}}, bson.M{
		"$set":   bson.M{"status": "queued", "queued_at": time.Now().UTC()},
		"$unset": bson.M{"started_at": ""},
	})
	if err != nil {
		return 0, err
	}
	return result.ModifiedCount, nil
}

func (s *Store) Metrics(ctx context.Context, id primitive.ObjectID, from, to time.Time) ([]model.Metric, error) {
	filter := bson.M{"domain_id": id, "snapshot_date": bson.M{"$gte": from, "$lte": to}}
	cursor, err := s.metrics.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "snapshot_date", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var items []model.Metric
	if err := cursor.All(ctx, &items); err != nil {
		return nil, err
	}
	return items, nil
}

func (s *Store) LatestMetric(ctx context.Context, id primitive.ObjectID) (*model.Metric, error) {
	var metric model.Metric
	err := s.metrics.FindOne(ctx, bson.M{"domain_id": id}, options.FindOne().SetSort(bson.D{{Key: "snapshot_date", Value: -1}})).Decode(&metric)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	return &metric, err
}

func (s *Store) ListJobs(ctx context.Context, status string, limit int64) ([]model.CollectionJob, error) {
	filter := bson.M{}
	if status != "" {
		filter["status"] = status
	}
	cursor, err := s.jobs.Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "queued_at", Value: -1}}).SetLimit(limit))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var items []model.CollectionJob
	if err := cursor.All(ctx, &items); err != nil {
		return nil, err
	}
	return items, nil
}
