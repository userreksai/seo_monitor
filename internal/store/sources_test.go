package store

import (
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"reflect"
	"seo-monitor/internal/model"
	"strings"
	"testing"
	"time"
)

func applySourceUpdate(doc bson.M, update bson.M) {
	if len(doc) == 0 {
		for k, v := range update["$setOnInsert"].(bson.M) {
			doc[k] = v
		}
	}
	for k, v := range update["$set"].(bson.M) {
		doc[k] = v
	}
	if unset, ok := update["$unset"].(bson.M); ok {
		for k := range unset {
			delete(doc, k)
		}
	}
}

func TestIndependentMetricWritesBothOrders(t *testing.T) {
	pc := int16(4)
	rank := int64(12)
	name := "owner"
	primary := model.Metric{DomainID: primitive.NewObjectID(), Domain: "example.com", SnapshotDate: time.Now().UTC().Truncate(24 * time.Hour), CollectedAt: time.Now().UTC(), SourceURL: "https://www.aizhan.com/cha/example.com/", RawSHA256: strings.Repeat("a", 64), BaiduPCWeight: &pc}
	extra := primary
	extra.SourceURL = "https://seo.chinaz.com/example.com"
	extra.RawSHA256 = strings.Repeat("b", 64)
	extra.APPPCPCrank = &rank
	extra.RegistrantName = &name
	// Chinaz parser also returns weights; ownership must prevent them leaking.
	wrong := int16(9)
	extra.BaiduPCWeight = &wrong
	a, err := sourceMetricUpdate(primary, "aizhan")
	if err != nil {
		t.Fatal(err)
	}
	c, err := sourceMetricUpdate(extra, "chinaz_supplement")
	if err != nil {
		t.Fatal(err)
	}
	var results []bson.M
	for _, updates := range [][]bson.M{{a, c}, {c, a}} {
		doc := bson.M{}
		for _, u := range updates {
			// Validate no path overlaps between MongoDB update operators.
			paths := map[string]bool{}
			for _, op := range []string{"$set", "$unset", "$setOnInsert"} {
				if fields, ok := u[op].(bson.M); ok {
					for k := range fields {
						if paths[k] {
							t.Fatalf("conflicting update path %s", k)
						}
						paths[k] = true
					}
				}
			}
			if len(doc) == 0 && u["$setOnInsert"] == nil {
				u["$setOnInsert"] = bson.M{}
			}
			applySourceUpdate(doc, u)
			for _, required := range []string{"domain_id", "domain", "snapshot_date", "collected_at", "source_url", "raw_sha256"} {
				if _, ok := doc[required]; !ok {
					t.Fatalf("validator field missing: %s", required)
				}
			}
		}
		if doc["baidu_pc_weight"] != int32(4) || doc["apppc_pc_rank"] != int64(12) || doc["registrant_name"] != "owner" {
			t.Fatalf("cross-source overwrite: %+v", doc)
		}
		if doc["source_url"] != primary.SourceURL || doc["supplemental_source_url"] != extra.SourceURL {
			t.Fatal("provenance mixed")
		}
		results = append(results, doc)
	}
	if !reflect.DeepEqual(results[0], results[1]) {
		t.Fatal("completion order changes snapshot")
	}
	// A real zero replaces prior weight while retaining supplementary values.
	zero := int16(0)
	primary.BaiduPCWeight = &zero
	u, _ := sourceMetricUpdate(primary, "aizhan")
	applySourceUpdate(results[0], u)
	if results[0]["baidu_pc_weight"] != int32(0) || results[0]["registrant_name"] != "owner" {
		t.Fatal("force refresh clobbered other source")
	}
	// Explicit absence clears only fields owned by that successful source.
	extra.RegistrantName = nil
	u, _ = sourceMetricUpdate(extra, "chinaz_supplement")
	applySourceUpdate(results[0], u)
	if _, ok := results[0]["registrant_name"]; ok {
		t.Fatal("old missing field retained")
	}
	if results[0]["baidu_pc_weight"] != int32(0) {
		t.Fatal("supplement removed primary weight")
	}
}
