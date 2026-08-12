package repository

import (
	"time"
)

// deref returns the pointed-to value, or the zero value when p is nil. Reads
// of nullable columns that historically flattened SQL NULL to the zero value
// keep that behaviour through this helper.
func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

// utcPtr normalises a nullable timestamp to UTC without changing the instant,
// returning a fresh pointer (never the input). Timestamps read from the
// database carry the session's location; comparisons with == and
// reflect.DeepEqual depend on Location, so reads normalise here.
func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// nilIfEmpty maps an empty string to nil (SQL NULL) and a non-empty string to
// its pointer. Callers bind optional predicates and titles through this so an
// empty string never reaches a WHERE clause that expects NULL semantics.
func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// jsonbOrEmpty is the JSONB default for a nil patch/detail/config. A nil []byte
// sent to a NOT NULL JSONB column inserts SQL NULL (NOT the column DEFAULT), so
// the repository substitutes '{}' to preserve the table contract.
func jsonbOrEmpty(b []byte) []byte {
	if len(b) == 0 {
		return []byte("{}")
	}
	return b
}
