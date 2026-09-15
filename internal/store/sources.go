package store

import (
	"context"
	"fmt"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"seo-monitor/internal/model"
)

// EnableIndependentSources must run before workers and HTTP handlers start.
// The legacy queue remains the weights queue; supplemental jobs have their own
// durable queue, deduplication, retries and recovery.
func (s *Store) EnableIndependentSources(ctx context.Context) (*Store, error) {
	extra := *s
	extra.supplement = nil
	extra.metricSource = "chinaz_supplement"
	extra.jobs = s.db.Collection("chinaz_supplement_jobs")
	_, err := extra.jobs.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "dedupe_key", Value: 1}}, Options: options.Index().SetName("uq_jobs_open").SetUnique(true).SetSparse(true)},
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "available_at", Value: 1}, {Key: "queued_at", Value: 1}}},
		{Keys: bson.D{{Key: "domain_id", Value: 1}, {Key: "snapshot_date", Value: -1}, {Key: "queued_at", Value: -1}}},
		{Keys: bson.D{{Key: "snapshot_date", Value: 1}}},
	})
	if err != nil {
		return nil, err
	}
	s.metricSource, s.supplement = "aizhan", &extra
	return &extra, nil
}

// BackfillSupplementJobs splits existing open jobs on upgrade even when
// QUEUE_ON_START=false. Completed supplemental jobs are never duplicated.
func (s *Store) BackfillSupplementJobs(ctx context.Context) error {
	if s.supplement == nil {
		return nil
	}
	cursor, err := s.jobs.Find(ctx, bson.M{"status": bson.M{"$in": bson.A{"queued", "running"}}})
	if err != nil {
		return err
	}
	defer cursor.Close(ctx)
	for cursor.Next(ctx) {
		var job model.CollectionJob
		if err := cursor.Decode(&job); err != nil {
			return err
		}
		domain, err := s.GetDomain(ctx, job.DomainID)
		if err == ErrNotFound || (err == nil && !domain.Active) {
			continue
		}
		if err != nil {
			return err
		}
		if _, _, err := s.supplement.queueDomain(ctx, job.DomainID, job.SnapshotDate, job.RequestedBy, false); err != nil {
			return err
		}
	}
	return cursor.Err()
}

var supplementalFields = []string{"apppc_pc_rank", "site_category", "registrant_name", "registrant_email", "expires_on"}

func (s *Store) ListSourceJobs(ctx context.Context, source, status string, limit int64) ([]model.CollectionJob, error) {
	switch source {
	case "", "aizhan", "chinaz":
		return s.ListJobs(ctx, status, limit)
	case "chinaz_supplement":
		if s.supplement == nil {
			return []model.CollectionJob{}, nil
		}
		return s.supplement.ListJobs(ctx, status, limit)
	default:
		return nil, ErrInvalidSearch
	}
}

var primaryFields = []string{"traffic_text", "traffic_min", "traffic_max", "baidu_pc_weight", "baidu_mobile_weight", "sogou_weight", "bing_weight", "so_360_weight", "shenma_weight", "pr_weight", "backlink_count", "domain_age_text", "domain_age_days", "collection_route"}

func sourceMetricUpdate(metric model.Metric, source string) (bson.M, error) {
	raw, err := bson.Marshal(metric)
	if err != nil {
		return nil, err
	}
	var values bson.M
	if err := bson.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	set := bson.M{"domain_id": metric.DomainID, "domain": metric.Domain, "snapshot_date": metric.SnapshotDate}
	unset := bson.M{}
	insert := bson.M{}
	fields := primaryFields
	switch source {
	case "aizhan":
		set["collected_at"], set["source_url"], set["raw_sha256"] = metric.CollectedAt, metric.SourceURL, metric.RawSHA256
	case "chinaz_supplement":
		fields = supplementalFields
		set["supplemental_collected_at"], set["supplemental_source_url"], set["supplemental_raw_sha256"] = metric.CollectedAt, metric.SourceURL, metric.RawSHA256
		// A supplement may arrive first. Its real provenance satisfies existing
		// strict validators until Aizhan writes the primary provenance.
		insert["collected_at"], insert["source_url"], insert["raw_sha256"] = metric.CollectedAt, metric.SourceURL, metric.RawSHA256
	default:
		return nil, fmt.Errorf("unknown metric source %q", source)
	}
	for _, key := range fields {
		if value, ok := values[key]; ok {
			set[key] = value
		} else {
			unset[key] = ""
		}
	}
	update := bson.M{"$set": set}
	if len(unset) > 0 {
		update["$unset"] = unset
	}
	if len(insert) > 0 {
		update["$setOnInsert"] = insert
	}
	return update, nil
}
