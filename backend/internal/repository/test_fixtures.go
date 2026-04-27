package repository

// TEST ONLY. Thin repository wrappers around test-only sqlc bindings used by
// jsonb_gin_index_test.go. These wrappers return the generated db.* types
// directly to keep the fixture surface minimal — no production code path
// depends on them.

import (
	"context"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// TestJSONBFixturesRepository wraps the test-only sqlc bindings for
// JSONB-array index integration tests. It is in the production repository
// package so tests can construct it from a *db.Database, but no production
// caller references it.
type TestJSONBFixturesRepository struct {
	queries db.Querier
}

// NewTestJSONBFixturesRepository constructs the test-only fixture repository.
func NewTestJSONBFixturesRepository(queries db.Querier) *TestJSONBFixturesRepository {
	return &TestJSONBFixturesRepository{queries: queries}
}

// InsertExternalContactRawEmails inserts an external_contact with a literal
// JSONB value (or nil for SQL NULL) for the emails column. The caller passes
// raw JSONB bytes; nothing is marshalled. Returns the generated db row.
func (r *TestJSONBFixturesRepository) InsertExternalContactRawEmails(ctx context.Context, source, sourceID, displayName string, emails []byte) (*db.ExternalContact, error) {
	return r.queries.TestInsertExternalContactRawEmails(ctx, db.TestInsertExternalContactRawEmailsParams{
		Source:      source,
		SourceID:    sourceID,
		DisplayName: displayName,
		Emails:      emails,
	})
}

// InsertCalendarEventRawAttendees inserts a calendar_event with a literal
// JSONB value (or nil for SQL NULL) for the attendees column.
func (r *TestJSONBFixturesRepository) InsertCalendarEventRawAttendees(
	ctx context.Context,
	gcalEventID, gcalCalendarID, googleAccountID string,
	startTime, endTime time.Time,
	status string,
	attendees []byte,
	matchedContactIDs []uuid.UUID,
) (*db.CalendarEvent, error) {
	pgIDs := make([]pgtype.UUID, len(matchedContactIDs))
	for i, id := range matchedContactIDs {
		pgIDs[i] = uuidToPgUUID(id)
	}
	return r.queries.TestInsertCalendarEventRawAttendees(ctx, db.TestInsertCalendarEventRawAttendeesParams{
		GcalEventID:       gcalEventID,
		GcalCalendarID:    gcalCalendarID,
		GoogleAccountID:   googleAccountID,
		StartTime:         pgtype.Timestamptz{Time: startTime, Valid: true},
		EndTime:           pgtype.Timestamptz{Time: endTime, Valid: true},
		Status:            status,
		Attendees:         attendees,
		MatchedContactIds: pgIDs,
	})
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

// FindExternalContactsByNormalizedEmailLegacy runs the legacy EXISTS /
// jsonb_array_elements form of the query for parity assertions. Callers
// must restrict input fixtures to well-formed JSONB arrays — the legacy
// SQL raises on scalar/object shapes.
func (r *TestJSONBFixturesRepository) FindExternalContactsByNormalizedEmailLegacy(ctx context.Context, email string) ([]*db.ExternalContact, error) {
	return r.queries.TestParityFindExternalContactsByNormalizedEmailLegacy(ctx, email)
}

// FindEventsByAttendeeEmailUnmatchedForContactLegacy runs the legacy form
// of the calendar_event query for parity assertions. Callers must restrict
// input fixtures to well-formed JSONB arrays.
func (r *TestJSONBFixturesRepository) FindEventsByAttendeeEmailUnmatchedForContactLegacy(ctx context.Context, email string, contactID uuid.UUID) ([]*db.CalendarEvent, error) {
	return r.queries.TestParityFindEventsByAttendeeEmailUnmatchedForContactLegacy(ctx, db.TestParityFindEventsByAttendeeEmailUnmatchedForContactLegacyParams{
		Email:     email,
		ContactID: uuidToPgUUID(contactID),
	})
}
