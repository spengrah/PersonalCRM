package service

import (
	"context"
	"testing"

	"personal-crm/backend/internal/events"

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
