package scraper

import (
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
)

func TestAizhanMongoDocumentPreservesZeroAndMissing(t *testing.T) {
	body := strings.ReplaceAll(string(aizhanFixture(t)), "/br/9.png", "/br/0.png")
	m, err := ParseAizhan([]byte(body), "www.baidu.com")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := bson.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var doc bson.M
	if err := bson.Unmarshal(encoded, &doc); err != nil {
		t.Fatal(err)
	}
	if value, ok := doc["baidu_pc_weight"]; !ok || value != int32(0) {
		t.Fatalf("zero was lost: %v", value)
	}
	for _, field := range []string{"shenma_weight", "registrant_name", "registrant_email", "expires_on", "site_category", "apppc_pc_rank"} {
		if _, ok := doc[field]; ok {
			t.Errorf("missing field %s was fabricated", field)
		}
	}
}
