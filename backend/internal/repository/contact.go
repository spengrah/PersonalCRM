package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/logger"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ContactRepository struct {
	queries db.Querier
	// pool is optional; when non-nil it enables own-tx methods like
	// SnapshotContactCadenceFields that need to run a SELECT outside
	// the caller's tx. Nil in most test constructors — methods that
	// require it return an error when pool == nil.
	pool *pgxpool.Pool
}

// NewContactRepository constructs the repository with the sqlc queries
// querier. Callers that need own-tx methods must separately call SetPool.
func NewContactRepository(queries db.Querier) *ContactRepository {
	return &ContactRepository{queries: queries}
}

// SetPool injects the pgxpool used by own-tx methods. Optional; safe to
// leave unset for code paths that don't need SnapshotContactCadenceFields.
func (r *ContactRepository) SetPool(pool *pgxpool.Pool) {
	r.pool = pool
}

// ContactCadenceFields is the four-cadence-column snapshot carried on
// the interaction.recorded V2 payload (spec §3.4.2). It captures the
// pre-image of the cadence columns so CadenceUpdater can replay
// forward-only math against a deterministic prev state.
//
// Timestamps are UTC; ContactBy is day-precision (DATE column).
type ContactCadenceFields struct {
	LastContacted  *time.Time
	LastOutreachAt *time.Time
	LastResponseAt *time.Time
	ContactBy      *time.Time
}

// CadenceApplyFlagsByDirection returns the four per-column apply flags
// derived from the interaction direction: outbound → only
// last_outreach_at; inbound → last_contacted + last_response_at +
// contact_by (but NOT last_outreach_at — inbound interactions are
// responses to outreach we've already recorded, not new outreach);
// mutual → all four. Unknown / empty direction returns all-false.
//
// NOTE: applyContactBy here is the "direction permits contact_by" flag;
// the actual per-call decision combines this with ShouldApplyContactBy
// (which additionally gates on prev.LastContacted + occurredAt + manual
// vs automated + cadence-set).
func CadenceApplyFlagsByDirection(direction string) (applyLastContacted, applyLastOutreachAt, applyLastResponseAt, applyContactBy bool) {
	switch direction {
	case InteractionDirectionOutbound:
		return false, true, false, false
	case InteractionDirectionInbound:
		// Inbound does NOT bump last_outreach_at: it's the response, not
		// the outreach.
		return true, false, true, true
	case InteractionDirectionMutual:
		return true, true, true, true
	default:
		return false, false, false, false
	}
}

// ShouldApplyContactBy gates whether an interaction event should
// recompute contact_by: apply if the contact has cadence AND (source is
// manual OR prev.LastContacted is nil OR occurredAt is strictly after
// prev.LastContacted). Used by the CadenceUpdater consumer when
// replaying against the payload's prev snapshot. Keeping the logic in
// the repository package avoids duplicating it between service and
// consumer and eliminates drift risk.
func ShouldApplyContactBy(prevLastContacted *time.Time, occurredAt time.Time, isManual bool, hasCadence bool) bool {
	if !hasCadence {
		return false
	}
	if isManual {
		return true
	}
	if prevLastContacted == nil {
		return true
	}
	return occurredAt.After(*prevLastContacted)
}

// ForwardMax returns the strictly-forward max of prev and incoming.
// Mirrors the "last_X IS NULL OR incoming > last_X" semantics used by
// the forward-only cadence UPDATE. Strict `>` — equal timestamps do
// NOT advance.
func ForwardMax(prev *time.Time, incoming time.Time) time.Time {
	if prev == nil {
		return incoming
	}
	if incoming.After(*prev) {
		return incoming
	}
	return *prev
}

// ContactCadenceFieldsFromContact extracts the four cadence columns from
// an in-memory Contact row. Used by any path that needs to snapshot the
// pre-cadence state without a DB round-trip (e.g. the InteractionRecorder
// path that populates the V2 interaction.recorded payload). Returns a
// value (not pointer) so callers can embed it in closures by value.
func ContactCadenceFieldsFromContact(c *Contact) ContactCadenceFields {
	if c == nil {
		return ContactCadenceFields{}
	}
	return ContactCadenceFields{
		LastContacted:  c.LastContacted,
		LastOutreachAt: c.LastOutreachAt,
		LastResponseAt: c.LastResponseAt,
		ContactBy:      c.ContactBy,
	}
}

// Contact represents a contact entity
type Contact struct {
	ID                uuid.UUID       `json:"id"`
	FullName          string          `json:"full_name"`
	Methods           []ContactMethod `json:"methods,omitempty"`
	PrimaryMethod     *ContactMethod  `json:"primary_method,omitempty"`
	Location          *string         `json:"location,omitempty"`
	Birthday          *time.Time      `json:"birthday,omitempty"`
	HowMet            *string         `json:"how_met,omitempty"`
	Cadence           *string         `json:"cadence,omitempty"`
	LastContacted     *time.Time      `json:"last_contacted,omitempty"`
	ContactBy         *time.Time      `json:"contact_by,omitempty"`
	LastInteractionAt *time.Time      `json:"last_interaction_at,omitempty"`
	LastOutreachAt    *time.Time      `json:"last_outreach_at,omitempty"`
	LastResponseAt    *time.Time      `json:"last_response_at,omitempty"`
	ProfilePhoto      *string         `json:"profile_photo,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

// CreateContactRequest represents the request to create a contact
type CreateContactRequest struct {
	FullName      string     `json:"full_name"`
	Location      *string    `json:"location,omitempty"`
	Birthday      *time.Time `json:"birthday,omitempty"`
	HowMet        *string    `json:"how_met,omitempty"`
	Cadence       *string    `json:"cadence,omitempty"`
	LastContacted *time.Time `json:"last_contacted,omitempty"`
	ProfilePhoto  *string    `json:"profile_photo,omitempty"`
}

// UpdateContactRequest represents the request to update a contact
type UpdateContactRequest struct {
	FullName     string     `json:"full_name"`
	Location     *string    `json:"location,omitempty"`
	Birthday     *time.Time `json:"birthday,omitempty"`
	HowMet       *string    `json:"how_met,omitempty"`
	Cadence      *string    `json:"cadence,omitempty"`
	ContactBy    *time.Time `json:"contact_by,omitempty"`
	ProfilePhoto *string    `json:"profile_photo,omitempty"`
}

// ListContactsParams represents parameters for listing contacts
type ListContactsParams struct {
	Limit          int32  `json:"limit"`
	Offset         int32  `json:"offset"`
	Sort           string `json:"sort,omitempty"`
	Order          string `json:"order,omitempty"`
	CadenceFilter  string `json:"cadence_filter,omitempty"`
	FollowupFilter string `json:"followup_filter,omitempty"`
}

// SearchContactsParams represents parameters for searching contacts
type SearchContactsParams struct {
	Query          string `json:"query"`
	Limit          int32  `json:"limit"`
	Offset         int32  `json:"offset"`
	Sort           string `json:"sort,omitempty"`
	Order          string `json:"order,omitempty"`
	CadenceFilter  string `json:"cadence_filter,omitempty"`
	FollowupFilter string `json:"followup_filter,omitempty"`
}

// convertDbContact converts a database contact to a repository contact
func convertDbContact(dbContact *db.Contact) Contact {
	contact := Contact{
		FullName: dbContact.FullName,
	}

	// Convert UUID
	if dbContact.ID.Valid {
		contact.ID = uuid.UUID(dbContact.ID.Bytes)
	}

	// Convert timestamps
	if dbContact.CreatedAt.Valid {
		contact.CreatedAt = dbContact.CreatedAt.Time
	}
	if dbContact.UpdatedAt.Valid {
		contact.UpdatedAt = dbContact.UpdatedAt.Time
	}

	// Convert nullable fields
	if dbContact.Location.Valid {
		contact.Location = &dbContact.Location.String
	}
	if dbContact.Birthday.Valid {
		birthday := dbContact.Birthday.Time
		contact.Birthday = &birthday
	}
	if dbContact.HowMet.Valid {
		contact.HowMet = &dbContact.HowMet.String
	}
	if dbContact.Cadence.Valid {
		contact.Cadence = &dbContact.Cadence.String
	}
	if dbContact.LastContacted.Valid {
		lastContacted := dbContact.LastContacted.Time.UTC()
		contact.LastContacted = &lastContacted
	}
	if dbContact.ProfilePhoto.Valid {
		contact.ProfilePhoto = &dbContact.ProfilePhoto.String
	}
	if dbContact.ContactBy.Valid {
		contactBy := dbContact.ContactBy.Time
		contact.ContactBy = &contactBy
	}
	if dbContact.LastInteractionAt.Valid {
		t := dbContact.LastInteractionAt.Time.UTC()
		contact.LastInteractionAt = &t
	}
	if dbContact.LastOutreachAt.Valid {
		t := dbContact.LastOutreachAt.Time.UTC()
		contact.LastOutreachAt = &t
	}
	if dbContact.LastResponseAt.Valid {
		t := dbContact.LastResponseAt.Time.UTC()
		contact.LastResponseAt = &t
	}

	return contact
}

// GetContact retrieves a contact by ID
func (r *ContactRepository) GetContact(ctx context.Context, id uuid.UUID) (*Contact, error) {
	dbContact, err := r.queries.GetContact(ctx, uuidToPgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	contact := convertDbContact(dbContact)
	return &contact, nil
}

// GetContactTx is the tx-threaded variant of GetContact. Used by the
// InteractionRecorder consumer to verify the contact exists inside the
// caller's tx before the interaction insert runs. Returns db.ErrNotFound
// for a missing row so the consumer can propagate it as a clean 404-style
// error rather than an opaque FK violation.
func (r *ContactRepository) GetContactTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*Contact, error) {
	dbContact, err := db.New(tx).GetContact(ctx, uuidToPgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	contact := convertDbContact(dbContact)
	return &contact, nil
}

// ListContacts retrieves a paginated list of contacts
func (r *ContactRepository) ListContacts(ctx context.Context, params ListContactsParams) ([]Contact, error) {
	var (
		dbContacts []*db.Contact
		err        error
	)

	if params.Sort != "" {
		dbContacts, err = r.queries.ListContactsSorted(ctx, db.ListContactsSortedParams{
			CadenceFilter:  params.CadenceFilter,
			FollowupFilter: params.FollowupFilter,
			SortField:      params.Sort,
			SortOrder:      params.Order,
			PageOffset:     params.Offset,
			PageLimit:      params.Limit,
		})
	} else {
		dbContacts, err = r.queries.ListContacts(ctx, db.ListContactsParams{
			CadenceFilter:  params.CadenceFilter,
			FollowupFilter: params.FollowupFilter,
			PageOffset:     params.Offset,
			PageLimit:      params.Limit,
		})
	}
	if err != nil {
		return nil, err
	}

	contacts := make([]Contact, len(dbContacts))
	for i, dbContact := range dbContacts {
		contacts[i] = convertDbContact(dbContact)
	}

	return contacts, nil
}

// SearchContacts searches for contacts by query
func (r *ContactRepository) SearchContacts(ctx context.Context, params SearchContactsParams) ([]Contact, error) {
	var (
		dbContacts []*db.Contact
		err        error
	)

	if params.Sort != "" {
		dbContacts, err = r.queries.SearchContactsSorted(ctx, db.SearchContactsSortedParams{
			CadenceFilter:  params.CadenceFilter,
			FollowupFilter: params.FollowupFilter,
			SearchQuery:    params.Query,
			SortField:      params.Sort,
			SortOrder:      params.Order,
			PageOffset:     params.Offset,
			PageLimit:      params.Limit,
		})
	} else {
		dbContacts, err = r.queries.SearchContacts(ctx, db.SearchContactsParams{
			CadenceFilter:  params.CadenceFilter,
			FollowupFilter: params.FollowupFilter,
			SearchQuery:    params.Query,
			PageOffset:     params.Offset,
			PageLimit:      params.Limit,
		})
	}
	if err != nil {
		return nil, err
	}

	contacts := make([]Contact, len(dbContacts))
	for i, dbContact := range dbContacts {
		contacts[i] = convertDbContact(dbContact)
	}

	return contacts, nil
}

// CreateContact creates a new contact
func (r *ContactRepository) CreateContact(ctx context.Context, req CreateContactRequest) (*Contact, error) {
	// Use accelerated time for created_at to ensure consistency with time acceleration
	createdAt := accelerated.GetCurrentTime()

	// Calculate contact_by if cadence is set
	var contactBy pgtype.Date
	if req.Cadence != nil && *req.Cadence != "" {
		if cadenceType, err := cadence.ParseCadence(*req.Cadence); err == nil {
			// Use created_at as base since last_contacted is typically nil for new contacts
			base := createdAt
			if req.LastContacted != nil {
				base = *req.LastContacted
			}
			contactByTime := cadence.CalculateContactBy(base, cadenceType)
			contactBy = pgtype.Date{Time: contactByTime, Valid: true}
		}
	}

	dbContact, err := r.queries.CreateContact(ctx, db.CreateContactParams{
		FullName:      req.FullName,
		Location:      stringToPgText(req.Location),
		Birthday:      timeToPgDate(req.Birthday),
		HowMet:        stringToPgText(req.HowMet),
		Cadence:       stringToPgText(req.Cadence),
		LastContacted: timeToPgTimestamptz(req.LastContacted),
		ProfilePhoto:  stringToPgText(req.ProfilePhoto),
		CreatedAt:     pgtype.Timestamptz{Time: createdAt, Valid: true},
		ContactBy:     contactBy,
	})
	if err != nil {
		return nil, err
	}

	contact := convertDbContact(dbContact)
	return &contact, nil
}

// UpdateContact updates an existing contact's profile fields (name,
// location, birthday, how_met, cadence, profile_photo). Post-cutover
// this path NEVER writes last_contacted, last_outreach_at,
// last_response_at, or contact_by. Cadence-change side effects on
// contact_by are the caller's responsibility (ContactService routes
// them through CadenceUpdater.ApplyContactByOverride).
//
// The req.ContactBy field is preserved on the DTO for call-site
// convenience (service layer still wants to pass it through so it can
// be handed to CadenceUpdater) but is intentionally NOT threaded into
// the SQL parameters below.
func (r *ContactRepository) UpdateContact(ctx context.Context, id uuid.UUID, req UpdateContactRequest) (*Contact, error) {
	dbContact, err := r.queries.UpdateContact(ctx, db.UpdateContactParams{
		ID:           uuidToPgUUID(id),
		FullName:     req.FullName,
		Location:     stringToPgText(req.Location),
		Birthday:     timeToPgDate(req.Birthday),
		HowMet:       stringToPgText(req.HowMet),
		Cadence:      stringToPgText(req.Cadence),
		ProfilePhoto: stringToPgText(req.ProfilePhoto),
	})
	if err != nil {
		return nil, err
	}

	contact := convertDbContact(dbContact)
	return &contact, nil
}

// UpdateContactLastContacted updates the last contacted date and contact_by for a contact.
// contactBy should be the newly calculated next due date based on lastContacted + cadence.
func (r *ContactRepository) UpdateContactLastContacted(ctx context.Context, id uuid.UUID, lastContacted time.Time, contactBy *time.Time) error {
	return r.queries.UpdateContactLastContacted(ctx, db.UpdateContactLastContactedParams{
		ID:            uuidToPgUUID(id),
		LastContacted: pgtype.Timestamptz{Time: lastContacted, Valid: true},
		ContactBy:     timeToPgDate(contactBy),
	})
}

// UpdateContactLastContactedIfLater updates last_contacted to the later of the current value or the provided value.
// This prevents last_contacted from moving backward when events are processed out of order.
// The contact_by date is automatically recalculated in SQL using the contact's existing cadence.
func (r *ContactRepository) UpdateContactLastContactedIfLater(ctx context.Context, id uuid.UUID, lastContacted time.Time) error {
	return r.queries.UpdateContactLastContactedIfLater(ctx, db.UpdateContactLastContactedIfLaterParams{
		ID:            uuidToPgUUID(id),
		LastContacted: pgtype.Timestamptz{Time: lastContacted, Valid: true},
	})
}

// UpdateContactBy updates just the contact_by field (used by Todoist sync for deadline changes)
func (r *ContactRepository) UpdateContactBy(ctx context.Context, id uuid.UUID, contactBy time.Time) error {
	return r.queries.UpdateContactBy(ctx, db.UpdateContactByParams{
		ID:        uuidToPgUUID(id),
		ContactBy: timeToPgDate(&contactBy),
	})
}

// UpdateContactByTx is the tx-threaded variant of UpdateContactBy.
func (r *ContactRepository) UpdateContactByTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, contactBy time.Time) error {
	q := r.queries
	if tx != nil {
		q = db.New(tx)
	}
	return q.UpdateContactBy(ctx, db.UpdateContactByParams{
		ID:        uuidToPgUUID(id),
		ContactBy: timeToPgDate(&contactBy),
	})
}

// UpdateContactOutreachAt updates only last_outreach_at (for outbound interactions)
func (r *ContactRepository) UpdateContactOutreachAt(ctx context.Context, id uuid.UUID, outreachAt time.Time, isManual bool) error {
	return r.queries.UpdateContactOutreachAt(ctx, db.UpdateContactOutreachAtParams{
		ID:         uuidToPgUUID(id),
		OutreachAt: pgtype.Timestamptz{Time: outreachAt, Valid: true},
		IsManual:   isManual,
	})
}

// UpdateContactOutreachAtTx is the tx-threaded variant of UpdateContactOutreachAt.
// Used by InteractionRecorder / ContactService.RecordInteractionTx so the
// cadence UPDATE shares the caller's tx — spec §3.4.1 atomicity contract.
func (r *ContactRepository) UpdateContactOutreachAtTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, outreachAt time.Time, isManual bool) error {
	return db.New(tx).UpdateContactOutreachAt(ctx, db.UpdateContactOutreachAtParams{
		ID:         uuidToPgUUID(id),
		OutreachAt: pgtype.Timestamptz{Time: outreachAt, Valid: true},
		IsManual:   isManual,
	})
}

// UpdateContactResponseFields updates last_contacted, last_interaction_at, last_response_at, contact_by (for inbound interactions)
func (r *ContactRepository) UpdateContactResponseFields(ctx context.Context, id uuid.UUID, occurredAt time.Time, contactBy *time.Time, isManual bool) error {
	return r.queries.UpdateContactResponseFields(ctx, db.UpdateContactResponseFieldsParams{
		ID:         uuidToPgUUID(id),
		OccurredAt: pgtype.Timestamptz{Time: occurredAt, Valid: true},
		ContactBy:  timeToPgDate(contactBy),
		IsManual:   isManual,
	})
}

// UpdateContactResponseFieldsTx is the tx-threaded variant of UpdateContactResponseFields.
func (r *ContactRepository) UpdateContactResponseFieldsTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, occurredAt time.Time, contactBy *time.Time, isManual bool) error {
	return db.New(tx).UpdateContactResponseFields(ctx, db.UpdateContactResponseFieldsParams{
		ID:         uuidToPgUUID(id),
		OccurredAt: pgtype.Timestamptz{Time: occurredAt, Valid: true},
		ContactBy:  timeToPgDate(contactBy),
		IsManual:   isManual,
	})
}

// UpdateContactMutualFields updates all direction fields + last_contacted + contact_by (for mutual interactions)
func (r *ContactRepository) UpdateContactMutualFields(ctx context.Context, id uuid.UUID, occurredAt time.Time, contactBy *time.Time, isManual bool) error {
	return r.queries.UpdateContactMutualFields(ctx, db.UpdateContactMutualFieldsParams{
		ID:         uuidToPgUUID(id),
		OccurredAt: pgtype.Timestamptz{Time: occurredAt, Valid: true},
		ContactBy:  timeToPgDate(contactBy),
		IsManual:   isManual,
	})
}

// UpdateContactMutualFieldsTx is the tx-threaded variant of UpdateContactMutualFields.
func (r *ContactRepository) UpdateContactMutualFieldsTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, occurredAt time.Time, contactBy *time.Time, isManual bool) error {
	return db.New(tx).UpdateContactMutualFields(ctx, db.UpdateContactMutualFieldsParams{
		ID:         uuidToPgUUID(id),
		OccurredAt: pgtype.Timestamptz{Time: occurredAt, Valid: true},
		ContactBy:  timeToPgDate(contactBy),
		IsManual:   isManual,
	})
}

// SnapshotContactCadenceFields returns the four cadence columns
// (last_contacted, last_outreach_at, last_response_at, contact_by) for
// the given contact. Opens a short-lived own-tx on the configured pool
// — callers MUST NOT be inside an existing tx that they'd starve by
// taking another pool connection. Intended for any path that needs a
// post-image read outside a caller's tx. Returns db.ErrNotFound when
// the contact is soft-deleted.
//
// Requires SetPool to have been called. Returns an error otherwise.
func (r *ContactRepository) SnapshotContactCadenceFields(
	ctx context.Context, id uuid.UUID,
) (*ContactCadenceFields, error) {
	if r.pool == nil {
		return nil, errors.New("contact repo: SnapshotContactCadenceFields requires SetPool (pool not configured)")
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin snapshot tx: %w", err)
	}
	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && !errors.Is(rbErr, pgx.ErrTxClosed) {
			// Best-effort rollback on a read-only tx — swallow.
			_ = rbErr
		}
	}()

	row, err := db.New(tx).SnapshotContactCadenceFields(ctx, uuidToPgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, fmt.Errorf("snapshot cadence fields: %w", err)
	}
	return &ContactCadenceFields{
		LastContacted:  pgTimestamptzToTimePtr(row.LastContacted),
		LastOutreachAt: pgTimestamptzToTimePtr(row.LastOutreachAt),
		LastResponseAt: pgTimestamptzToTimePtr(row.LastResponseAt),
		ContactBy:      pgDateToTimePtr(row.ContactBy),
	}, nil
}

// SoftDeleteContact soft deletes a contact
func (r *ContactRepository) SoftDeleteContact(ctx context.Context, id uuid.UUID) error {
	return r.queries.SoftDeleteContact(ctx, uuidToPgUUID(id))
}

// HardDeleteContact permanently deletes a contact
func (r *ContactRepository) HardDeleteContact(ctx context.Context, id uuid.UUID) error {
	return r.queries.HardDeleteContact(ctx, uuidToPgUUID(id))
}

// CountContacts returns the total number of active contacts
func (r *ContactRepository) CountContacts(ctx context.Context, cadenceFilter string, followupFilter string) (int64, error) {
	return r.queries.CountContacts(ctx, db.CountContactsParams{
		CadenceFilter:  cadenceFilter,
		FollowupFilter: followupFilter,
	})
}

// CountSearchContacts returns the total number of contacts matching a search query.
func (r *ContactRepository) CountSearchContacts(ctx context.Context, query string, cadenceFilter string, followupFilter string) (int64, error) {
	return r.queries.CountSearchContacts(ctx, db.CountSearchContactsParams{
		CadenceFilter:  cadenceFilter,
		FollowupFilter: followupFilter,
		SearchQuery:    query,
	})
}

// ListContactIDsParams represents parameters for listing contact IDs
type ListContactIDsParams struct {
	Sort           string `json:"sort,omitempty"`
	Order          string `json:"order,omitempty"`
	Search         string `json:"search,omitempty"`
	CadenceFilter  string `json:"cadence_filter,omitempty"`
	FollowupFilter string `json:"followup_filter,omitempty"`
}

// ListContactIDs retrieves a list of contact IDs with optional sorting and search.
// This is a lightweight method for navigation purposes.
func (r *ContactRepository) ListContactIDs(ctx context.Context, params ListContactIDsParams) ([]uuid.UUID, error) {
	var (
		dbIDs []pgtype.UUID
		err   error
	)

	hasSearch := params.Search != ""
	hasSort := params.Sort != ""

	switch {
	case hasSearch && hasSort:
		dbIDs, err = r.queries.SearchContactIDsSorted(ctx, db.SearchContactIDsSortedParams{
			CadenceFilter:  params.CadenceFilter,
			FollowupFilter: params.FollowupFilter,
			SearchQuery:    params.Search,
			SortField:      params.Sort,
			SortOrder:      params.Order,
		})
	case hasSearch:
		dbIDs, err = r.queries.SearchContactIDs(ctx, db.SearchContactIDsParams{
			CadenceFilter:  params.CadenceFilter,
			FollowupFilter: params.FollowupFilter,
			SearchQuery:    params.Search,
		})
	case hasSort:
		dbIDs, err = r.queries.ListContactIDsSorted(ctx, db.ListContactIDsSortedParams{
			CadenceFilter:  params.CadenceFilter,
			FollowupFilter: params.FollowupFilter,
			SortField:      params.Sort,
			SortOrder:      params.Order,
		})
	default:
		dbIDs, err = r.queries.ListContactIDs(ctx, db.ListContactIDsParams{
			CadenceFilter:  params.CadenceFilter,
			FollowupFilter: params.FollowupFilter,
		})
	}

	if err != nil {
		return nil, err
	}

	ids := make([]uuid.UUID, len(dbIDs))
	for i, dbID := range dbIDs {
		if dbID.Valid {
			ids[i] = uuid.UUID(dbID.Bytes)
		}
	}

	return ids, nil
}

// ContactMatch represents a potential contact match with similarity score
type ContactMatch struct {
	Contact    Contact
	Similarity float64
}

// FindSimilarContacts finds contacts with similar names using fuzzy matching
// Returns contacts with similarity above the threshold, ordered by similarity (highest first)
func (r *ContactRepository) FindSimilarContacts(ctx context.Context, name string, threshold float64, limit int32) ([]ContactMatch, error) {
	rows, err := r.queries.FindSimilarContacts(ctx, db.FindSimilarContactsParams{
		SearchName:  name,
		Threshold:   float32(threshold),
		ResultLimit: limit,
	})
	if err != nil {
		return nil, err
	}

	matches := make([]ContactMatch, 0, len(rows))
	for _, row := range rows {
		// Convert UUID
		var contactID uuid.UUID
		if row.ID.Valid {
			contactID = uuid.UUID(row.ID.Bytes)
		}

		// Parse contact methods from JSON
		var methods []ContactMethod
		if len(row.MethodsJson) > 0 {
			// Unmarshal JSON into temporary struct
			var methodData []struct {
				Type  string `json:"type"`
				Value string `json:"value"`
			}
			if err := json.Unmarshal(row.MethodsJson, &methodData); err != nil {
				logger.Warn().Err(err).Str("contact_id", contactID.String()).Msg("failed to unmarshal contact methods JSON")
			} else {
				methods = make([]ContactMethod, len(methodData))
				for i, m := range methodData {
					methods[i] = ContactMethod{
						Type:  m.Type,
						Value: m.Value,
					}
				}
			}
		}

		matches = append(matches, ContactMatch{
			Contact: Contact{
				ID:       contactID,
				FullName: row.FullName,
				Methods:  methods,
			},
			Similarity: float64(row.NameSimilarity),
		})
	}

	return matches, nil
}

// BatchContactInput represents input for batch matching
type BatchContactInput struct {
	CandidateID   string // External contact ID for result grouping
	CandidateName string // Name to match against
}

// BatchContactMatch represents matches for a single candidate
type BatchContactMatch struct {
	CandidateID string         // External contact ID
	Matches     []ContactMatch // Matching contacts ordered by similarity
}

// FindSimilarContactsBatch finds similar contacts for multiple names in one query.
// Returns matches grouped by candidate ID, preserving order of inputs.
// Candidates with empty names should be filtered out before calling this method.
func (r *ContactRepository) FindSimilarContactsBatch(
	ctx context.Context,
	inputs []BatchContactInput,
	threshold float64,
	limitPerCandidate int32,
) ([]BatchContactMatch, error) {
	if len(inputs) == 0 {
		return []BatchContactMatch{}, nil
	}

	// Build input arrays
	names := make([]string, len(inputs))
	ids := make([]string, len(inputs))
	for i, input := range inputs {
		names[i] = input.CandidateName
		ids[i] = input.CandidateID
	}

	rows, err := r.queries.FindSimilarContactsBatch(ctx, db.FindSimilarContactsBatchParams{
		CandidateNames:    names,
		CandidateIds:      ids,
		Threshold:         float32(threshold),
		LimitPerCandidate: limitPerCandidate,
	})
	if err != nil {
		return nil, err
	}

	// Group results by candidate ID
	resultMap := make(map[string][]ContactMatch)
	for _, row := range rows {
		// Convert UUID
		var contactID uuid.UUID
		if row.ContactID.Valid {
			contactID = uuid.UUID(row.ContactID.Bytes)
		}

		// Parse contact methods from JSON
		var methods []ContactMethod
		if len(row.MethodsJson) > 0 {
			var methodData []struct {
				Type  string `json:"type"`
				Value string `json:"value"`
			}
			if err := json.Unmarshal(row.MethodsJson, &methodData); err != nil {
				logger.Warn().Err(err).Str("contact_id", contactID.String()).Str("candidate_id", row.CandidateID).Msg("failed to unmarshal contact methods JSON in batch")
			} else {
				methods = make([]ContactMethod, len(methodData))
				for i, m := range methodData {
					methods[i] = ContactMethod{
						Type:  m.Type,
						Value: m.Value,
					}
				}
			}
		}

		match := ContactMatch{
			Contact: Contact{
				ID:       contactID,
				FullName: row.ContactName,
				Methods:  methods,
			},
			Similarity: float64(row.NameSimilarity),
		}

		resultMap[row.CandidateID] = append(resultMap[row.CandidateID], match)
	}

	// Preserve input order and include empty results for candidates with no matches
	results := make([]BatchContactMatch, len(inputs))
	for i, input := range inputs {
		matches := resultMap[input.CandidateID]
		if matches == nil {
			matches = []ContactMatch{}
		}
		results[i] = BatchContactMatch{
			CandidateID: input.CandidateID,
			Matches:     matches,
		}
	}

	return results, nil
}

// ListOverdueContacts retrieves contacts whose contact_by date is before today.
// The today parameter should be the current date in server timezone (use cadence.Today()).
func (r *ContactRepository) ListOverdueContacts(ctx context.Context, today time.Time, limit int32) ([]Contact, error) {
	dbContacts, err := r.queries.ListOverdueContacts(ctx, db.ListOverdueContactsParams{
		Today:      pgtype.Date{Time: today, Valid: true},
		LimitCount: limit,
	})
	if err != nil {
		return nil, err
	}

	contacts := make([]Contact, len(dbContacts))
	for i, dbContact := range dbContacts {
		contacts[i] = convertDbContact(dbContact)
	}

	return contacts, nil
}

// ListContactsWithContactBy retrieves contacts that have a contact_by date set.
// Used primarily in testing mode to do in-memory timestamp filtering.
func (r *ContactRepository) ListContactsWithContactBy(ctx context.Context, limit int32) ([]Contact, error) {
	dbContacts, err := r.queries.ListContactsWithContactBy(ctx, limit)
	if err != nil {
		return nil, err
	}

	contacts := make([]Contact, len(dbContacts))
	for i, dbContact := range dbContacts {
		contacts[i] = convertDbContact(dbContact)
	}

	return contacts, nil
}

// RecomputeContactDatesAfterDeleteTx surgically rolls back the contact's
// date columns after a derived interaction at `deletedAt` was soft-deleted.
// It is the REMOVAL counterpart to CadenceUpdater's additive forward path:
// a timestamp column is touched ONLY when the removed interaction was its
// source (column == deletedAt), rolling to MAX(remaining live interactions
// of its direction subset) or NULL when none remain — preserving creation-
// set values and still-live later interactions. contact_by is decided in Go
// via cadence.CalculateContactBy (environment-aware) so it matches the
// forward writer exactly and never clobbers a Todoist/user override.
//
// Orchestration (all on the caller's tx): ComputeContactDatesAfterDelete
// (reads + FOR UPDATE locks the contact row, recomputes the timestamps) →
// Go contact_by decision → WriteContactDatesAfterDelete. The FOR UPDATE lock
// is held across the write so a concurrent cadence/interaction writer on the
// same contact cannot interleave a stale overwrite.
//
// Returns db.ErrNotFound when the contact was soft-deleted between the
// decline publish and consume (the read filters deleted_at IS NULL); the
// caller treats that as a benign no-op (the interaction is already
// soft-deleted; a deleted contact needs no recompute).
func (r *ContactRepository) RecomputeContactDatesAfterDeleteTx(ctx context.Context, tx pgx.Tx, contactID uuid.UUID, deletedAt time.Time) error {
	return recomputeContactDatesAfterDelete(ctx, db.New(tx), contactID, deletedAt)
}

// RecomputeContactDatesAfterDelete is the non-tx variant of
// RecomputeContactDatesAfterDeleteTx, used for direct testing. The two
// queries run on the repository's base querier rather than a caller tx.
func (r *ContactRepository) RecomputeContactDatesAfterDelete(ctx context.Context, contactID uuid.UUID, deletedAt time.Time) error {
	return recomputeContactDatesAfterDelete(ctx, r.queries, contactID, deletedAt)
}

// recomputeContactDatesAfterDelete is the shared orchestration body for the
// tx and non-tx wrappers. Reads the recomputed timestamps + the fields the
// contact_by decision needs, decides contact_by in Go, then writes.
func recomputeContactDatesAfterDelete(ctx context.Context, q db.Querier, contactID uuid.UUID, deletedAt time.Time) error {
	row, err := q.ComputeContactDatesAfterDelete(ctx, db.ComputeContactDatesAfterDeleteParams{
		ID:          uuidToPgUUID(contactID),
		DeletedAtTs: pgtype.Timestamptz{Time: deletedAt, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.ErrNotFound
		}
		return err
	}

	newContactBy := decideContactByAfterDelete(row)

	return q.WriteContactDatesAfterDelete(ctx, db.WriteContactDatesAfterDeleteParams{
		NewLastContacted:     row.NewLastContacted,
		NewLastInteractionAt: row.NewLastInteractionAt,
		NewLastResponseAt:    row.NewLastResponseAt,
		NewLastOutreachAt:    row.NewLastOutreachAt,
		NewContactBy:         newContactBy,
		ID:                   uuidToPgUUID(contactID),
	})
}

// decideContactByAfterDelete computes the contact_by value to write after a
// removal, mirroring the forward writer's environment-aware
// cadence.CalculateContactBy. It NEVER clobbers a Todoist/user override:
//
//   - If last_contacted did not move (new == old) or there is no cadence,
//     contact_by is preserved as-is.
//   - Else, if the existing contact_by differs from what the cadence would
//     have produced for old_last_contacted, a Todoist/user override landed
//     since the interaction; contact_by is preserved.
//   - Else (override-free), contact_by is re-derived from the new base
//     (new_last_contacted, or created_at when it rolled to NULL) so the
//     removed interaction's own contribution is undone exactly.
func decideContactByAfterDelete(row *db.ComputeContactDatesAfterDeleteRow) pgtype.Date {
	oldContactBy := row.OldContactBy

	cadenceStr := ""
	if row.Cadence.Valid {
		cadenceStr = row.Cadence.String
	}
	if cadenceStr == "" {
		return oldContactBy
	}

	oldLastContacted := pgTimestamptzToTimePtr(row.OldLastContacted)
	newLastContacted := pgTimestamptzToTimePtr(row.NewLastContacted)

	// last_contacted did not move → contact_by is not implicated; preserve.
	if !lastContactedMoved(oldLastContacted, newLastContacted) {
		return oldContactBy
	}

	cadenceType, err := cadence.ParseCadence(cadenceStr)
	if err != nil {
		return oldContactBy
	}

	// Override guard: if the stored contact_by differs from what the cadence
	// would have produced for the old last_contacted, a Todoist/user override
	// landed since the interaction; preserve it.
	if oldLastContacted != nil && row.OldContactBy.Valid {
		expectedOldCb := cadence.CalculateContactBy(*oldLastContacted, cadenceType)
		if !contactByMatchesStoredDate(row.OldContactBy, expectedOldCb) {
			return oldContactBy
		}
	}

	// Override-free: re-derive contact_by from the new base. When
	// last_contacted rolled to NULL, fall back to created_at so a cadence
	// contact is never dropped from due tracking.
	base := newLastContacted
	if base == nil {
		createdAt := pgTimestamptzToTimePtr(row.CreatedAt)
		base = createdAt
	}
	if base == nil {
		// No base to compute from (should not happen — created_at is NOT
		// NULL). Preserve rather than write a garbage value.
		return oldContactBy
	}
	newCb := cadence.CalculateContactBy(*base, cadenceType)
	return pgtype.Date{Time: newCb, Valid: true}
}

// lastContactedMoved reports whether last_contacted changed between the
// pre- and post-recompute values (including NULL transitions).
func lastContactedMoved(old, new *time.Time) bool {
	if old == nil && new == nil {
		return false
	}
	if old == nil || new == nil {
		return true
	}
	return !old.Equal(*new)
}

// contactByMatchesStoredDate reports whether the stored contact_by DATE
// equals the cadence-derived expected value, compared at DATE precision.
// contact_by is a DATE column, so the stored value is always day-precision
// regardless of mode; the expected value is reduced to its date via
// cadence.DateOnly (the same truncation the forward writer applies when it
// stores contact_by). Comparing the resulting Y/M/D components mirrors the
// existing contact_by parity tests (TestContactBy_* in backend/tests) and is
// environment-independent. A mismatch means a Todoist/user override landed
// since the interaction.
func contactByMatchesStoredDate(stored pgtype.Date, expected time.Time) bool {
	if !stored.Valid {
		return false
	}
	sY, sM, sD := stored.Time.Date()
	expectedDate := cadence.DateOnly(expected)
	eY, eM, eD := expectedDate.Date()
	return sY == eY && sM == eM && sD == eD
}
