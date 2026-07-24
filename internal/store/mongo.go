package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"

	"seo-monitor/internal/model"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrConflict      = errors.New("conflict")
	ErrInvalidSearch = errors.New("invalid search")
)

type latestSearchField struct {
	Path    string
	Numeric bool
}

var latestSearchFields = map[string]latestSearchField{
	"domain":              {Path: "domain"},
	"display_name":        {Path: "display_name"},
	"site_category":       {Path: "metric.site_category"},
	"registrant_name":     {Path: "metric.registrant_name"},
	"registrant_email":    {Path: "metric.registrant_email"},
	"baidu_pc_weight":     {Path: "metric.baidu_pc_weight", Numeric: true},
	"baidu_mobile_weight": {Path: "metric.baidu_mobile_weight", Numeric: true},
	"sogou_weight":        {Path: "metric.sogou_weight", Numeric: true},
	"bing_weight":         {Path: "metric.bing_weight", Numeric: true},
	"so_360_weight":       {Path: "metric.so_360_weight", Numeric: true},
	"shenma_weight":       {Path: "metric.shenma_weight", Numeric: true},
	"pr_weight":           {Path: "metric.pr_weight", Numeric: true},
	"apppc_pc_rank":       {Path: "metric.apppc_pc_rank", Numeric: true},
	"backlink_count":      {Path: "metric.backlink_count", Numeric: true},
	"traffic_min":         {Path: "metric.traffic_min", Numeric: true},
	"traffic_max":         {Path: "metric.traffic_max", Numeric: true},
	"domain_age_days":     {Path: "metric.domain_age_days", Numeric: true},
}

type Store struct {
	client             *mongo.Client
	db                 *mongo.Database
	domains            *mongo.Collection
	metrics            *mongo.Collection
	jobs               *mongo.Collection
	certificates       *mongo.Collection
	certificateHistory *mongo.Collection
}

// CleanupResult reports how many records were removed from each retained collection.
type CleanupResult struct {
	MetricsDeleted int64
	JobsDeleted    int64
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
		client:             client,
		db:                 db,
		domains:            db.Collection("domains"),
		metrics:            db.Collection("domain_daily_metrics"),
		jobs:               db.Collection("collection_jobs"),
		certificates:       db.Collection("domain_certificates"),
		certificateHistory: db.Collection("domain_certificate_history"),
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
		{Keys: bson.D{{Key: "snapshot_date", Value: -1}}, Options: options.Index().SetName("ix_jobs_date")},
		{Keys: bson.D{{Key: "dedupe_key", Value: 1}}, Options: options.Index().SetName("uq_jobs_open").SetUnique(true).SetSparse(true)},
	})
	if err != nil {
		return fmt.Errorf("create job indexes: %w", err)
	}

	_, err = s.certificates.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "domain_id", Value: 1}}, Options: options.Index().SetName("uq_certificates_domain_id").SetUnique(true)},
		{Keys: bson.D{{Key: "expires_at", Value: 1}}, Options: options.Index().SetName("ix_certificates_expires")},
		{Keys: bson.D{{Key: "checked_at", Value: -1}}, Options: options.Index().SetName("ix_certificates_checked")},
	})
	if err != nil {
		return fmt.Errorf("create certificate indexes: %w", err)
	}
	_, err = s.certificateHistory.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "domain_id", Value: 1}, {Key: "check_date", Value: -1}}, Options: options.Index().SetName("ix_certificate_history_domain_date")},
		{Keys: bson.D{{Key: "check_date", Value: -1}}, Options: options.Index().SetName("ix_certificate_history_date")},
		{Keys: bson.D{{Key: "domain_id", Value: 1}, {Key: "checked_at", Value: -1}}, Options: options.Index().SetName("ix_certificate_history_domain_checked")},
	})
	if err != nil {
		return fmt.Errorf("create certificate history indexes: %w", err)
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

func (s *Store) QueueAll(ctx context.Context, snapshotDate time.Time, requestedBy string, force bool) (int, error) {
	domains, err := s.ListDomains(ctx, false)
	if err != nil {
		return 0, err
	}
	queued := 0
	for _, domain := range domains {
		_, added, queueErr := s.QueueDomain(ctx, domain.ID, snapshotDate, requestedBy, force)
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

// CleanupBefore removes historical metrics and collection/polling records whose
// snapshot date is strictly older than cutoff. The cutoff date itself is kept.
func (s *Store) CleanupBefore(ctx context.Context, cutoff time.Time) (CleanupResult, error) {
	var cleaned CleanupResult
	if cutoff.IsZero() {
		return cleaned, fmt.Errorf("cleanup cutoff must not be zero")
	}
	filter := snapshotDateBeforeFilter(cutoff)

	jobsResult, err := s.jobs.DeleteMany(ctx, filter)
	if err != nil {
		return cleaned, fmt.Errorf("delete expired collection jobs: %w", err)
	}
	cleaned.JobsDeleted = jobsResult.DeletedCount

	metricsResult, err := s.metrics.DeleteMany(ctx, filter)
	if err != nil {
		return cleaned, fmt.Errorf("delete expired daily metrics: %w", err)
	}
	cleaned.MetricsDeleted = metricsResult.DeletedCount
	return cleaned, nil
}

func snapshotDateBeforeFilter(cutoff time.Time) bson.M {
	return bson.M{"snapshot_date": bson.M{"$lt": cutoff}}
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

// SaveCertificate appends every polling attempt to history, then replaces the
// latest state used by the certificate dashboard.
func (s *Store) SaveCertificate(ctx context.Context, item model.Certificate) error {
	if item.CheckedAt.IsZero() {
		return fmt.Errorf("certificate checked_at must not be zero")
	}
	if item.CheckDate.IsZero() {
		year, month, day := item.CheckedAt.UTC().Date()
		item.CheckDate = time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
	}
	item.ID = primitive.NilObjectID
	_, err := s.certificateHistory.InsertOne(ctx, item)
	if err != nil {
		return fmt.Errorf("save certificate polling history: %w", err)
	}
	_, err = s.certificates.ReplaceOne(ctx, bson.M{"domain_id": item.DomainID}, item,
		options.Replace().SetUpsert(true))
	if err != nil {
		return fmt.Errorf("save latest certificate: %w", err)
	}
	return nil
}

// CertificateHistory returns retained polling results for a domain, newest
// first.
func (s *Store) CertificateHistory(ctx context.Context, id primitive.ObjectID, limit int64) ([]model.Certificate, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	cursor, err := s.certificateHistory.Find(ctx, bson.M{"domain_id": id},
		options.Find().SetSort(bson.D{{Key: "check_date", Value: -1}, {Key: "checked_at", Value: -1}}).SetLimit(limit))
	if err != nil {
		return nil, err
	}
	defer cursor.Close(ctx)
	var items []model.Certificate
	if err := cursor.All(ctx, &items); err != nil {
		return nil, err
	}
	if items == nil {
		items = []model.Certificate{}
	}
	return items, nil
}

// CleanupCertificateHistory keeps all polling results inside the normal
// retention window. Older failures are removed, while successful results keep
// an additional window ending at the most recent success for each domain. This
// preserves a week of known-good data during a prolonged outage without
// retaining every historical failure.
func (s *Store) CleanupCertificateHistory(ctx context.Context, cutoff time.Time, retentionDays int) (int64, error) {
	if cutoff.IsZero() {
		return 0, fmt.Errorf("certificate cleanup cutoff must not be zero")
	}
	if retentionDays < 1 {
		return 0, fmt.Errorf("certificate retention days must be positive")
	}

	oldFailures, err := s.certificateHistory.DeleteMany(ctx, bson.M{
		"check_date":    bson.M{"$type": "date", "$lt": cutoff},
		"error_message": bson.M{"$type": "string", "$ne": ""},
	})
	if err != nil {
		return 0, fmt.Errorf("delete expired certificate failures: %w", err)
	}
	deleted := oldFailures.DeletedCount

	cursor, err := s.certificateHistory.Aggregate(ctx, mongo.Pipeline{
		bson.D{{Key: "$match", Value: certificateHistorySuccessMatch()}},
		bson.D{{Key: "$group", Value: bson.M{
			"_id":            "$domain_id",
			"latest_success": bson.M{"$max": "$check_date"},
		}}},
	})
	if err != nil {
		return deleted, fmt.Errorf("find latest certificate successes: %w", err)
	}
	defer cursor.Close(ctx)
	var latestSuccesses []struct {
		DomainID      primitive.ObjectID `bson:"_id"`
		LatestSuccess time.Time          `bson:"latest_success"`
	}
	if err := cursor.All(ctx, &latestSuccesses); err != nil {
		return deleted, fmt.Errorf("decode latest certificate successes: %w", err)
	}

	models := make([]mongo.WriteModel, 0, len(latestSuccesses))
	for _, success := range latestSuccesses {
		successCutoff := certificateSuccessRetentionCutoff(cutoff, success.LatestSuccess, retentionDays)
		filter := certificateHistorySuccessMatch()
		filter["domain_id"] = success.DomainID
		filter["check_date"] = bson.M{"$type": "date", "$lt": successCutoff}
		models = append(models, mongo.NewDeleteManyModel().SetFilter(filter))
	}
	if len(models) == 0 {
		return deleted, nil
	}
	result, err := s.certificateHistory.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false))
	if err != nil {
		return deleted, fmt.Errorf("delete expired certificate successes: %w", err)
	}
	return deleted + result.DeletedCount, nil
}

func certificateHistorySuccessMatch() bson.M {
	return bson.M{
		"check_date": bson.M{"$type": "date"},
		"$or": bson.A{
			bson.M{"error_message": bson.M{"$exists": false}},
			bson.M{"error_message": nil},
			bson.M{"error_message": ""},
		},
	}
}

func certificateSuccessRetentionCutoff(cutoff, latestSuccess time.Time, retentionDays int) time.Time {
	if latestSuccess.IsZero() {
		return cutoff
	}
	successCutoff := latestSuccess.AddDate(0, 0, -retentionDays)
	if successCutoff.Before(cutoff) {
		return successCutoff
	}
	return cutoff
}

// ListCertificates returns active domains joined with their latest certificate
// check. Summary counts cover the complete query result before the optional
// status filter is applied, so dashboard cards remain independent of paging.
func (s *Store) ListCertificates(ctx context.Context, query, status string, page, limit int64) ([]model.LatestCertificate, int64, model.CertificateSummary, error) {
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 50
	}
	now := time.Now().UTC()
	statusMatch, err := certificateStatusMatch(status, now)
	if err != nil {
		return nil, 0, model.CertificateSummary{}, err
	}

	match := bson.M{"active": true}
	if query = strings.TrimSpace(query); query != "" {
		pattern := primitive.Regex{Pattern: regexp.QuoteMeta(query), Options: "i"}
		match["$or"] = bson.A{
			bson.M{"domain": pattern},
			bson.M{"display_name": pattern},
		}
	}

	itemsPipeline := mongo.Pipeline{}
	totalPipeline := mongo.Pipeline{}
	if len(statusMatch) > 0 {
		itemsPipeline = append(itemsPipeline, bson.D{{Key: "$match", Value: statusMatch}})
		totalPipeline = append(totalPipeline, bson.D{{Key: "$match", Value: statusMatch}})
	}
	itemsPipeline = append(itemsPipeline,
		bson.D{{Key: "$sort", Value: bson.D{{Key: "domain", Value: 1}}}},
		bson.D{{Key: "$skip", Value: (page - 1) * limit}},
		bson.D{{Key: "$limit", Value: limit}},
		bson.D{{Key: "$project", Value: bson.D{
			{Key: "_id", Value: 0},
			{Key: "domain_record", Value: bson.D{
				{Key: "_id", Value: "$_id"},
				{Key: "domain", Value: "$domain"},
				{Key: "display_name", Value: "$display_name"},
				{Key: "active", Value: "$active"},
				{Key: "created_at", Value: "$created_at"},
				{Key: "updated_at", Value: "$updated_at"},
				{Key: "archived_at", Value: "$archived_at"},
			}},
			{Key: "certificate", Value: "$certificate"},
		}}},
	)
	totalPipeline = append(totalPipeline, bson.D{{Key: "$count", Value: "value"}})

	pipeline := mongo.Pipeline{
		bson.D{{Key: "$match", Value: match}},
		bson.D{{Key: "$lookup", Value: bson.M{
			"from":         "domain_certificates",
			"localField":   "_id",
			"foreignField": "domain_id",
			"as":           "certificate_docs",
		}}},
		bson.D{{Key: "$set", Value: bson.M{
			"certificate": bson.M{"$ifNull": bson.A{
				bson.M{"$arrayElemAt": bson.A{"$certificate_docs", 0}}, nil,
			}},
		}}},
		bson.D{{Key: "$facet", Value: bson.D{
			{Key: "items", Value: itemsPipeline},
			{Key: "total", Value: totalPipeline},
			{Key: "summary", Value: mongo.Pipeline{
				bson.D{{Key: "$group", Value: bson.M{
					"_id":   nil,
					"total": bson.M{"$sum": 1},
					"checked": bson.M{"$sum": bson.M{"$cond": bson.A{
						bson.M{"$ne": bson.A{"$certificate", nil}}, 1, 0,
					}}},
					"expiring_soon": bson.M{"$sum": bson.M{"$cond": bson.A{
						certificateExpiryCondition(now, now.Add(30*24*time.Hour)), 1, 0,
					}}},
					"expired": bson.M{"$sum": bson.M{"$cond": bson.A{
						certificateExpiredCondition(now), 1, 0,
					}}},
					"failed": bson.M{"$sum": bson.M{"$cond": bson.A{
						bson.M{"$ne": bson.A{
							bson.M{"$ifNull": bson.A{"$certificate.error_message", ""}}, "",
						}}, 1, 0,
					}}},
				}}},
			}},
		}}},
	}

	cursor, err := s.domains.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, 0, model.CertificateSummary{}, err
	}
	defer cursor.Close(ctx)
	var result []struct {
		Items []model.LatestCertificate `bson:"items"`
		Total []struct {
			Value int64 `bson:"value"`
		} `bson:"total"`
		Summary []model.CertificateSummary `bson:"summary"`
	}
	if err := cursor.All(ctx, &result); err != nil {
		return nil, 0, model.CertificateSummary{}, err
	}
	if len(result) == 0 {
		return []model.LatestCertificate{}, 0, model.CertificateSummary{}, nil
	}
	total := int64(0)
	if len(result[0].Total) > 0 {
		total = result[0].Total[0].Value
	}
	summary := model.CertificateSummary{}
	if len(result[0].Summary) > 0 {
		summary = result[0].Summary[0]
	}
	return result[0].Items, total, summary, nil
}

func certificateStatusMatch(status string, now time.Time) (bson.D, error) {
	switch strings.TrimSpace(status) {
	case "":
		return nil, nil
	case "checked":
		return bson.D{{Key: "certificate", Value: bson.M{"$ne": nil}}}, nil
	case "expiring":
		return bson.D{{Key: "certificate.expires_at", Value: bson.M{
			"$type": "date",
			"$gte":  now,
			"$lte":  now.Add(30 * 24 * time.Hour),
		}}}, nil
	case "expired":
		return bson.D{{Key: "certificate.expires_at", Value: bson.M{"$type": "date", "$lt": now}}}, nil
	case "failed":
		return bson.D{{Key: "certificate.error_message", Value: bson.M{
			"$exists": true,
			"$type":   "string",
			"$ne":     "",
		}}}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported certificate status %q", ErrInvalidSearch, status)
	}
}

func certificateExpiryCondition(now, cutoff time.Time) bson.M {
	return bson.M{"$and": bson.A{
		bson.M{"$eq": bson.A{bson.M{"$type": "$certificate.expires_at"}, "date"}},
		bson.M{"$gte": bson.A{"$certificate.expires_at", now}},
		bson.M{"$lte": bson.A{"$certificate.expires_at", cutoff}},
	}}
}

func certificateExpiredCondition(now time.Time) bson.M {
	return bson.M{"$and": bson.A{
		bson.M{"$eq": bson.A{bson.M{"$type": "$certificate.expires_at"}, "date"}},
		bson.M{"$lt": bson.A{"$certificate.expires_at", now}},
	}}
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

// SearchLatest returns active domains with their newest metric snapshot. Only
// fields in latestSearchFields are accepted, preventing arbitrary MongoDB paths
// from being supplied by API callers.
func (s *Store) SearchLatest(ctx context.Context, field, query string, page, limit int64) ([]model.LatestMetric, int64, error) {
	match, err := latestSearchMatch(field, query)
	if err != nil {
		return nil, 0, err
	}
	if page < 1 {
		page = 1
	}
	if limit < 1 || limit > 100 {
		limit = 50
	}

	lookupStage := bson.D{{Key: "$lookup", Value: bson.M{
		"from": "domain_daily_metrics",
		"let":  bson.M{"domainID": "$_id"},
		"pipeline": mongo.Pipeline{
			bson.D{{Key: "$match", Value: bson.M{
				"$expr": bson.M{"$eq": bson.A{"$domain_id", "$$domainID"}},
			}}},
			bson.D{{Key: "$sort", Value: bson.M{"snapshot_date": -1}}},
			bson.D{{Key: "$limit", Value: 1}},
		},
		"as": "metric_docs",
	}}}
	setMetricStage := bson.D{{Key: "$set", Value: bson.M{
		"metric": bson.M{"$arrayElemAt": bson.A{"$metric_docs", 0}},
	}}}
	pipeline := mongo.Pipeline{
		bson.D{{Key: "$match", Value: bson.M{"active": true}}},
		lookupStage,
		setMetricStage,
	}
	if len(match) > 0 {
		pipeline = append(pipeline, bson.D{{Key: "$match", Value: match}})
	}
	pipeline = append(pipeline,
		bson.D{{Key: "$sort", Value: bson.D{{Key: "domain", Value: 1}}}},
		bson.D{{Key: "$facet", Value: bson.D{
			{Key: "items", Value: mongo.Pipeline{
				bson.D{{Key: "$skip", Value: (page - 1) * limit}},
				bson.D{{Key: "$limit", Value: limit}},
				bson.D{{Key: "$project", Value: bson.D{
					{Key: "_id", Value: 0},
					{Key: "domain_record", Value: bson.D{
						{Key: "_id", Value: "$_id"},
						{Key: "domain", Value: "$domain"},
						{Key: "display_name", Value: "$display_name"},
						{Key: "active", Value: "$active"},
						{Key: "created_at", Value: "$created_at"},
						{Key: "updated_at", Value: "$updated_at"},
						{Key: "archived_at", Value: "$archived_at"},
					}},
					{Key: "metric", Value: "$metric"},
				}}},
			}},
			{Key: "total", Value: mongo.Pipeline{
				bson.D{{Key: "$count", Value: "value"}},
			}},
		}}},
	)

	cursor, err := s.domains.Aggregate(ctx, pipeline)
	if err != nil {
		return nil, 0, err
	}
	defer cursor.Close(ctx)
	var result []struct {
		Items []model.LatestMetric `bson:"items"`
		Total []struct {
			Value int64 `bson:"value"`
		} `bson:"total"`
	}
	if err := cursor.All(ctx, &result); err != nil {
		return nil, 0, err
	}
	if len(result) == 0 {
		return []model.LatestMetric{}, 0, nil
	}
	total := int64(0)
	if len(result[0].Total) > 0 {
		total = result[0].Total[0].Value
	}
	return result[0].Items, total, nil
}

func latestSearchMatch(field, query string) (bson.D, error) {
	field = strings.TrimSpace(field)
	if field == "" {
		field = "domain"
	}
	spec, ok := latestSearchFields[field]
	if !ok {
		return nil, fmt.Errorf("%w: unsupported field %q", ErrInvalidSearch, field)
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	if spec.Numeric {
		value, err := strconv.ParseInt(query, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: field %s requires an integer", ErrInvalidSearch, field)
		}
		return bson.D{{Key: spec.Path, Value: value}}, nil
	}
	return bson.D{{Key: spec.Path, Value: primitive.Regex{Pattern: regexp.QuoteMeta(query), Options: "i"}}}, nil
}
