package repository

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// stubTx is a pgx.Tx satisfier used by repository tests that want to
// exercise validation guards without constructing a real transaction.
// The embedded pgx.Tx is nil; methods are never called because the
// validation check under test fires before any tx method runs.
type stubTx struct{ pgx.Tx }

// TestConvertDbEvent_FullRow asserts every column round-trips from the
// sqlc-generated db.Event into the events.Envelope shape.
func TestConvertDbEvent_FullRow(t *testing.T) {
	id := uuid.New()
	observed := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	payload := []byte(`{"version":1}`)
	sourceID := "tg:1:2:42"

	row := &db.Event{
		ID:         id,
		Source:     "telegram",
		SourceID:   &sourceID,
		Kind:       "message.received",
		Payload:    payload,
		ObservedAt: observed,
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
		ID:         uuid.New(),
		Source:     "manual",
		SourceID:   nil,
		Kind:       "interaction.manual",
		Payload:    []byte(`{"version":1}`),
		ObservedAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	}
	env := convertDbEvent(row)
	require.Equal(t, "", env.SourceID)
}

// TestConvertDbEvent_InvalidTimestamp asserts that an omitted observed_at
// column yields a zero-value time.Time on the envelope. The sqlc type
// override maps NOT NULL timestamptz to plain time.Time, so there is no
// more Valid=false state to construct — the zero time.Time is now the
// direct representation of "no value set" and this test covers that the
// conversion doesn't panic on it.
func TestConvertDbEvent_InvalidTimestamp(t *testing.T) {
	row := &db.Event{
		ID:      uuid.New(),
		Source:  "telegram",
		Kind:    "message.received",
		Payload: []byte(`{"version":1}`),
		// ObservedAt intentionally omitted (zero time.Time).
	}
	env := convertDbEvent(row)
	require.True(t, env.ObservedAt.IsZero())
}

// TestConvertDbEvent_InvalidID asserts that an omitted id column yields
// uuid.Nil on the envelope. Same rationale as the timestamp test above —
// the override maps NOT NULL uuid to plain uuid.UUID, so uuid.Nil is now
// the direct representation of "no value set".
func TestConvertDbEvent_InvalidID(t *testing.T) {
	row := &db.Event{
		// ID intentionally omitted (uuid.Nil).
		Source:     "telegram",
		Kind:       "message.received",
		Payload:    []byte(`{"version":1}`),
		ObservedAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	}
	env := convertDbEvent(row)
	require.Equal(t, uuid.Nil, env.ID)
}

// TestInsertEvent_NilTx_ReturnsError is a regression guard: InsertEvent is
// a public boundary method that later calls db.New(tx) which dereferences
// tx. A nil tx must be rejected with an error, not a panic.
func TestInsertEvent_NilTx_ReturnsError(t *testing.T) {
	// Queries doesn't matter — the nil-tx check fires before anything else.
	repo := NewEventRepository(nil)
	env := &events.Envelope{
		Source:     "telegram",
		Kind:       events.KindInteractionManual,
		Payload:    []byte(`{"version":1}`),
		ObservedAt: time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC),
	}
	err := repo.InsertEvent(context.Background(), nil, env)
	require.Error(t, err)
	require.Contains(t, err.Error(), "nil tx")
}

// TestInsertEvent_NilEnvelope_ReturnsError is the sibling guard for nil
// envelopes. Uses a typed-nil pgx.Tx (non-nil interface) so the nil-tx
// check passes and we hit the envelope check.
func TestInsertEvent_NilEnvelope_ReturnsError(t *testing.T) {
	repo := NewEventRepository(nil)
	err := repo.InsertEvent(context.Background(), stubTx{}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "nil envelope")
}
