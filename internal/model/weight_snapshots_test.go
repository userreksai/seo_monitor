package model

import (
	"encoding/json"
	"go.mongodb.org/mongo-driver/bson"
	"testing"
	"time"
)

func TestSourceWeightMetricKeepsBothSourcesSeparate(t *testing.T) {
	a, c := int16(5), int16(0)
	valid := true
	metric := Metric{Domain: "example.com", SnapshotDate: time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC), WeightSource: "aizhan", WeightValid: &valid, BaiduPCWeight: &a,
		WeightSnapshots: map[string]WeightSnapshot{
			"aizhan": {Metric: &Metric{WeightSource: "aizhan", BaiduPCWeight: &a, BaiduMobile: &a}, Valid: true},
			"chinaz": {Metric: &Metric{WeightSource: "chinaz", BaiduPCWeight: &c, BaiduMobile: &c}, Valid: true},
		},
	}
	for source, want := range map[string]int16{"aizhan": 5, "chinaz": 0} {
		result, ok := metric.SourceWeightMetric(source)
		if !ok || result.WeightSource != source || result.WeightValid == nil || !*result.WeightValid || *result.BaiduPCWeight != want || result.Domain != metric.Domain || !result.SnapshotDate.Equal(metric.SnapshotDate) {
			t.Fatalf("%s: %+v", source, result)
		}
	}
	snapshot := metric.WeightSnapshots["aizhan"]
	snapshot.Valid = false
	snapshot.ErrorMessage = "timeout"
	metric.WeightSnapshots["aizhan"] = snapshot
	result, _ := metric.SourceWeightMetric("aizhan")
	if result.WeightValid == nil || *result.WeightValid || *result.BaiduPCWeight != 5 {
		t.Fatal("failed source must keep old data but invalidate it")
	}
	result, _ = metric.SourceWeightMetric("chinaz")
	if result.WeightValid == nil || !*result.WeightValid || *result.BaiduPCWeight != 0 {
		t.Fatal("other source invalidated")
	}
	if _, ok := metric.SourceWeightMetric("unknown"); ok {
		t.Fatal("unknown source accepted")
	}
	for _, marshal := range []func(any) ([]byte, error){json.Marshal, bson.Marshal} {
		if _, err := marshal(metric); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSourceWeightMetricMissingAndLegacy(t *testing.T) {
	metric := Metric{WeightSource: "aizhan"}
	if _, ok := metric.SourceWeightMetric("chinaz"); ok {
		t.Fatal("must not substitute the other site")
	}
	legacy, ok := metric.SourceWeightMetric("aizhan")
	if !ok || legacy.WeightValid != nil {
		t.Fatal("legacy validity must not be invented")
	}
	metric.WeightSnapshots = map[string]WeightSnapshot{"chinaz": {Valid: false, ErrorMessage: "timeout"}}
	failed, ok := metric.SourceWeightMetric("chinaz")
	if !ok || failed.BaiduPCWeight != nil || failed.WeightValid == nil || *failed.WeightValid {
		t.Fatal("missing source result fabricated")
	}
}
