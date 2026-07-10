package collector

import (
	"testing"
	"time"
)

func TestSnapshotDateUsesConfiguredTimezone(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	instant := time.Date(2026, 7, 9, 17, 0, 0, 0, time.UTC)
	got := SnapshotDate(instant, location)
	want := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("SnapshotDate = %v, want %v", got, want)
	}
}
