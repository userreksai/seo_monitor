package store

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestLatestSearchMatchText(t *testing.T) {
	match, err := latestSearchMatch("domain", "Example.COM")
	if err != nil {
		t.Fatal(err)
	}
	if len(match) != 1 || match[0].Key != "domain" {
		t.Fatalf("unexpected match: %#v", match)
	}
	regex, ok := match[0].Value.(primitive.Regex)
	if !ok || regex.Pattern != "Example\\.COM" || regex.Options != "i" {
		t.Fatalf("unexpected regex: %#v", match[0].Value)
	}
}

func TestLatestSearchMatchNumeric(t *testing.T) {
	match, err := latestSearchMatch("baidu_pc_weight", "2")
	if err != nil {
		t.Fatal(err)
	}
	if len(match) != 1 || match[0].Key != "metric.baidu_pc_weight" || match[0].Value != int64(2) {
		t.Fatalf("unexpected match: %#v", match)
	}
}

func TestLatestSearchMatchValidation(t *testing.T) {
	if _, err := latestSearchMatch("unknown", "value"); !errors.Is(err, ErrInvalidSearch) {
		t.Fatalf("expected invalid field error, got %v", err)
	}
	if _, err := latestSearchMatch("traffic_max", "abc"); !errors.Is(err, ErrInvalidSearch) {
		t.Fatalf("expected invalid numeric error, got %v", err)
	}
}

func TestSnapshotDateBeforeFilterKeepsCutoffDate(t *testing.T) {
	cutoff := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	got := snapshotDateBeforeFilter(cutoff)
	want := bson.M{"snapshot_date": bson.M{"$lt": cutoff}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshotDateBeforeFilter = %#v, want %#v", got, want)
	}
}

func TestCertificateStatusMatch(t *testing.T) {
	now := time.Date(2026, 7, 22, 3, 0, 0, 0, time.UTC)
	tests := []struct {
		status string
		want   bson.D
	}{
		{"", nil},
		{"checked", bson.D{{Key: "certificate", Value: bson.M{"$ne": nil}}}},
		{"expiring", bson.D{{Key: "certificate.expires_at", Value: bson.M{
			"$type": "date", "$gte": now, "$lte": now.Add(30 * 24 * time.Hour),
		}}}},
		{"expired", bson.D{{Key: "certificate.expires_at", Value: bson.M{"$type": "date", "$lt": now}}}},
		{"failed", bson.D{{Key: "certificate.error_message", Value: bson.M{
			"$exists": true, "$type": "string", "$ne": "",
		}}}},
	}
	for _, test := range tests {
		got, err := certificateStatusMatch(test.status, now)
		if err != nil {
			t.Fatalf("certificateStatusMatch(%q): %v", test.status, err)
		}
		if !reflect.DeepEqual(got, test.want) {
			t.Fatalf("certificateStatusMatch(%q) = %#v, want %#v", test.status, got, test.want)
		}
	}
	if _, err := certificateStatusMatch("unknown", now); !errors.Is(err, ErrInvalidSearch) {
		t.Fatalf("expected invalid status error, got %v", err)
	}
}

func TestCertificateExpiredConditionRequiresDate(t *testing.T) {
	now := time.Date(2026, 7, 22, 3, 0, 0, 0, time.UTC)
	want := bson.M{"$and": bson.A{
		bson.M{"$eq": bson.A{bson.M{"$type": "$certificate.expires_at"}, "date"}},
		bson.M{"$lt": bson.A{"$certificate.expires_at", now}},
	}}
	if got := certificateExpiredCondition(now); !reflect.DeepEqual(got, want) {
		t.Fatalf("certificateExpiredCondition = %#v, want %#v", got, want)
	}
}
