package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CalendarEventRepository handles calendar event persistence
type CalendarEventRepository struct {
	queries db.Querier
}

// NewCalendarEventRepository creates a new calendar event repository
func NewCalendarEventRepository(queries db.Querier) *CalendarEventRepository {
	return &CalendarEventRepository{queries: queries}
}

// Attendee represents a calendar event attendee
type Attendee struct {
	Email        string `json:"email"`
	DisplayName  string `json:"display_name,omitempty"`
	ResponseType string `json:"response_type,omitempty"`
	Self         bool   `json:"self,omitempty"`
	Organizer    bool   `json:"organizer,omitempty"`
}

// CalendarEvent represents a calendar event entity
type CalendarEvent struct {
	ID                   uuid.UUID   `json:"id"`
	GcalEventID          string      `json:"gcal_event_id"`
	GcalCalendarID       string      `json:"gcal_calendar_id"`
	GoogleAccountID      string      `json:"google_account_id"`
	Title                *string     `json:"title,omitempty"`
	Description          *string     `json:"description,omitempty"`
	Location             *string     `json:"location,omitempty"`
	StartTime            time.Time   `json:"start_time"`
	EndTime              time.Time   `json:"end_time"`
	AllDay               bool        `json:"all_day"`
	Status               string      `json:"status"`
	UserResponse         *string     `json:"user_response,omitempty"`
	OrganizerEmail       *string     `json:"organizer_email,omitempty"`
	Attendees            []Attendee  `json:"attendees"`
	MatchedContactIDs    []uuid.UUID `json:"matched_contact_ids"`
	SyncedAt             time.Time   `json:"synced_at"`
	LastContactedUpdated bool        `json:"last_contacted_updated"`
	HtmlLink             *string     `json:"html_link,omitempty"`
	CreatedAt            time.Time   `json:"created_at"`
	UpdatedAt            time.Time   `json:"updated_at"`
}

// UpsertCalendarEventRequest holds parameters for upserting a calendar event
type UpsertCalendarEventRequest struct {
	GcalEventID          string
	GcalCalendarID       string
	GoogleAccountID      string
	Title                *string
	Description          *string
	Location             *string
	StartTime            time.Time
	EndTime              time.Time
	AllDay               bool
	Status               string
	UserResponse         *string
	OrganizerEmail       *string
	Attendees            []Attendee
	MatchedContactIDs    []uuid.UUID
	SyncedAt             time.Time
	LastContactedUpdated bool
	HtmlLink             *string
}

// convertDbCalendarEvent converts a database calendar event to a repository calendar event
func convertDbCalendarEvent(dbEvent *db.CalendarEvent) CalendarEvent {
	event := CalendarEvent{
		GcalEventID:     dbEvent.GcalEventID,
		GcalCalendarID:  dbEvent.GcalCalendarID,
		GoogleAccountID: dbEvent.GoogleAccountID,
	}

	// Convert UUID
	event.ID = dbEvent.ID

	// Convert nullable strings
	event.Title = dbEvent.Title
	event.Description = dbEvent.Description
	event.Location = dbEvent.Location
	if dbEvent.Status != nil {
		event.Status = *dbEvent.Status
	}
	event.UserResponse = dbEvent.UserResponse
	event.OrganizerEmail = dbEvent.OrganizerEmail

	// Convert timestamps
	event.StartTime = dbEvent.StartTime
	event.EndTime = dbEvent.EndTime
	if dbEvent.SyncedAt != nil {
		event.SyncedAt = *dbEvent.SyncedAt
	}
	if dbEvent.CreatedAt != nil {
		event.CreatedAt = *dbEvent.CreatedAt
	}
	if dbEvent.UpdatedAt != nil {
		event.UpdatedAt = *dbEvent.UpdatedAt
	}

	// Convert booleans
	if dbEvent.AllDay != nil {
		event.AllDay = *dbEvent.AllDay
	}
	if dbEvent.LastContactedUpdated != nil {
		event.LastContactedUpdated = *dbEvent.LastContactedUpdated
	}

	// Convert html_link
	event.HtmlLink = dbEvent.HtmlLink

	// Convert attendees JSONB
	if len(dbEvent.Attendees) > 0 {
		var attendees []Attendee
		if err := json.Unmarshal(dbEvent.Attendees, &attendees); err == nil {
			event.Attendees = attendees
		}
	}
	if event.Attendees == nil {
		event.Attendees = []Attendee{}
	}

	// Convert matched contact IDs. A NULL array element decodes to
	// uuid.Nil rather than being rejected (repo-shrink plan §1.5/§5.6),
	// so filter those out here on the read side, preserving order.
	event.MatchedContactIDs = make([]uuid.UUID, 0, len(dbEvent.MatchedContactIds))
	for _, id := range dbEvent.MatchedContactIds {
		if id != uuid.Nil {
			event.MatchedContactIDs = append(event.MatchedContactIDs, id)
		}
	}

	return event
}

// filterNilUUIDs drops any uuid.Nil element from ids, preserving the order
// of the survivors, and always returns a non-nil slice so an empty result
// still encodes as '{}' rather than SQL NULL. Applied on the write side by
// every exported writer of matched_contact_ids (repo-shrink plan §5.6).
func filterNilUUIDs(ids []uuid.UUID) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if id != uuid.Nil {
			out = append(out, id)
		}
	}
	return out
}

// Upsert inserts or updates a calendar event
func (r *CalendarEventRepository) Upsert(ctx context.Context, req UpsertCalendarEventRequest) (*CalendarEvent, error) {
	// Convert attendees to JSON
	attendeesJSON, err := json.Marshal(req.Attendees)
	if err != nil {
		return nil, err
	}

	dbEvent, err := r.queries.UpsertCalendarEvent(ctx, db.UpsertCalendarEventParams{
		GcalEventID:          req.GcalEventID,
		GcalCalendarID:       req.GcalCalendarID,
		GoogleAccountID:      req.GoogleAccountID,
		Title:                req.Title,
		Description:          req.Description,
		Location:             req.Location,
		StartTime:            req.StartTime,
		EndTime:              req.EndTime,
		AllDay:               &req.AllDay,
		Status:               &req.Status,
		UserResponse:         req.UserResponse,
		OrganizerEmail:       req.OrganizerEmail,
		Attendees:            attendeesJSON,
		MatchedContactIds:    filterNilUUIDs(req.MatchedContactIDs),
		SyncedAt:             &req.SyncedAt,
		LastContactedUpdated: &req.LastContactedUpdated,
		HtmlLink:             req.HtmlLink,
	})
	if err != nil {
		return nil, err
	}

	event := convertDbCalendarEvent(dbEvent)
	return &event, nil
}

// GetByID retrieves a calendar event by its UUID
func (r *CalendarEventRepository) GetByID(ctx context.Context, id uuid.UUID) (*CalendarEvent, error) {
	dbEvent, err := r.queries.GetCalendarEventByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	event := convertDbCalendarEvent(dbEvent)
	return &event, nil
}

// TestHardDeleteByID is a test-only helper that hard-deletes a single
// calendar_event row by primary key. Production code must NOT call this.
func (r *CalendarEventRepository) TestHardDeleteByID(ctx context.Context, id uuid.UUID) error {
	return r.queries.TestHardDeleteCalendarEventByID(ctx, id)
}

// GetByIDTx is the tx-bound variant of GetByID. Used by callers that
// need the read to participate in the same tx as a subsequent write
// (e.g. the meeting_note resolve-link flow's target-existence check).
func (r *CalendarEventRepository) GetByIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*CalendarEvent, error) {
	dbEvent, err := db.New(tx).GetCalendarEventByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	event := convertDbCalendarEvent(dbEvent)
	return &event, nil
}

// GetByGcalID retrieves a calendar event by its Google Calendar ID
func (r *CalendarEventRepository) GetByGcalID(ctx context.Context, gcalEventID, gcalCalendarID, googleAccountID string) (*CalendarEvent, error) {
	dbEvent, err := r.queries.GetCalendarEventByGcalID(ctx, db.GetCalendarEventByGcalIDParams{
		GcalEventID:     gcalEventID,
		GcalCalendarID:  gcalCalendarID,
		GoogleAccountID: googleAccountID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	event := convertDbCalendarEvent(dbEvent)
	return &event, nil
}

// ListEventsForContact retrieves calendar events involving a specific contact
func (r *CalendarEventRepository) ListEventsForContact(ctx context.Context, contactID uuid.UUID, limit, offset int32) ([]CalendarEvent, error) {
	dbEvents, err := r.queries.ListEventsForContact(ctx, db.ListEventsForContactParams{
		ContactID:   contactID,
		EventLimit:  limit,
		EventOffset: offset,
	})
	if err != nil {
		return nil, err
	}

	events := make([]CalendarEvent, len(dbEvents))
	for i, dbEvent := range dbEvents {
		events[i] = convertDbCalendarEvent(dbEvent)
	}

	return events, nil
}

// ListUpcomingEventsForContact retrieves upcoming calendar events for a specific contact
func (r *CalendarEventRepository) ListUpcomingEventsForContact(ctx context.Context, contactID uuid.UUID, after time.Time, limit int32) ([]CalendarEvent, error) {
	dbEvents, err := r.queries.ListUpcomingEventsForContact(ctx, db.ListUpcomingEventsForContactParams{
		ContactID:  contactID,
		AfterTime:  after,
		EventLimit: limit,
	})
	if err != nil {
		return nil, err
	}

	events := make([]CalendarEvent, len(dbEvents))
	for i, dbEvent := range dbEvents {
		events[i] = convertDbCalendarEvent(dbEvent)
	}

	return events, nil
}

// ListUpcomingEventsWithContacts retrieves upcoming events that have matched CRM contacts
func (r *CalendarEventRepository) ListUpcomingEventsWithContacts(ctx context.Context, after time.Time, limit, offset int32) ([]CalendarEvent, error) {
	dbEvents, err := r.queries.ListUpcomingEventsWithContacts(ctx, db.ListUpcomingEventsWithContactsParams{
		StartTime: after,
		Limit:     limit,
		Offset:    offset,
	})
	if err != nil {
		return nil, err
	}

	events := make([]CalendarEvent, len(dbEvents))
	for i, dbEvent := range dbEvents {
		events[i] = convertDbCalendarEvent(dbEvent)
	}

	return events, nil
}

// ListPastEventsNeedingUpdate retrieves past events that haven't updated last_contacted yet
func (r *CalendarEventRepository) ListPastEventsNeedingUpdate(ctx context.Context, before time.Time, limit int32) ([]CalendarEvent, error) {
	dbEvents, err := r.queries.ListPastEventsNeedingUpdate(ctx, db.ListPastEventsNeedingUpdateParams{
		EndTime: before,
		Limit:   limit,
	})
	if err != nil {
		return nil, err
	}

	events := make([]CalendarEvent, len(dbEvents))
	for i, dbEvent := range dbEvents {
		events[i] = convertDbCalendarEvent(dbEvent)
	}

	return events, nil
}

// ListPastEventsNeedingUpdateByPrefixForTest is ListPastEventsNeedingUpdate
// scoped to one synthetic namespace's gcal_event_id prefix. TEST ONLY — the
// synthetic replay harness wraps the calendar provider with it so the provider's
// DB-wide past-event enumeration can never read, mark or publish for another
// namespace's events on the shared test database.
//
// The scoping is in SQL rather than applied to the production query's result
// because the LIMIT binds first: with a page's worth of older unprocessed foreign
// rows present, a Go-side filter returns an empty local set on every retry and the
// replay starves instead of settling. Exported for the same reason
// MacHostRepository.SeedHostForTest is — the caller is an external package.
func (r *CalendarEventRepository) ListPastEventsNeedingUpdateByPrefixForTest(
	ctx context.Context,
	before time.Time,
	prefix string,
	limit int32,
) ([]CalendarEvent, error) {
	dbEvents, err := r.queries.SyntheticListPastEventsNeedingUpdateByPrefix(ctx, db.SyntheticListPastEventsNeedingUpdateByPrefixParams{
		Before:            before,
		GcalEventIDPrefix: &prefix,
		RowLimit:          limit,
	})
	if err != nil {
		return nil, err
	}
	events := make([]CalendarEvent, len(dbEvents))
	for i, dbEvent := range dbEvents {
		events[i] = convertDbCalendarEvent(dbEvent)
	}
	return events, nil
}

// MarkLastContactedUpdated marks an event as having updated last_contacted for its contacts
func (r *CalendarEventRepository) MarkLastContactedUpdated(ctx context.Context, id uuid.UUID) error {
	return r.queries.MarkLastContactedUpdated(ctx, id)
}

// UpdateMatchedContacts updates the matched contact IDs for an event
func (r *CalendarEventRepository) UpdateMatchedContacts(ctx context.Context, id uuid.UUID, contactIDs []uuid.UUID) (*CalendarEvent, error) {
	dbEvent, err := r.queries.UpdateMatchedContacts(ctx, db.UpdateMatchedContactsParams{
		ID:                id,
		MatchedContactIds: filterNilUUIDs(contactIDs),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	event := convertDbCalendarEvent(dbEvent)
	return &event, nil
}

// CountEventsForContact counts events for a specific contact
func (r *CalendarEventRepository) CountEventsForContact(ctx context.Context, contactID uuid.UUID) (int64, error) {
	return r.queries.CountEventsForContact(ctx, contactID)
}

// FindEventsByAttendeeEmailUnmatchedForContact returns events whose attendees JSONB
// contains the given normalized email but where the given contact is not yet in
// matched_contact_ids. Used by the rematch service.
func (r *CalendarEventRepository) FindEventsByAttendeeEmailUnmatchedForContact(ctx context.Context, email string, contactID uuid.UUID) ([]CalendarEvent, error) {
	dbEvents, err := r.queries.FindEventsByAttendeeEmailUnmatchedForContact(ctx, db.FindEventsByAttendeeEmailUnmatchedForContactParams{
		Email:     email,
		ContactID: contactID,
	})
	if err != nil {
		return nil, err
	}

	events := make([]CalendarEvent, len(dbEvents))
	for i, dbEvent := range dbEvents {
		events[i] = convertDbCalendarEvent(dbEvent)
	}

	return events, nil
}

// AppendMatchedContact appends contactID to an event's matched_contact_ids array
// iff it isn't already present. Does NOT reset last_contacted_updated. A
// uuid.Nil contactID is a no-op (repo-shrink plan §5.6 — the array must
// never accumulate a nil element).
func (r *CalendarEventRepository) AppendMatchedContact(ctx context.Context, eventID, contactID uuid.UUID) error {
	if contactID == uuid.Nil {
		return nil
	}
	return r.queries.AppendMatchedContact(ctx, db.AppendMatchedContactParams{
		ContactID: contactID,
		EventID:   eventID,
	})
}

// DeleteEventsByAccount deletes all events for a Google account
func (r *CalendarEventRepository) DeleteEventsByAccount(ctx context.Context, googleAccountID string) error {
	return r.queries.DeleteEventsByAccount(ctx, googleAccountID)
}

// DeleteByGcalID hard-deletes the stored calendar_event row keyed by its
// Google identity triple. Non-tx variant for completeness / future callers;
// the decline remove branch uses DeleteByGcalIDTx. calendar_event has no
// deleted_at column — removal is a hard DELETE.
func (r *CalendarEventRepository) DeleteByGcalID(ctx context.Context, gcalEventID, gcalCalendarID, googleAccountID string) error {
	return r.queries.DeleteCalendarEventByGcalID(ctx, db.DeleteCalendarEventByGcalIDParams{
		GcalEventID:     gcalEventID,
		GcalCalendarID:  gcalCalendarID,
		GoogleAccountID: googleAccountID,
	})
}

// DeleteByGcalIDTx is the tx-bound variant of DeleteByGcalID. Used by the
// cutover decline remove branch so the delete commits atomically with the
// calendar.declined publishes (publish-before-delete in one tx).
func (r *CalendarEventRepository) DeleteByGcalIDTx(ctx context.Context, tx pgx.Tx, gcalEventID, gcalCalendarID, googleAccountID string) error {
	return db.New(tx).DeleteCalendarEventByGcalID(ctx, db.DeleteCalendarEventByGcalIDParams{
		GcalEventID:     gcalEventID,
		GcalCalendarID:  gcalCalendarID,
		GoogleAccountID: googleAccountID,
	})
}

// MarkCancelledByGcalID marks the stored event cancelled (keyed by its
// Google identity triple) instead of deleting it. Used by the off-mode
// deferral branch of the decline remove path when the event bus is
// unavailable: status='cancelled' excludes the row from re-firing
// calendar.attended and from contact-facing reads without losing the row
// (and any already-recorded interaction's only cleanup handle).
func (r *CalendarEventRepository) MarkCancelledByGcalID(ctx context.Context, gcalEventID, gcalCalendarID, googleAccountID string) error {
	return r.queries.MarkCalendarEventCancelledByGcalID(ctx, db.MarkCalendarEventCancelledByGcalIDParams{
		GcalEventID:     gcalEventID,
		GcalCalendarID:  gcalCalendarID,
		GoogleAccountID: googleAccountID,
	})
}

// GetByIDForShareTx is a locking read of a calendar_event by UUID inside
// the caller's tx (SELECT ... FOR SHARE). Used by the InteractionRecorder
// calendar.attended branch to serialize against a concurrent decline
// DELETE on the same row: the FOR SHARE lock is held until the attended tx
// commits, so an interleaving decline DELETE either blocks (attended
// inserts first) or has already committed (this returns db.ErrNotFound and
// the attended insert is skipped). Returns db.ErrNotFound when no row.
func (r *CalendarEventRepository) GetByIDForShareTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*CalendarEvent, error) {
	dbEvent, err := db.New(tx).GetCalendarEventByIDForShare(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	event := convertDbCalendarEvent(dbEvent)
	return &event, nil
}

// TestGetByIDForUpdateNoWaitTx is a TEST-ONLY probe: it reads a
// calendar_event with FOR UPDATE NOWAIT inside the caller's tx, returning a
// lock-conflict error immediately (without blocking) when another tx holds
// a conflicting lock on the row (e.g. the attended branch's FOR SHARE). Used
// by the attended-vs-decline lock-serialization integration test. Production
// code must NOT call this. Returns db.ErrNotFound when no row.
func (r *CalendarEventRepository) TestGetByIDForUpdateNoWaitTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*CalendarEvent, error) {
	dbEvent, err := db.New(tx).TestGetCalendarEventByIDForUpdateNoWait(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	event := convertDbCalendarEvent(dbEvent)
	return &event, nil
}

// LockExistsByIDTx reports whether a calendar_event with the given UUID
// exists, taking a FOR SHARE lock on it inside the caller's tx. Thin
// adapter over GetByIDForShareTx that satisfies the InteractionRecorder's
// narrow calendarEventLocker interface (it only needs existence, not the
// row). Returns (false, nil) for a missing row, (true, nil) when the row
// is present (lock held until the tx commits), and (false, err) on any
// other error.
func (r *CalendarEventRepository) LockExistsByIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (bool, error) {
	_, err := r.GetByIDForShareTx(ctx, tx, id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// FindLinkageCandidatesTx returns candidate calendar_event rows for the
// meeting_note.recorded inline handler's linkage-detection algorithm.
// Filters cancelled events and orders by start_time ASC. Calendar is
// the only candidate dimension today; when the phone_call table lands,
// callers will combine this with PhoneCallRepository.FindLinkageCandidatesTx
// and dedupe in the service layer. Caller owns the tx lifecycle.
func (r *CalendarEventRepository) FindLinkageCandidatesTx(ctx context.Context, tx pgx.Tx, windowStart, windowEnd time.Time) ([]LinkageCandidate, error) {
	rows, err := db.New(tx).FindCalendarEventsInWindow(ctx, db.FindCalendarEventsInWindowParams{
		WindowStart: windowStart,
		WindowEnd:   windowEnd,
	})
	if err != nil {
		return nil, err
	}
	out := make([]LinkageCandidate, 0, len(rows))
	for _, row := range rows {
		event := convertDbCalendarEvent(row)
		out = append(out, LinkageCandidate{
			Kind:               "event",
			ID:                 event.ID,
			OccurredAt:         event.StartTime,
			AttendeeContactIDs: event.MatchedContactIDs,
			NormalizedTitle:    NormalizeCoalesceTitle(event.Title),
		})
	}
	return out, nil
}
