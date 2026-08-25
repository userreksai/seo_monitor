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

func TestRetentionCutoffKeepsBoundaryDay(t *testing.T) {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	instant := time.Date(2026, 7, 9, 17, 0, 0, 0, time.UTC)
	got := RetentionCutoff(instant, location, 60)
	want := time.Date(2026, 5, 11, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("RetentionCutoff = %v, want %v", got, want)
	}
}

func TestRetryDelaySchedule(t *testing.T) {
	delays := []time.Duration{10 * time.Minute, 30 * time.Minute, time.Hour}
	for attempt, want := range delays {
		got, retry := retryDelay(attempt+1, delays)
		if !retry || got != want {
			t.Fatalf("retryDelay(%d) = %v, %v; want %v, true", attempt+1, got, retry, want)
		}
	}
	if delay, retry := retryDelay(4, delays); retry || delay != 0 {
		t.Fatalf("fourth failure must be final, got %v, %v", delay, retry)
	}
}
