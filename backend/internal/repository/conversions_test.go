package repository

import (
	"testing"
	"time"
)

func TestUtcPtr(t *testing.T) {
	t.Parallel()

	if got := utcPtr(nil); got != nil {
		t.Fatalf("utcPtr(nil) = %v, want nil", got)
	}

	zone := time.FixedZone("UTC+7", 7*60*60)
	in := time.Date(2026, 3, 14, 15, 9, 26, 535000000, zone)
	p := &in
	got := utcPtr(p)
	if got == nil {
		t.Fatal("utcPtr(non-nil) = nil")
	}
	if got == p {
		t.Fatal("utcPtr returned the input pointer; want a fresh pointer")
	}
	if got.Location() != time.UTC {
		t.Fatalf("Location = %v, want UTC", got.Location())
	}
	if !got.Equal(in) {
		t.Fatalf("instant changed: %v != %v", got, in)
	}
}
