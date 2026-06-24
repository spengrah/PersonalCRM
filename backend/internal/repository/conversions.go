package repository

import (
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

func uuidToPgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: true}
}

func stringToPgText(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{Valid: false}
	}
	return pgtype.Text{String: *s, Valid: true}
}

func timeToPgDate(t *time.Time) pgtype.Date {
	if t == nil {
		return pgtype.Date{Valid: false}
	}
	return pgtype.Date{Time: *t, Valid: true}
}

func timeToPgTimestamptz(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{Valid: false}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

// pgTimestamptzToTimePtr converts a pgtype.Timestamptz to *time.Time;
// invalid/NULL → nil. Returned time is normalized to UTC.
func pgTimestamptzToTimePtr(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	u := t.Time.UTC()
	return &u
}

// pgDateToTimePtr converts a pgtype.Date to *time.Time; invalid/NULL → nil.
// Returned time is normalized to UTC with day precision preserved.
func pgDateToTimePtr(d pgtype.Date) *time.Time {
	if !d.Valid {
		return nil
	}
	u := d.Time.UTC()
	return &u
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
