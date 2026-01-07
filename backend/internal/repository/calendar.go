package repository

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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
	if dbEvent.ID.Valid {
		event.ID = uuid.UUID(dbEvent.ID.Bytes)
	}

	// Convert nullable strings
	if dbEvent.Title.Valid {
		event.Title = &dbEvent.Title.String
	}
	if dbEvent.Description.Valid {
		event.Description = &dbEvent.Description.String
	}
	if dbEvent.Location.Valid {
		event.Location = &dbEvent.Location.String
	}
	if dbEvent.Status.Valid {
		event.Status = dbEvent.Status.String
	}
	if dbEvent.UserResponse.Valid {
		event.UserResponse = &dbEvent.UserResponse.String
	}
	if dbEvent.OrganizerEmail.Valid {
		event.OrganizerEmail = &dbEvent.OrganizerEmail.String
	}

	// Convert timestamps
	if dbEvent.StartTime.Valid {
		event.StartTime = dbEvent.StartTime.Time
	}
	if dbEvent.EndTime.Valid {
		event.EndTime = dbEvent.EndTime.Time
	}
	if dbEvent.SyncedAt.Valid {
		event.SyncedAt = dbEvent.SyncedAt.Time
	}
	if dbEvent.CreatedAt.Valid {
		event.CreatedAt = dbEvent.CreatedAt.Time
	}
	if dbEvent.UpdatedAt.Valid {
		event.UpdatedAt = dbEvent.UpdatedAt.Time
	}

	// Convert booleans
	if dbEvent.AllDay.Valid {
		event.AllDay = dbEvent.AllDay.Bool
	}
	if dbEvent.LastContactedUpdated.Valid {
		event.LastContactedUpdated = dbEvent.LastContactedUpdated.Bool
	}

	// Convert html_link
	if dbEvent.HtmlLink.Valid {
		event.HtmlLink = &dbEvent.HtmlLink.String
	}

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

	// Convert matched contact IDs
	event.MatchedContactIDs = make([]uuid.UUID, 0, len(dbEvent.MatchedContactIds))
	for _, pgUUID := range dbEvent.MatchedContactIds {
		if pgUUID.Valid {
			event.MatchedContactIDs = append(event.MatchedContactIDs, uuid.UUID(pgUUID.Bytes))
		}
	}

	return event
}

// Upsert inserts or updates a calendar event
func (r *CalendarEventRepository) Upsert(ctx context.Context, req UpsertCalendarEventRequest) (*CalendarEvent, error) {
	// Convert attendees to JSON
	attendeesJSON, err := json.Marshal(req.Attendees)
	if err != nil {
		return nil, err
	}

	// Convert matched contact IDs
	matchedContactIDs := make([]pgtype.UUID, len(req.MatchedContactIDs))
	for i, id := range req.MatchedContactIDs {
		matchedContactIDs[i] = uuidToPgUUID(id)
	}

	dbEvent, err := r.queries.UpsertCalendarEvent(ctx, db.UpsertCalendarEventParams{
		GcalEventID:          req.GcalEventID,
		GcalCalendarID:       req.GcalCalendarID,
		GoogleAccountID:      req.GoogleAccountID,
		Title:                stringToPgText(req.Title),
		Description:          stringToPgText(req.Description),
		Location:             stringToPgText(req.Location),
		StartTime:            pgtype.Timestamptz{Time: req.StartTime, Valid: true},
		EndTime:              pgtype.Timestamptz{Time: req.EndTime, Valid: true},
		AllDay:               pgtype.Bool{Bool: req.AllDay, Valid: true},
		Status:               stringToPgText(&req.Status),
		UserResponse:         stringToPgText(req.UserResponse),
		OrganizerEmail:       stringToPgText(req.OrganizerEmail),
		Attendees:            attendeesJSON,
		MatchedContactIds:    matchedContactIDs,
		SyncedAt:             pgtype.Timestamptz{Time: req.SyncedAt, Valid: true},
		LastContactedUpdated: pgtype.Bool{Bool: req.LastContactedUpdated, Valid: true},
		HtmlLink:             stringToPgText(req.HtmlLink),
	})
	if err != nil {
		return nil, err
	}

	event := convertDbCalendarEvent(dbEvent)
	return &event, nil
}

// GetByID retrieves a calendar event by its UUID
func (r *CalendarEventRepository) GetByID(ctx context.Context, id uuid.UUID) (*CalendarEvent, error) {
	dbEvent, err := r.queries.GetCalendarEventByID(ctx, uuidToPgUUID(id))
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
		ContactID:   uuidToPgUUID(contactID),
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
		ContactID:  uuidToPgUUID(contactID),
		AfterTime:  pgtype.Timestamptz{Time: after, Valid: true},
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
		StartTime: pgtype.Timestamptz{Time: after, Valid: true},
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
		EndTime: pgtype.Timestamptz{Time: before, Valid: true},
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

// MarkLastContactedUpdated marks an event as having updated last_contacted for its contacts
func (r *CalendarEventRepository) MarkLastContactedUpdated(ctx context.Context, id uuid.UUID) error {
	return r.queries.MarkLastContactedUpdated(ctx, uuidToPgUUID(id))
}

// UpdateMatchedContacts updates the matched contact IDs for an event
func (r *CalendarEventRepository) UpdateMatchedContacts(ctx context.Context, id uuid.UUID, contactIDs []uuid.UUID) (*CalendarEvent, error) {
	matchedContactIDs := make([]pgtype.UUID, len(contactIDs))
	for i, cid := range contactIDs {
		matchedContactIDs[i] = uuidToPgUUID(cid)
	}

	dbEvent, err := r.queries.UpdateMatchedContacts(ctx, db.UpdateMatchedContactsParams{
		ID:                uuidToPgUUID(id),
		MatchedContactIds: matchedContactIDs,
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
	return r.queries.CountEventsForContact(ctx, uuidToPgUUID(contactID))
}

// DeleteEventsByAccount deletes all events for a Google account
func (r *CalendarEventRepository) DeleteEventsByAccount(ctx context.Context, googleAccountID string) error {
	return r.queries.DeleteEventsByAccount(ctx, googleAccountID)
}
