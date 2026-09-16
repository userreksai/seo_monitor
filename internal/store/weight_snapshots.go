package store

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"

	"seo-monitor/internal/model"
)

func literal(value any) bson.M             { return bson.M{"$literal": value} }
func choose(condition, yes, no any) bson.M { return bson.M{"$cond": bson.A{condition, yes, no}} }
func otherwise(value, fallback any) bson.M { return bson.M{"$ifNull": bson.A{value, fallback}} }

func snapshotMetricFields() []string {
	fields := append([]string{}, primaryFields...)
	return append(fields, "domain_id", "domain", "snapshot_date", "collected_at", "source_url", "raw_sha256", "weight_source", "weight_valid")
}

func snapshotMetricValues(metric model.Metric) (bson.M, error) {
	raw, err := bson.Marshal(metric)
	if err != nil {
		return nil, err
	}
	var values bson.M
	if err = bson.Unmarshal(raw, &values); err != nil {
		return nil, err
	}
	result := bson.M{}
	for _, key := range snapshotMetricFields() {
		if value, ok := values[key]; ok {
			result[key] = value
		}
	}
	return result, nil
}

// Preserve a legacy selected result before either source overwrites the view.
// This runs in the same MongoDB update as every write; background migration is
// not a prerequisite for safely collecting after an upgrade.
func bootstrapWeightSnapshots() bson.D {
	fields := bson.M{}
	for _, source := range []string{"aizhan", "chinaz"} {
		values := bson.M{}
		for _, key := range snapshotMetricFields() {
			values[key] = "$" + key
		}
		values["weight_source"] = literal(source)
		// Very old records may not yet have been labeled by the background
		// migration. Preserve them only when URL and real core weights identify
		// the source; missing validity remains false, never assumed successful.
		host := "www[.]aizhan[.]com"
		if source == "chinaz" {
			host = "seo[.]chinaz[.]com"
		}
		legacyChecks := bson.A{
			bson.M{"$eq": bson.A{otherwise("$weight_source", ""), ""}},
			bson.M{"$regexMatch": bson.M{"input": otherwise("$source_url", ""), "regex": "^https?://" + host + "(?:[:/]|$)"}},
		}
		for _, field := range []string{"$baidu_pc_weight", "$baidu_mobile_weight"} {
			legacyChecks = append(legacyChecks, bson.M{"$isNumber": field}, bson.M{"$gte": bson.A{field, 0}}, bson.M{"$lte": bson.A{field, 10}})
		}
		matches := bson.M{"$or": bson.A{bson.M{"$eq": bson.A{"$weight_source", source}}, bson.M{"$and": legacyChecks}}}
		old := bson.M{"metric": values, "valid": bson.M{"$eq": bson.A{"$weight_valid", true}}, "last_attempt_at": "$collected_at"}
		path := "weight_snapshots." + source
		fields[path] = otherwise("$"+path, choose(matches, old, "$$REMOVE"))
	}
	return bson.D{{Key: "$set", Value: fields}}
}

// Each update selects the preferred result from the document's current source
// snapshots. MongoDB executes the stages atomically, so completion order cannot
// make Chinaz overwrite a valid Aizhan result or drop the other source's data.
func preferredWeightProjection() mongo.Pipeline {
	selected := choose(bson.M{"$eq": bson.A{"$weight_snapshots.aizhan.valid", true}}, "$weight_snapshots.aizhan.metric",
		choose(bson.M{"$eq": bson.A{"$weight_snapshots.chinaz.valid", true}}, "$weight_snapshots.chinaz.metric", nil))
	valid := bson.M{"$ne": bson.A{otherwise("$_preferred_weight", nil), nil}}
	fields := bson.M{"weight_valid": valid}
	for _, key := range append(append([]string{}, primaryFields...), "collected_at", "source_url", "raw_sha256", "weight_source") {
		fields[key] = choose(valid, otherwise("$_preferred_weight."+key, "$$REMOVE"), otherwise("$"+key, "$$REMOVE"))
	}
	return mongo.Pipeline{
		bson.D{{Key: "$set", Value: bson.M{"_preferred_weight": selected}}},
		bson.D{{Key: "$set", Value: fields}},
		bson.D{{Key: "$unset", Value: "_preferred_weight"}},
	}
}

func weightSnapshotUpdate(metric model.Metric, full bool) (mongo.Pipeline, error) {
	if metric.WeightSource != "aizhan" && metric.WeightSource != "chinaz" {
		return nil, fmt.Errorf("invalid weight source %q", metric.WeightSource)
	}
	values, err := snapshotMetricValues(metric)
	if err != nil {
		return nil, err
	}
	valid := metric.WeightValid != nil && *metric.WeightValid && metric.HasBaiduWeights()
	values["weight_valid"] = valid
	snapshot := bson.M{"metric": values, "valid": valid, "last_attempt_at": metric.CollectedAt}
	fields := bson.M{
		"domain_id": literal(metric.DomainID), "domain": literal(metric.Domain), "snapshot_date": literal(metric.SnapshotDate),
		"weight_snapshots." + metric.WeightSource: literal(snapshot),
	}
	// Required legacy validator fields also exist if a partial result is saved.
	for _, key := range []string{"collected_at", "source_url", "raw_sha256"} {
		fields[key] = otherwise("$"+key, literal(values[key]))
	}
	if full {
		raw, _ := bson.Marshal(metric)
		var all bson.M
		_ = bson.Unmarshal(raw, &all)
		for _, key := range supplementalFields {
			if value, ok := all[key]; ok {
				fields[key] = literal(value)
			} else {
				fields[key] = "$$REMOVE"
			}
		}
		fields["supplemental_source_url"] = literal(metric.SourceURL)
		fields["supplemental_raw_sha256"] = literal(metric.RawSHA256)
		fields["supplemental_collected_at"] = literal(metric.CollectedAt)
	}
	pipeline := mongo.Pipeline{bootstrapWeightSnapshots(), bson.D{{Key: "$set", Value: fields}}}
	return append(pipeline, preferredWeightProjection()...), nil
}

func weightFailureUpdate(source string, attemptedAt time.Time, message string) (mongo.Pipeline, error) {
	if source != "aizhan" && source != "chinaz" {
		return nil, fmt.Errorf("invalid weight source %q", source)
	}
	path := "weight_snapshots." + source
	// Keep the last successful source metric for inspection; Valid belongs to
	// the latest attempt. A failure never clears another source's snapshot.
	snapshot := bson.M{"$mergeObjects": bson.A{otherwise("$"+path, bson.M{}), literal(bson.M{"valid": false, "last_attempt_at": attemptedAt, "error_message": message})}}
	pipeline := mongo.Pipeline{bootstrapWeightSnapshots(), bson.D{{Key: "$set", Value: bson.M{path: snapshot}}}}
	return append(pipeline, preferredWeightProjection()...), nil
}

// Only known legacy sources can be migrated. Missing historical sources cannot
// be reconstructed, and concurrent collections take precedence over backfill.
func (s *Store) InitializeWeightSnapshots(ctx context.Context) (int64, error) {
	filter := bson.M{"$or": bson.A{
		bson.M{"weight_source": "aizhan", "weight_snapshots.aizhan": bson.M{"$exists": false}},
		bson.M{"weight_source": "chinaz", "weight_snapshots.chinaz": bson.M{"$exists": false}},
	}}
	cursor, err := s.metrics.Find(ctx, filter)
	if err != nil {
		return 0, err
	}
	defer cursor.Close(ctx)
	var count int64
	for cursor.Next(ctx) {
		var metric model.Metric
		if err := cursor.Decode(&metric); err != nil {
			return count, err
		}
		values, err := snapshotMetricValues(metric)
		if err != nil {
			return count, err
		}
		valid := metric.WeightValid != nil && *metric.WeightValid && metric.HasBaiduWeights()
		path := "weight_snapshots." + metric.WeightSource
		result, err := s.metrics.UpdateOne(ctx, bson.M{"_id": metric.ID, path: bson.M{"$exists": false}, "weight_source": metric.WeightSource, "collected_at": metric.CollectedAt, "weight_valid": metric.WeightValid},
			bson.M{"$set": bson.M{path: bson.M{"metric": values, "valid": valid, "last_attempt_at": metric.CollectedAt}}})
		if err != nil {
			return count, err
		}
		count += result.ModifiedCount
	}
	return count, cursor.Err()
}

func combinedWeightProgress(aizhan, chinaz model.CollectionProgress) model.CollectionProgress {
	result := aizhan
	primary := aizhan
	primary.Supplement = nil
	primary.Sources = nil
	result.Sources = map[string]*model.CollectionProgress{"aizhan": &primary, "chinaz": &chinaz}
	result.Total += chinaz.Total
	result.Completed += chinaz.Completed
	result.Pending += chinaz.Pending
	result.Queued += chinaz.Queued
	result.Running += chinaz.Running
	result.Succeeded += chinaz.Succeeded
	result.Failed += chinaz.Failed
	result.Canceled += chinaz.Canceled
	result.InProgress = aizhan.InProgress || chinaz.InProgress
	return result
}

// The existing UI's collection status follows the displayed source. Keep both
// source job states available so callers can also show independent failures.
func preferredCollectionStages() mongo.Pipeline {
	return mongo.Pipeline{
		bson.D{{Key: "$lookup", Value: bson.M{
			"from": "chinaz_weight_jobs", "let": bson.M{"domainID": "$_id"},
			"pipeline": mongo.Pipeline{
				bson.D{{Key: "$match", Value: bson.M{"$expr": bson.M{"$eq": bson.A{"$domain_id", "$$domainID"}}}}},
				bson.D{{Key: "$sort", Value: bson.D{{Key: "queued_at", Value: -1}}}},
				bson.D{{Key: "$limit", Value: 1}},
			}, "as": "chinaz_collection_docs",
		}}},
		bson.D{{Key: "$set", Value: bson.M{
			"weight_collections": bson.M{"aizhan": "$collection", "chinaz": bson.M{"$arrayElemAt": bson.A{"$chinaz_collection_docs", 0}}},
			"collection":         choose(bson.M{"$eq": bson.A{"$metric.weight_source", "chinaz"}}, otherwise(bson.M{"$arrayElemAt": bson.A{"$chinaz_collection_docs", 0}}, "$collection"), "$collection"),
		}}},
	}
}
