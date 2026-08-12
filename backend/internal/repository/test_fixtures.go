package repository

// TEST ONLY. Thin repository wrappers around test-only sqlc bindings used by
// jsonb_gin_index_test.go. Production code does not depend on these — they
// only construct edge-case JSONB fixtures (NULL, scalar, missing keys) that
// the typed-Go production paths cannot produce, plus run the legacy form
// of the rewritten queries for parity assertions.

import (
	"context"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
)

// TestJSONBFixturesRepository wraps the test-only sqlc bindings for
// JSONB-array index integration tests. It is in the production repository
// package so tests can construct it from a *db.Database, but no production
// caller references it. The returned types are clean repository/domain
// types (or just the row's UUID) — generated db.* types are not leaked
// across the package boundary.
type TestJSONBFixturesRepository struct {
	queries db.Querier
}

// NewTestJSONBFixturesRepository constructs the test-only fixture repository.
func NewTestJSONBFixturesRepository(queries db.Querier) *TestJSONBFixturesRepository {
	return &TestJSONBFixturesRepository{queries: queries}
}

// InsertExternalContactRawEmails inserts an external_contact with a literal
// JSONB value (or nil for SQL NULL) for the emails column. The caller passes
// raw JSONB bytes; nothing is marshalled. Returns the inserted row's UUID.
func (r *TestJSONBFixturesRepository) InsertExternalContactRawEmails(ctx context.Context, source, sourceID, displayName string, emails []byte) (uuid.UUID, error) {
	row, err := r.queries.TestInsertExternalContactRawEmails(ctx, db.TestInsertExternalContactRawEmailsParams{
		Source:      source,
		SourceID:    sourceID,
		DisplayName: displayName,
		Emails:      emails,
	})
	if err != nil {
		return uuid.Nil, err
	}
	return row.ID, nil
}

// InsertCalendarEventRawAttendees inserts a calendar_event with a literal
// JSONB value (or nil for SQL NULL) for the attendees column. Returns the
// inserted row's UUID.
func (r *TestJSONBFixturesRepository) InsertCalendarEventRawAttendees(
	ctx context.Context,
	gcalEventID, gcalCalendarID, googleAccountID string,
	startTime, endTime time.Time,
	status string,
	attendees []byte,
	matchedContactIDs []uuid.UUID,
) (uuid.UUID, error) {
	if matchedContactIDs == nil {
		matchedContactIDs = []uuid.UUID{}
	}
	row, err := r.queries.TestInsertCalendarEventRawAttendees(ctx, db.TestInsertCalendarEventRawAttendeesParams{
		GcalEventID:       gcalEventID,
		GcalCalendarID:    gcalCalendarID,
		GoogleAccountID:   googleAccountID,
		StartTime:         startTime,
		EndTime:           endTime,
		Status:            status,
		Attendees:         attendees,
		MatchedContactIds: matchedContactIDs,
	})
	if err != nil {
		return uuid.Nil, err
	}
	return row.ID, nil
}

// DeleteExternalContactsBySourceIDPrefix hard-deletes fixture rows by
// source_id prefix. Used in t.Cleanup.
func (r *TestJSONBFixturesRepository) DeleteExternalContactsBySourceIDPrefix(ctx context.Context, prefix string) error {
	return r.queries.TestDeleteExternalContactsBySourceIDPrefix(ctx, prefix)
}

// DeleteCalendarEventsByGcalEventIDPrefix hard-deletes fixture rows by
// gcal_event_id prefix. Used in t.Cleanup.
func (r *TestJSONBFixturesRepository) DeleteCalendarEventsByGcalEventIDPrefix(ctx context.Context, prefix string) error {
	return r.queries.TestDeleteCalendarEventsByGcalEventIDPrefix(ctx, prefix)
}

// LegacyFinding holds the per-row identifying columns returned by the
// legacy parity queries — just enough for the test to compare result
// sets without the test importing the generated db.* types. We return
// (id, source_id) for external_contact and (id, gcal_event_id) for
// calendar_event so the test can filter to fixtures it owns.
type LegacyExternalContactFinding struct {
	ID       uuid.UUID
	SourceID string
}

type LegacyCalendarEventFinding struct {
	ID          uuid.UUID
	GcalEventID string
}

// FindExternalContactsByNormalizedEmailLegacy runs the legacy EXISTS /
// jsonb_array_elements form of the query for parity assertions. Callers
// must restrict input fixtures to well-formed JSONB arrays — the legacy
// SQL raises on scalar/object shapes.
func (r *TestJSONBFixturesRepository) FindExternalContactsByNormalizedEmailLegacy(ctx context.Context, email string) ([]LegacyExternalContactFinding, error) {
	rows, err := r.queries.TestParityFindExternalContactsByNormalizedEmailLegacy(ctx, email)
	if err != nil {
		return nil, err
	}
	out := make([]LegacyExternalContactFinding, 0, len(rows))
	for _, row := range rows {
		out = append(out, LegacyExternalContactFinding{
			ID:       row.ID,
			SourceID: row.SourceID,
		})
	}
	return out, nil
}

// FindEventsByAttendeeEmailUnmatchedForContactLegacy runs the legacy form
// of the calendar_event query for parity assertions. Callers must restrict
// input fixtures to well-formed JSONB arrays.
func (r *TestJSONBFixturesRepository) FindEventsByAttendeeEmailUnmatchedForContactLegacy(ctx context.Context, email string, contactID uuid.UUID) ([]LegacyCalendarEventFinding, error) {
	rows, err := r.queries.TestParityFindEventsByAttendeeEmailUnmatchedForContactLegacy(ctx, db.TestParityFindEventsByAttendeeEmailUnmatchedForContactLegacyParams{
		Email:     email,
		ContactID: contactID,
	})
	if err != nil {
		return nil, err
	}
	out := make([]LegacyCalendarEventFinding, 0, len(rows))
	for _, row := range rows {
		out = append(out, LegacyCalendarEventFinding{
			ID:          row.ID,
			GcalEventID: row.GcalEventID,
		})
	}
	return out, nil
}

// IndexExists checks whether a named index is present in the database.
// Used by the integration test as a structural assertion that the GIN
// indexes from migration 045 actually exist (a behavior-only test would
// pass even if a future migration accidentally dropped the indexes).
func (r *TestJSONBFixturesRepository) IndexExists(ctx context.Context, indexName string) (bool, error) {
	return r.queries.TestIndexExists(ctx, indexName)
}
