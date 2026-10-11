package repository

import (
	"testing"
	"time"
)

func TestScanNullableTime(t *testing.T) {
	got, err := scanNullableTime(nil)
	if err != nil || got != nil {
		t.Fatalf("nil: got %v, %v", got, err)
	}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	got, err = scanNullableTime(now)
	if err != nil || got == nil || !got.Equal(now) {
		t.Fatalf("time.Time: got %v, %v", got, err)
	}
	if _, err = scanNullableTime("2026-01-02"); err == nil {
		t.Fatal("string: expected error")
	}
	if _, err = scanNullableTime(&now); err == nil {
		t.Fatal("*time.Time: expected error")
	}
}
