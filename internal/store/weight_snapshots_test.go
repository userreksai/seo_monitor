package store

import (
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"

	"seo-monitor/internal/model"
)

func TestCombinedWeightProgressIncludesBothQueues(t *testing.T) {
	a := model.CollectionProgress{Total: 498, Completed: 498, Succeeded: 498, Supplement: &model.CollectionProgress{Total: 498}}
	c := model.CollectionProgress{Total: 498, Completed: 490, Succeeded: 486, Failed: 4, Queued: 8, InProgress: true}
	p := combinedWeightProgress(a, c)
	if p.Total != 996 || p.Completed != 988 || p.Succeeded != 984 || p.Failed != 4 || p.Queued != 8 || !p.InProgress {
		t.Fatalf("%+v", p)
	}
	if p.Sources["aizhan"].InProgress || !p.Sources["chinaz"].InProgress || p.Supplement == nil {
		t.Fatal("source progress lost")
	}
	if p.Sources["aizhan"].Supplement != nil || p.Sources["aizhan"].Sources != nil {
		t.Fatal("recursive progress")
	}
}

func TestSnapshotMetricValuesOnlyContainsOwnWeights(t *testing.T) {
	zero := int16(0)
	category := "must stay independent"
	metric := model.Metric{WeightSource: "chinaz", BaiduPCWeight: &zero, BaiduMobile: &zero, SiteCategory: &category, WeightSnapshots: map[string]model.WeightSnapshot{"aizhan": {Valid: true}}}
	metric.MarkWeights("chinaz")
	values, err := snapshotMetricValues(metric)
	if err != nil {
		t.Fatal(err)
	}
	if values["baidu_pc_weight"] != int32(0) || values["weight_source"] != "chinaz" || values["weight_valid"] != true {
		t.Fatal(values)
	}
	for _, key := range []string{"site_category", "weight_snapshots", "supplemental_source_url"} {
		if _, ok := values[key]; ok {
			t.Fatalf("foreign field %s retained", key)
		}
	}
}

func TestWeightUpdatesValidateSourcesAndEncode(t *testing.T) {
	if _, err := weightSnapshotUpdate(model.Metric{WeightSource: "unknown"}, false); err == nil {
		t.Fatal("invalid source accepted")
	}
	if _, err := weightFailureUpdate("chinaz_supplement", time.Now(), "error"); err == nil {
		t.Fatal("supplement failure can invalidate weights")
	}
	metric := model.Metric{WeightSource: "chinaz", TrafficText: new(string)}
	*metric.TrafficText = "$untrusted-expression"
	success, err := weightSnapshotUpdate(metric, false)
	if err != nil {
		t.Fatal(err)
	}
	failure, err := weightFailureUpdate("aizhan", time.Now(), "$untrusted-error")
	if err != nil {
		t.Fatal(err)
	}
	for _, pipeline := range []any{success, failure, preferredCollectionStages()} {
		if _, err := bson.Marshal(bson.M{"pipeline": pipeline}); err != nil {
			t.Fatal(err)
		}
	}
	values := success[1][0].Value.(bson.M)["weight_snapshots.chinaz"].(bson.M)["$literal"].(bson.M)
	if values["metric"].(bson.M)["traffic_text"] != "$untrusted-expression" {
		t.Fatal("source values must be literal data")
	}
}
