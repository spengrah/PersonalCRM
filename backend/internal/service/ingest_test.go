package service

import (
	"context"
	"testing"

	"personal-crm/backend/internal/events"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestIngestService_EmptyBatch_FastPath ensures the empty-slice case does
// NOT open a transaction — it returns (0, 0, nil) immediately. Passing a
// nil database + nil bus would panic if the code tried to Begin on the
// pool, so the test proves the short-circuit works.
func TestIngestService_EmptyBatch_FastPath(t *testing.T) {
	svc := &IngestService{} // no DB, no bus — fast path must not touch them
	accepted, duplicate, err := svc.IngestBatch(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, 0, accepted)
	require.Equal(t, 0, duplicate)

	// Same for an explicit zero-length slice.
	accepted, duplicate, err = svc.IngestBatch(context.Background(), []*events.Envelope{})
	require.NoError(t, err)
	require.Equal(t, 0, accepted)
	require.Equal(t, 0, duplicate)
}

// TestIngestService_RejectsNilEnvelope guards against a caller bug where
// an envelope slot is nil. Surface a caller error rather than panicking
// inside the publish loop once a transaction is open.
func TestIngestService_RejectsNilEnvelope(t *testing.T) {
	svc := &IngestService{} // precondition check fires before DB access
	_, _, err := svc.IngestBatch(context.Background(), []*events.Envelope{nil})
	require.Error(t, err)
	require.Contains(t, err.Error(), "envelope at index 0 is nil")
}

// TestIngestService_RejectsPreAssignedID guards the fragile env.ID
// sentinel contract: callers MUST hand in uuid.Nil envelopes so the
// service can count duplicates correctly. A non-zero ID on entry would
// cause a duplicate to look accepted. Surface it as a caller bug at the
// service boundary.
func TestIngestService_RejectsPreAssignedID(t *testing.T) {
	svc := &IngestService{} // precondition check fires before DB access
	env := &events.Envelope{ID: uuid.New()}
	_, _, err := svc.IngestBatch(context.Background(), []*events.Envelope{env})
	require.Error(t, err)
	require.Contains(t, err.Error(), "pre-assigned ID")
}
