package store

import (
	"errors"
	"testing"

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
