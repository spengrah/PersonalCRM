package repository

import (
	"testing"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/require"
)

// TestConvertDbEvent_FullRow asserts every column round-trips from the
// sqlc-generated db.Event into the events.Envelope shape.
func TestConvertDbEvent_FullRow(t *testing.T) {
	id := uuid.New()
	observed := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	payload := []byte(`{"version":1}`)

	row := &db.Event{
		ID:         pgtype.UUID{Bytes: id, Valid: true},
		Source:     "telegram",
		SourceID:   pgtype.Text{String: "tg:1:2:42", Valid: true},
		Kind:       "message.received",
		Payload:    payload,
		ObservedAt: pgtype.Timestamptz{Time: observed, Valid: true},
	}

	env := convertDbEvent(row)
	require.Equal(t, id, env.ID)
	require.Equal(t, "telegram", env.Source)
	require.Equal(t, "tg:1:2:42", env.SourceID)
	require.Equal(t, "message.received", string(env.Kind))
	require.Equal(t, payload, []byte(env.Payload))
	require.Equal(t, observed, env.ObservedAt)
}

// TestConvertDbEvent_NullSourceID asserts that a NULL source_id column maps
// to an empty string on the envelope.
func TestConvertDbEvent_NullSourceID(t *testing.T) {
	row := &db.Event{
		ID:         pgtype.UUID{Bytes: uuid.New(), Valid: true},
		Source:     "manual",
		SourceID:   pgtype.Text{Valid: false},
		Kind:       "interaction.manual",
		Payload:    []byte(`{"version":1}`),
		ObservedAt: pgtype.Timestamptz{Time: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC), Valid: true},
	}
	env := convertDbEvent(row)
	require.Equal(t, "", env.SourceID)
}

// TestConvertDbEvent_InvalidTimestamp asserts that an invalid observed_at
// column (Valid=false) yields a zero-value time.Time on the envelope.
// Safety net: the DB column is NOT NULL in the migration, so this should
// never happen in practice, but covering the path prevents a nil panic if
// sqlc semantics ever shift.
func TestConvertDbEvent_InvalidTimestamp(t *testing.T) {
	row := &db.Event{
		ID:         pgtype.UUID{Bytes: uuid.New(), Valid: true},
		Source:     "telegram",
		Kind:       "message.received",
		Payload:    []byte(`{"version":1}`),
		ObservedAt: pgtype.Timestamptz{Valid: false},
	}
	env := convertDbEvent(row)
	require.True(t, env.ObservedAt.IsZero())
}

// TestConvertDbEvent_InvalidID asserts that an invalid id column yields
// uuid.Nil on the envelope. Same defensive-only rationale as the timestamp
// test above.
func TestConvertDbEvent_InvalidID(t *testing.T) {
	row := &db.Event{
		ID:         pgtype.UUID{Valid: false},
		Source:     "telegram",
		Kind:       "message.received",
		Payload:    []byte(`{"version":1}`),
		ObservedAt: pgtype.Timestamptz{Time: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC), Valid: true},
	}
	env := convertDbEvent(row)
	require.Equal(t, uuid.Nil, env.ID)
}
