package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// MatchStatus represents the match status of an external contact
type MatchStatus string

const (
	MatchStatusMatched   MatchStatus = "matched"
	MatchStatusUnmatched MatchStatus = "unmatched"
	MatchStatusIgnored   MatchStatus = "ignored"
	MatchStatusImported  MatchStatus = "imported"
)

// EmailEntry represents an email in an external contact
type EmailEntry struct {
	Value   string `json:"value"`
	Type    string `json:"type,omitempty"`
	Primary bool   `json:"primary,omitempty"`
}

// PhoneEntry represents a phone number in an external contact
type PhoneEntry struct {
	Value   string `json:"value"`
	Type    string `json:"type,omitempty"`
	Primary bool   `json:"primary,omitempty"`
}

// AddressEntry represents an address in an external contact
type AddressEntry struct {
	Formatted string `json:"formatted"`
	Type      string `json:"type,omitempty"`
}

// PendingMethodSuggestion is one (type,value) entry in the
// pending_method_suggestions / dismissed_method_suggestions JSONB
// columns. Value is the normalized method value (the reconcile path
// stores normalized values so dedup against contact methods and against
// the dismissed set is direct).
type PendingMethodSuggestion struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// ReconcileTarget is one address-book row resolved to its effective CRM
// contact + effective match status for the method reconcile. The
// driver query (ListLinkedAddressBookExternalContactsForReconcile)
// joins a duplicate row to its canonical and the repository computes the
// D2a precedence (`ignored > imported > matched`); the service then
// branches on EffectiveStatus. ExternalContact is the row whose
// emails[]/phones[] carry the methods to reconcile (for a duplicate,
// the dup's own methods reconcile against the CANONICAL's contact).
type ReconcileTarget struct {
	ExternalContact    ExternalContact
	EffectiveContactID uuid.UUID
	EffectiveStatus    MatchStatus
}

// ExternalContact represents an external contact from Google/iCloud
type ExternalContact struct {
	ID            uuid.UUID      `json:"id"`
	Source        string         `json:"source"`
	SourceID      string         `json:"source_id"`
	AccountID     *string        `json:"account_id,omitempty"`
	DisplayName   *string        `json:"display_name,omitempty"`
	FirstName     *string        `json:"first_name,omitempty"`
	LastName      *string        `json:"last_name,omitempty"`
	Emails        []EmailEntry   `json:"emails"`
	Phones        []PhoneEntry   `json:"phones"`
	Addresses     []AddressEntry `json:"addresses"`
	Organization  *string        `json:"organization,omitempty"`
	JobTitle      *string        `json:"job_title,omitempty"`
	Birthday      *time.Time     `json:"birthday,omitempty"`
	PhotoURL      *string        `json:"photo_url,omitempty"`
	CRMContactID  *uuid.UUID     `json:"crm_contact_id,omitempty"`
	MatchStatus   MatchStatus    `json:"match_status"`
	DuplicateOfID *uuid.UUID     `json:"duplicate_of_id,omitempty"`
	Etag          *string        `json:"etag,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	SyncedAt      *time.Time     `json:"synced_at,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
	UpdatedAt     time.Time      `json:"updated_at"`
	// DeletedAt is set when a soft-delete tombstones the row. Production
	// read queries filter `deleted_at IS NULL` so a tombstoned row only
	// surfaces through GetBySource (intentionally tombstone-aware for the
	// mac-daemon revive path).
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	// HostID is the paired Mac host that claimed this row (mac-daemon
	// sources only). Written on INSERT and on UPDATE via
	// COALESCE(existing, EXCLUDED) — a NULL row (legacy or never-claimed)
	// is claimed by the next non-NULL emit; non-NULL ownership is
	// preserved thereafter across all subsequent upserts.
	HostID *uuid.UUID `json:"host_id,omitempty"`
	// PendingMethodSuggestions is the current un-applied missing-method
	// set recorded for a linked `imported` address-book row (nil when no
	// suggestions). Stored in a dedicated JSONB column the producer
	// upsert never writes, so it survives address-book resyncs. Inert in
	// PR 1 (written by the reconcile path, read by the PR-2 surface).
	PendingMethodSuggestions []PendingMethodSuggestion `json:"pending_method_suggestions,omitempty"`
	// DismissedMethodSuggestions is the append-only set of (type,value)
	// the user has dismissed for this row (nil when nothing dismissed).
	// The reconcile path subtracts these so a dismissed method is never
	// re-suggested. Also a dedicated, upsert-surviving JSONB column.
	DismissedMethodSuggestions []PendingMethodSuggestion `json:"dismissed_method_suggestions,omitempty"`
	// LastContentHash is the lowercase-hex SHA-256 of the JCS-
	// canonicalized payload (minus host_id) that produced this row's
	// current content. Written on every UPSERT for mac-daemon sources;
	// preserved on soft-delete so a future delete-event can validate
	// against the prior hash. Powers GET /sync/:source/known-ids.
	LastContentHash *string `json:"last_content_hash,omitempty"`
}

// KnownExternalContactID is the (source_id, last_content_hash) pair
// returned by GET /api/v1/host/:id/sync/:source/known-ids. The daemon
// uses it to (a) drive tombstone reconciliation after a full
// CNContactStore scan, and (b) construct deterministic delete
// source_ids of the form `<entity>@deleted@<prev_hash>` per the
// mac-daemon spec. LastContentHash is nil for legacy rows from
// before the column existed; the daemon falls back to the
// `@deleted@unknown` sentinel.
type KnownExternalContactID struct {
	SourceID        string  `json:"source_id"`
	LastContentHash *string `json:"last_content_hash"`
}

// UpsertTelegramDiscoveryCandidateRequest carries the Telegram-specific fields
// used by the peer matcher. Nil name fields are preserved (never overwrite a
// previously-captured name). Metadata is merged with existing stored metadata.
type UpsertTelegramDiscoveryCandidateRequest struct {
	SourceID    string         `json:"source_id"`
	DisplayName *string        `json:"display_name,omitempty"`
	FirstName   *string        `json:"first_name,omitempty"`
	LastName    *string        `json:"last_name,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	SyncedAt    *time.Time     `json:"synced_at,omitempty"`
}

// AnarlogTitleGroup is one normalized-token group of anarlog_title weak
// candidates surfaced on the People-tab discovery section. EvidenceCount
// is the number of member external_contact rows for the token (the
// authoritative ranking signal); SessionTitles are the distinct
// human-readable session titles joined from meeting_note (display only —
// may be shorter than EvidenceCount when a source session was
// tombstoned). MemberIDs is for diagnostics/tests; the resolve path
// re-derives the sibling set server-side from the token and never trusts
// a client-supplied id list.
type AnarlogTitleGroup struct {
	NormalizedToken string      `json:"normalized_token"`
	TokenDisplay    string      `json:"token_display"`
	EvidenceCount   int64       `json:"evidence_count"`
	MemberIDs       []uuid.UUID `json:"member_ids"`
	SessionTitles   []string    `json:"session_titles"`
}

// UpsertExternalContactRequest holds parameters for creating/updating an external contact
type UpsertExternalContactRequest struct {
	Source       string         `json:"source"`
	SourceID     string         `json:"source_id"`
	AccountID    *string        `json:"account_id,omitempty"`
	DisplayName  *string        `json:"display_name,omitempty"`
	FirstName    *string        `json:"first_name,omitempty"`
	LastName     *string        `json:"last_name,omitempty"`
	Emails       []EmailEntry   `json:"emails,omitempty"`
	Phones       []PhoneEntry   `json:"phones,omitempty"`
	Addresses    []AddressEntry `json:"addresses,omitempty"`
	Organization *string        `json:"organization,omitempty"`
	JobTitle     *string        `json:"job_title,omitempty"`
	Birthday     *time.Time     `json:"birthday,omitempty"`
	PhotoURL     *string        `json:"photo_url,omitempty"`
	Etag         *string        `json:"etag,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	SyncedAt     *time.Time     `json:"synced_at,omitempty"`
	// HostID is set ONLY by mac-daemon ingest paths. The underlying
	// sqlc query writes it on INSERT, and on UPDATE via
	// COALESCE(existing, EXCLUDED) — legacy NULL rows are claimed by
	// the next non-NULL emit, and non-NULL ownership is preserved
	// across all subsequent upserts.
	HostID *uuid.UUID `json:"host_id,omitempty"`
	// LastContentHash is the lowercase-hex SHA-256 of the JCS-
	// canonicalized payload (minus host_id). Set by mac-daemon ingest
	// paths after the ingest layer verifies the hash matches the
	// envelope's source_id suffix. Written on both INSERT and UPDATE.
	LastContentHash *string `json:"last_content_hash,omitempty"`
}

// ExternalContactRepository handles external contact persistence
type ExternalContactRepository struct {
	queries db.Querier
}

// NewExternalContactRepository creates a new external contact repository
func NewExternalContactRepository(queries db.Querier) *ExternalContactRepository {
	return &ExternalContactRepository{queries: queries}
}

// reconcileRowToDbExternalContact projects the ec.* columns of a
// ListLinkedAddressBookExternalContactsForReconcileRow back into a
// db.ExternalContact so convertDbExternalContact can decode it. The
// canon_* columns are handled separately by the caller. The two structs
// share identical field names/types for the ec.* projection; this is a
// straight field copy.
func reconcileRowToDbExternalContact(row *db.ListLinkedAddressBookExternalContactsForReconcileRow) *db.ExternalContact {
	return &db.ExternalContact{
		ID:                         row.ID,
		Source:                     row.Source,
		SourceID:                   row.SourceID,
		AccountID:                  row.AccountID,
		DisplayName:                row.DisplayName,
		FirstName:                  row.FirstName,
		LastName:                   row.LastName,
		Emails:                     row.Emails,
		Phones:                     row.Phones,
		Addresses:                  row.Addresses,
		Organization:               row.Organization,
		JobTitle:                   row.JobTitle,
		Birthday:                   row.Birthday,
		PhotoUrl:                   row.PhotoUrl,
		CrmContactID:               row.CrmContactID,
		MatchStatus:                row.MatchStatus,
		DuplicateOfID:              row.DuplicateOfID,
		Etag:                       row.Etag,
		Metadata:                   row.Metadata,
		SyncedAt:                   row.SyncedAt,
		CreatedAt:                  row.CreatedAt,
		UpdatedAt:                  row.UpdatedAt,
		DeletedAt:                  row.DeletedAt,
		HostID:                     row.HostID,
		LastContentHash:            row.LastContentHash,
		PendingMethodSuggestions:   row.PendingMethodSuggestions,
		DismissedMethodSuggestions: row.DismissedMethodSuggestions,
	}
}

// convertDbExternalContact converts a database external contact to a repository model
func convertDbExternalContact(dbContact *db.ExternalContact) (*ExternalContact, error) {
	contact := &ExternalContact{
		Source:      dbContact.Source,
		SourceID:    dbContact.SourceID,
		MatchStatus: MatchStatus(dbContact.MatchStatus.String),
	}

	// Convert UUID
	if dbContact.ID.Valid {
		contact.ID = uuid.UUID(dbContact.ID.Bytes)
	}

	// Convert optional strings
	if dbContact.AccountID.Valid {
		contact.AccountID = &dbContact.AccountID.String
	}
	if dbContact.DisplayName.Valid {
		contact.DisplayName = &dbContact.DisplayName.String
	}
	if dbContact.FirstName.Valid {
		contact.FirstName = &dbContact.FirstName.String
	}
	if dbContact.LastName.Valid {
		contact.LastName = &dbContact.LastName.String
	}
	if dbContact.Organization.Valid {
		contact.Organization = &dbContact.Organization.String
	}
	if dbContact.JobTitle.Valid {
		contact.JobTitle = &dbContact.JobTitle.String
	}
	if dbContact.PhotoUrl.Valid {
		contact.PhotoURL = &dbContact.PhotoUrl.String
	}
	if dbContact.Etag.Valid {
		contact.Etag = &dbContact.Etag.String
	}

	// Convert birthday
	if dbContact.Birthday.Valid {
		t := dbContact.Birthday.Time
		contact.Birthday = &t
	}

	// Convert CRM contact ID
	if dbContact.CrmContactID.Valid {
		id := uuid.UUID(dbContact.CrmContactID.Bytes)
		contact.CRMContactID = &id
	}

	// Convert duplicate of ID
	if dbContact.DuplicateOfID.Valid {
		id := uuid.UUID(dbContact.DuplicateOfID.Bytes)
		contact.DuplicateOfID = &id
	}

	// Parse JSONB fields
	if len(dbContact.Emails) > 0 {
		if err := json.Unmarshal(dbContact.Emails, &contact.Emails); err != nil {
			contact.Emails = []EmailEntry{}
		}
	} else {
		contact.Emails = []EmailEntry{}
	}

	if len(dbContact.Phones) > 0 {
		if err := json.Unmarshal(dbContact.Phones, &contact.Phones); err != nil {
			contact.Phones = []PhoneEntry{}
		}
	} else {
		contact.Phones = []PhoneEntry{}
	}

	if len(dbContact.Addresses) > 0 {
		if err := json.Unmarshal(dbContact.Addresses, &contact.Addresses); err != nil {
			contact.Addresses = []AddressEntry{}
		}
	} else {
		contact.Addresses = []AddressEntry{}
	}

	if len(dbContact.Metadata) > 0 {
		if err := json.Unmarshal(dbContact.Metadata, &contact.Metadata); err != nil {
			contact.Metadata = map[string]any{}
		}
	} else {
		contact.Metadata = map[string]any{}
	}

	// Pending / dismissed method suggestions: nil-safe. A NULL column
	// (the default for every row not touched by the reconcile path)
	// leaves the slice nil, which the reconcile / suggestion-list logic
	// treats as "none". A malformed value is also coerced to nil rather
	// than failing the whole row conversion.
	contact.PendingMethodSuggestions = parseMethodSuggestions(dbContact.PendingMethodSuggestions)
	contact.DismissedMethodSuggestions = parseMethodSuggestions(dbContact.DismissedMethodSuggestions)

	// Convert timestamps
	if dbContact.SyncedAt.Valid {
		contact.SyncedAt = &dbContact.SyncedAt.Time
	}
	if dbContact.CreatedAt.Valid {
		contact.CreatedAt = dbContact.CreatedAt.Time
	}
	if dbContact.UpdatedAt.Valid {
		contact.UpdatedAt = dbContact.UpdatedAt.Time
	}
	if dbContact.DeletedAt.Valid {
		t := dbContact.DeletedAt.Time
		contact.DeletedAt = &t
	}
	if dbContact.HostID.Valid {
		id := uuid.UUID(dbContact.HostID.Bytes)
		contact.HostID = &id
	}
	if dbContact.LastContentHash.Valid {
		contact.LastContentHash = &dbContact.LastContentHash.String
	}

	return contact, nil
}

// GetByID retrieves an external contact by ID
func (r *ExternalContactRepository) GetByID(ctx context.Context, id uuid.UUID) (*ExternalContact, error) {
	dbContact, err := r.queries.GetExternalContact(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return convertDbExternalContact(dbContact)
}

// GetBySource retrieves an external contact by source and source_id
func (r *ExternalContactRepository) GetBySource(ctx context.Context, source, sourceID string, accountID *string) (*ExternalContact, error) {
	var accountIDText pgtype.Text
	if accountID != nil {
		accountIDText = pgtype.Text{String: *accountID, Valid: true}
	}

	dbContact, err := r.queries.GetExternalContactBySource(ctx, db.GetExternalContactBySourceParams{
		Source:    source,
		SourceID:  sourceID,
		AccountID: accountIDText,
	})
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return convertDbExternalContact(dbContact)
}

// Upsert creates or updates an external contact
func (r *ExternalContactRepository) Upsert(ctx context.Context, req UpsertExternalContactRequest) (*ExternalContact, error) {
	dbContact, err := r.queries.UpsertExternalContact(ctx, buildUpsertExternalContactParams(req))
	if err != nil {
		return nil, err
	}
	return convertDbExternalContact(dbContact)
}

// buildUpsertExternalContactParams centralizes the pgtype + JSONB
// conversion shared between Upsert (non-tx) and UpsertTx. Keeping it in
// one place ensures both variants stay in lockstep — drift would
// silently produce different on-disk shapes depending on caller.
func buildUpsertExternalContactParams(req UpsertExternalContactRequest) db.UpsertExternalContactParams {
	emailsJSON, _ := json.Marshal(req.Emails)
	if req.Emails == nil {
		emailsJSON = []byte("[]")
	}
	phonesJSON, _ := json.Marshal(req.Phones)
	if req.Phones == nil {
		phonesJSON = []byte("[]")
	}
	addressesJSON, _ := json.Marshal(req.Addresses)
	if req.Addresses == nil {
		addressesJSON = []byte("[]")
	}
	metadataJSON, _ := json.Marshal(req.Metadata)
	if req.Metadata == nil {
		metadataJSON = []byte("{}")
	}

	params := db.UpsertExternalContactParams{
		Source:      req.Source,
		SourceID:    req.SourceID,
		Emails:      emailsJSON,
		Phones:      phonesJSON,
		Addresses:   addressesJSON,
		Metadata:    metadataJSON,
		MatchStatus: pgtype.Text{String: string(MatchStatusUnmatched), Valid: true},
	}

	if req.AccountID != nil {
		params.AccountID = pgtype.Text{String: *req.AccountID, Valid: true}
	}
	if req.DisplayName != nil {
		params.DisplayName = pgtype.Text{String: *req.DisplayName, Valid: true}
	}
	if req.FirstName != nil {
		params.FirstName = pgtype.Text{String: *req.FirstName, Valid: true}
	}
	if req.LastName != nil {
		params.LastName = pgtype.Text{String: *req.LastName, Valid: true}
	}
	if req.Organization != nil {
		params.Organization = pgtype.Text{String: *req.Organization, Valid: true}
	}
	if req.JobTitle != nil {
		params.JobTitle = pgtype.Text{String: *req.JobTitle, Valid: true}
	}
	if req.PhotoURL != nil {
		params.PhotoUrl = pgtype.Text{String: *req.PhotoURL, Valid: true}
	}
	if req.Etag != nil {
		params.Etag = pgtype.Text{String: *req.Etag, Valid: true}
	}
	if req.Birthday != nil {
		params.Birthday = pgtype.Date{Time: *req.Birthday, Valid: true}
	}
	if req.SyncedAt != nil {
		params.SyncedAt = pgtype.Timestamptz{Time: *req.SyncedAt, Valid: true}
	}
	if req.HostID != nil {
		params.HostID = pgtype.UUID{Bytes: *req.HostID, Valid: true}
	}
	if req.LastContentHash != nil {
		params.LastContentHash = pgtype.Text{String: *req.LastContentHash, Valid: true}
	}
	return params
}

// UpsertTelegramDiscoveryCandidate inserts or updates a Telegram discovery
// candidate via the dedicated SQL query. Unlike the shared Upsert, nil name
// fields are preserved (never overwrite a stored value) and metadata is merged
// with the existing stored map instead of replacing it. Unrelated columns
// (emails, phones, addresses, organization, job_title, birthday, photo_url,
// etag, account_id) are not touched on update.
func (r *ExternalContactRepository) UpsertTelegramDiscoveryCandidate(
	ctx context.Context,
	req UpsertTelegramDiscoveryCandidateRequest,
) (*ExternalContact, error) {
	metadata := req.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadataBytes, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("marshal metadata: %w", err)
	}

	params := db.UpsertTelegramDiscoveryCandidateParams{
		SourceID:    req.SourceID,
		DisplayName: stringToPgText(req.DisplayName),
		FirstName:   stringToPgText(req.FirstName),
		LastName:    stringToPgText(req.LastName),
		Metadata:    metadataBytes,
		SyncedAt:    timeToPgTimestamptz(req.SyncedAt),
	}
	dbContact, err := r.queries.UpsertTelegramDiscoveryCandidate(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("upsert telegram discovery candidate: %w", err)
	}
	return convertDbExternalContact(dbContact)
}

// ListUnmatched returns unmatched external contacts for a source
func (r *ExternalContactRepository) ListUnmatched(ctx context.Context, source string, limit, offset int32) ([]ExternalContact, error) {
	dbContacts, err := r.queries.ListUnmatchedExternalContacts(ctx, db.ListUnmatchedExternalContactsParams{
		Source: source,
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return nil, err
	}

	contacts := make([]ExternalContact, 0, len(dbContacts))
	for _, dbContact := range dbContacts {
		contact, err := convertDbExternalContact(dbContact)
		if err != nil {
			continue
		}
		contacts = append(contacts, *contact)
	}
	return contacts, nil
}

// ListAllUnmatched returns all unmatched external contacts across sources
func (r *ExternalContactRepository) ListAllUnmatched(ctx context.Context, limit, offset int32) ([]ExternalContact, error) {
	dbContacts, err := r.queries.ListAllUnmatchedExternalContacts(ctx, db.ListAllUnmatchedExternalContactsParams{
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return nil, err
	}

	contacts := make([]ExternalContact, 0, len(dbContacts))
	for _, dbContact := range dbContacts {
		contact, err := convertDbExternalContact(dbContact)
		if err != nil {
			continue
		}
		contacts = append(contacts, *contact)
	}
	return contacts, nil
}

// CountUnmatched returns the count of unmatched contacts for a source
func (r *ExternalContactRepository) CountUnmatched(ctx context.Context, source string) (int64, error) {
	return r.queries.CountUnmatchedExternalContacts(ctx, source)
}

// CountAllUnmatched returns the count of all unmatched contacts
func (r *ExternalContactRepository) CountAllUnmatched(ctx context.Context) (int64, error) {
	return r.queries.CountAllUnmatchedExternalContacts(ctx)
}

// UpdateMatch updates the CRM contact ID and match status. Returns
// db.ErrNotFound when the target row is tombstoned (the underlying
// query filters `deleted_at IS NULL`) or has been hard-deleted.
func (r *ExternalContactRepository) UpdateMatch(ctx context.Context, id uuid.UUID, crmContactID *uuid.UUID, status MatchStatus) (*ExternalContact, error) {
	var crmContactIDPg pgtype.UUID
	if crmContactID != nil {
		crmContactIDPg = pgtype.UUID{Bytes: *crmContactID, Valid: true}
	}

	dbContact, err := r.queries.UpdateExternalContactMatch(ctx, db.UpdateExternalContactMatchParams{
		ID:           pgtype.UUID{Bytes: id, Valid: true},
		CrmContactID: crmContactIDPg,
		MatchStatus:  pgtype.Text{String: string(status), Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	return convertDbExternalContact(dbContact)
}

// MarkAsDuplicate marks an external contact as a duplicate of another
func (r *ExternalContactRepository) MarkAsDuplicate(ctx context.Context, id, duplicateOfID uuid.UUID) error {
	return r.queries.UpdateExternalContactDuplicate(ctx, db.UpdateExternalContactDuplicateParams{
		ID:            pgtype.UUID{Bytes: id, Valid: true},
		DuplicateOfID: pgtype.UUID{Bytes: duplicateOfID, Valid: true},
	})
}

// Ignore marks an external contact as ignored
func (r *ExternalContactRepository) Ignore(ctx context.Context, id uuid.UUID) error {
	return r.queries.IgnoreExternalContact(ctx, pgtype.UUID{Bytes: id, Valid: true})
}

// FindBySourceAndSourceID finds all unmatched external_contact rows for a
// (source, source_id) pair regardless of account_id. Used by the calendar
// rematch handler to mark gcal_attendee candidates as matched.
func (r *ExternalContactRepository) FindBySourceAndSourceID(ctx context.Context, source, sourceID string) ([]ExternalContact, error) {
	dbContacts, err := r.queries.FindExternalContactsBySourceAndSourceID(ctx, db.FindExternalContactsBySourceAndSourceIDParams{
		Source:   source,
		SourceID: sourceID,
	})
	if err != nil {
		return nil, err
	}

	contacts := make([]ExternalContact, 0, len(dbContacts))
	for _, dbContact := range dbContacts {
		contact, err := convertDbExternalContact(dbContact)
		if err != nil {
			continue
		}
		contacts = append(contacts, *contact)
	}
	return contacts, nil
}

// FindByNormalizedEmail finds external contacts by normalized email
func (r *ExternalContactRepository) FindByNormalizedEmail(ctx context.Context, email string) ([]ExternalContact, error) {
	dbContacts, err := r.queries.FindExternalContactsByNormalizedEmail(ctx, email)
	if err != nil {
		return nil, err
	}

	contacts := make([]ExternalContact, 0, len(dbContacts))
	for _, dbContact := range dbContacts {
		contact, err := convertDbExternalContact(dbContact)
		if err != nil {
			continue
		}
		contacts = append(contacts, *contact)
	}
	return contacts, nil
}

// ListForCRMContact returns external contacts linked to a CRM contact
func (r *ExternalContactRepository) ListForCRMContact(ctx context.Context, crmContactID uuid.UUID) ([]ExternalContact, error) {
	dbContacts, err := r.queries.ListExternalContactsForCRMContact(ctx, pgtype.UUID{Bytes: crmContactID, Valid: true})
	if err != nil {
		return nil, err
	}

	contacts := make([]ExternalContact, 0, len(dbContacts))
	for _, dbContact := range dbContacts {
		contact, err := convertDbExternalContact(dbContact)
		if err != nil {
			continue
		}
		contacts = append(contacts, *contact)
	}
	return contacts, nil
}

// parseMethodSuggestions decodes a pending/dismissed_method_suggestions
// JSONB column into a slice. nil/empty bytes (SQL NULL) → nil slice.
// A malformed value → nil (the row is still usable; a bad suggestion
// cache must not poison reconcile).
func parseMethodSuggestions(raw []byte) []PendingMethodSuggestion {
	if len(raw) == 0 {
		return nil
	}
	var out []PendingMethodSuggestion
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// ListLinkedAddressBookExternalContactsForReconcile returns every live
// address-book row (source ∈ sources) that is itself linked OR is a
// duplicate of a live canonical row, each resolved to its effective CRM
// contact + effective match status per the D2a precedence
// (`ignored > imported > matched`). Rows whose effective status is
// `ignored` or that resolve to no live contact are dropped here so the
// caller never has to re-apply the precedence.
func (r *ExternalContactRepository) ListLinkedAddressBookExternalContactsForReconcile(
	ctx context.Context,
	sources []string,
) ([]ReconcileTarget, error) {
	rows, err := r.queries.ListLinkedAddressBookExternalContactsForReconcile(ctx, sources)
	if err != nil {
		return nil, fmt.Errorf("list linked address-book external contacts: %w", err)
	}
	targets := make([]ReconcileTarget, 0, len(rows))
	for _, row := range rows {
		contact, convErr := convertDbExternalContact(reconcileRowToDbExternalContact(row))
		if convErr != nil {
			continue
		}

		// Resolve the canonical's contact + status (a duplicate row joins
		// to its canonical; a self-linked row has no canonical).
		var canonContactID *uuid.UUID
		if row.CanonCrmContactID.Valid {
			id := uuid.UUID(row.CanonCrmContactID.Bytes)
			canonContactID = &id
		}
		canonStatus := MatchStatus("")
		if row.CanonMatchStatus.Valid {
			canonStatus = MatchStatus(row.CanonMatchStatus.String)
		}

		effectiveContactID, effectiveStatus, ok := resolveEffectiveReconcileState(
			contact.CRMContactID, contact.MatchStatus, canonContactID, canonStatus,
		)
		if !ok {
			continue
		}
		targets = append(targets, ReconcileTarget{
			ExternalContact:    *contact,
			EffectiveContactID: effectiveContactID,
			EffectiveStatus:    effectiveStatus,
		})
	}
	return targets, nil
}

// ResolveReconcileTarget resolves a single live address-book row (by id)
// into a ReconcileTarget using the same D2a precedence as the catchup
// driver. For a duplicate row it reads the canonical to resolve the
// effective contact/status; for a self-linked row the canonical lookup
// is skipped. Returns (nil, nil) — a no-op signal — when the row is
// missing/tombstoned, resolves to no live contact, or is effectively
// ignored. Used by the forward hooks (gcontacts processContact, icloud
// post-commit) so the dup-of-linked case reconciles the same way the
// catchup does, not via a self-only shortcut.
func (r *ExternalContactRepository) ResolveReconcileTarget(
	ctx context.Context,
	id uuid.UUID,
) (*ReconcileTarget, error) {
	row, err := r.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, nil
	}

	var canonContactID *uuid.UUID
	canonStatus := MatchStatus("")
	if row.DuplicateOfID != nil {
		canon, canonErr := r.GetByID(ctx, *row.DuplicateOfID)
		if canonErr != nil {
			return nil, canonErr
		}
		// GetByID filters deleted_at IS NULL, so a tombstoned canonical
		// surfaces as nil — leaving the row to fall back to its own
		// contact/status (matches the LEFT JOIN ... canon.deleted_at IS
		// NULL behavior of the catchup driver).
		if canon != nil {
			canonContactID = canon.CRMContactID
			canonStatus = canon.MatchStatus
		}
	}

	effectiveContactID, effectiveStatus, ok := resolveEffectiveReconcileState(
		row.CRMContactID, row.MatchStatus, canonContactID, canonStatus,
	)
	if !ok {
		return nil, nil
	}
	return &ReconcileTarget{
		ExternalContact:    *row,
		EffectiveContactID: effectiveContactID,
		EffectiveStatus:    effectiveStatus,
	}, nil
}

// resolveEffectiveReconcileState applies the D2a effective-contact +
// effective-status precedence for a (possibly duplicate) address-book
// row. selfContactID/selfStatus are the row's own; canonContactID/
// canonStatus are its canonical's (nil/"" when the row is not a dup or
// the canonical is gone).
//
//   - Effective contact = canonical's contact if present (the canonical
//     is the source of truth; a dup's own crm_contact_id may be stale),
//     else the row's own.
//   - Effective status = most-conservative of the two via
//     `ignored > imported > matched`: if EITHER is ignored → skip
//     (ok=false); else if EITHER is imported → imported; else if EITHER
//     is matched → matched; else skip.
//
// ok=false means "do not reconcile this row" (ignored, or no live
// contact resolved).
func resolveEffectiveReconcileState(
	selfContactID *uuid.UUID,
	selfStatus MatchStatus,
	canonContactID *uuid.UUID,
	canonStatus MatchStatus,
) (uuid.UUID, MatchStatus, bool) {
	// Sticky-ignore dominates: an ignored row (or a dup of / pointing at
	// an ignored canonical) must never reconcile or suggest.
	if selfStatus == MatchStatusIgnored || canonStatus == MatchStatusIgnored {
		return uuid.Nil, "", false
	}

	effectiveContactID := canonContactID
	if effectiveContactID == nil {
		effectiveContactID = selfContactID
	}
	if effectiveContactID == nil {
		return uuid.Nil, "", false
	}

	switch {
	case selfStatus == MatchStatusImported || canonStatus == MatchStatusImported:
		return *effectiveContactID, MatchStatusImported, true
	case selfStatus == MatchStatusMatched || canonStatus == MatchStatusMatched:
		return *effectiveContactID, MatchStatusMatched, true
	default:
		return uuid.Nil, "", false
	}
}

// SetMethodSuggestions overwrites the pending suggestion set for a row.
// An empty/nil slice writes SQL NULL (D6 empty-clears), so a method
// later applied by another path clears the stale suggestion on the next
// reconcile. Writes the dedicated column, never `metadata`.
func (r *ExternalContactRepository) SetMethodSuggestions(
	ctx context.Context,
	id uuid.UUID,
	pending []PendingMethodSuggestion,
) (*ExternalContact, error) {
	var pendingJSON []byte
	if len(pending) > 0 {
		marshalled, err := json.Marshal(pending)
		if err != nil {
			return nil, fmt.Errorf("marshal pending method suggestions: %w", err)
		}
		pendingJSON = marshalled
	}
	dbContact, err := r.queries.SetExternalContactMethodSuggestions(ctx, db.SetExternalContactMethodSuggestionsParams{
		ID:      pgtype.UUID{Bytes: id, Valid: true},
		Pending: pendingJSON,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, fmt.Errorf("set method suggestions: %w", err)
	}
	return convertDbExternalContact(dbContact)
}

// SetDismissedMethodSuggestionsForTest pre-seeds the
// dismissed_method_suggestions column. TEST ONLY — production dismissal
// (PR 2) appends via a read-modify-write path; this exists so
// integration tests can establish the dismissed pre-state without raw
// SQL in Go.
func (r *ExternalContactRepository) SetDismissedMethodSuggestionsForTest(
	ctx context.Context,
	id uuid.UUID,
	dismissed []PendingMethodSuggestion,
) (*ExternalContact, error) {
	var dismissedJSON []byte
	if len(dismissed) > 0 {
		marshalled, err := json.Marshal(dismissed)
		if err != nil {
			return nil, fmt.Errorf("marshal dismissed method suggestions: %w", err)
		}
		dismissedJSON = marshalled
	}
	dbContact, err := r.queries.SetDismissedMethodSuggestionsForTest(ctx, db.SetDismissedMethodSuggestionsForTestParams{
		ID:        pgtype.UUID{Bytes: id, Valid: true},
		Dismissed: dismissedJSON,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, fmt.Errorf("set dismissed method suggestions (test): %w", err)
	}
	return convertDbExternalContact(dbContact)
}

// Delete removes an external contact
func (r *ExternalContactRepository) Delete(ctx context.Context, id uuid.UUID) error {
	return r.queries.DeleteExternalContact(ctx, pgtype.UUID{Bytes: id, Valid: true})
}

// GetBySourceTx retrieves an external contact by (source, source_id,
// account_id) within an active transaction. Tombstone-aware: returns
// tombstoned rows too (caller inspects `DeletedAt != nil` to decide).
// Returns (nil, db.ErrNotFound) on miss. Used by the mac-daemon ingest
// service's inline external_contact.upserted / .deleted handlers — both
// need visibility into tombstoned rows (upserted to revive, deleted to
// confirm an existing tombstone for idempotent no-op).
func (r *ExternalContactRepository) GetBySourceTx(
	ctx context.Context,
	tx pgx.Tx,
	source, sourceID string,
	accountID *string,
) (*ExternalContact, error) {
	var accountIDText pgtype.Text
	if accountID != nil {
		accountIDText = pgtype.Text{String: *accountID, Valid: true}
	}
	dbContact, err := db.New(tx).GetExternalContactBySource(ctx, db.GetExternalContactBySourceParams{
		Source:    source,
		SourceID:  sourceID,
		AccountID: accountIDText,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	return convertDbExternalContact(dbContact)
}

// UpsertTx is the tx-bound variant of Upsert. Caller owns the tx
// lifecycle. The underlying UpsertExternalContact query does NOT touch
// deleted_at, crm_contact_id, or match_status on the UPDATE branch —
// the revive path issues a separate ReviveTx call when the pre-read
// detected a tombstone.
func (r *ExternalContactRepository) UpsertTx(
	ctx context.Context,
	tx pgx.Tx,
	req UpsertExternalContactRequest,
) (*ExternalContact, error) {
	params := buildUpsertExternalContactParams(req)
	dbContact, err := db.New(tx).UpsertExternalContact(ctx, params)
	if err != nil {
		return nil, err
	}
	return convertDbExternalContact(dbContact)
}

// UpdateMatchTx is the tx-bound variant of UpdateMatch. Used by the
// mac-daemon ingest handler's first-insert path to set
// crm_contact_id + match_status when an identity match succeeds.
// Returns db.ErrNotFound when the target row is tombstoned (the
// underlying query filters `deleted_at IS NULL`) or hard-deleted.
// Caller owns the tx lifecycle.
func (r *ExternalContactRepository) UpdateMatchTx(
	ctx context.Context,
	tx pgx.Tx,
	id uuid.UUID,
	crmContactID *uuid.UUID,
	status MatchStatus,
) (*ExternalContact, error) {
	var crmContactIDPg pgtype.UUID
	if crmContactID != nil {
		crmContactIDPg = pgtype.UUID{Bytes: *crmContactID, Valid: true}
	}
	dbContact, err := db.New(tx).UpdateExternalContactMatch(ctx, db.UpdateExternalContactMatchParams{
		ID:           pgtype.UUID{Bytes: id, Valid: true},
		CrmContactID: crmContactIDPg,
		MatchStatus:  pgtype.Text{String: string(status), Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	return convertDbExternalContact(dbContact)
}

// ReviveTx clears `deleted_at` on a tombstoned row. The underlying SQL
// has a defensive `WHERE deleted_at IS NOT NULL` predicate so a
// concurrent revive that beat us is absorbed safely. Caller owns the
// tx lifecycle. Returns db.ErrNotFound when the row is already live
// (or does not exist) — callers should pre-check via GetBySourceTx to
// avoid spurious not-found.
func (r *ExternalContactRepository) ReviveTx(
	ctx context.Context,
	tx pgx.Tx,
	id uuid.UUID,
) (*ExternalContact, error) {
	dbContact, err := db.New(tx).ReviveExternalContact(ctx, pgtype.UUID{Bytes: id, Valid: true})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	return convertDbExternalContact(dbContact)
}

// SoftDeleteTx tombstones a row by setting deleted_at = NOW(). The
// underlying SQL has a defensive `WHERE deleted_at IS NULL` predicate
// so calling it twice is an idempotent no-op (no error on already-
// tombstoned rows). crm_contact_id, match_status, and duplicate_of_id
// are preserved. Caller owns the tx lifecycle.
func (r *ExternalContactRepository) SoftDeleteTx(
	ctx context.Context,
	tx pgx.Tx,
	id uuid.UUID,
) error {
	return db.New(tx).SoftDeleteExternalContact(ctx, pgtype.UUID{Bytes: id, Valid: true})
}

// ListKnownIDsByHostAndSource returns (source_id, last_content_hash)
// for every live external_contact row owned by (host_id, source).
// Tombstoned rows are excluded — the daemon's set-diff reconciliation
// requires that rows the Pi has soft-deleted are NOT reported as
// known, else the daemon never re-tombstones them. Powers GET
// /api/v1/host/:id/sync/:source/known-ids.
func (r *ExternalContactRepository) ListKnownIDsByHostAndSource(
	ctx context.Context,
	hostID uuid.UUID,
	source string,
) ([]KnownExternalContactID, error) {
	rows, err := r.queries.ListKnownExternalContactIDsByHostAndSource(ctx, db.ListKnownExternalContactIDsByHostAndSourceParams{
		HostID: pgtype.UUID{Bytes: hostID, Valid: true},
		Source: source,
	})
	if err != nil {
		return nil, fmt.Errorf("list known external contact IDs: %w", err)
	}
	out := make([]KnownExternalContactID, 0, len(rows))
	for _, row := range rows {
		entry := KnownExternalContactID{SourceID: row.SourceID}
		if row.LastContentHash.Valid {
			h := row.LastContentHash.String
			entry.LastContentHash = &h
		}
		out = append(out, entry)
	}
	return out, nil
}

// CountByHostAndSource returns a per-source count of live
// external_contact rows owned by host_id. Tombstoned + merge-dupe rows
// are excluded. Powers GET /api/v1/host/:id/source-counts (issue #327).
// Returns nil on zero rows so the caller can distinguish "no rows
// matched" from "query failed".
func (r *ExternalContactRepository) CountByHostAndSource(
	ctx context.Context,
	hostID uuid.UUID,
) (map[string]int, error) {
	rows, err := r.queries.CountExternalContactsByHostAndSource(ctx, pgtype.UUID{Bytes: hostID, Valid: true})
	if err != nil {
		return nil, fmt.Errorf("count external_contact by host+source: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make(map[string]int, len(rows))
	for _, row := range rows {
		out[row.Source] = int(row.Count)
	}
	return out, nil
}

// DeleteBySourceForTest hard-deletes ALL external_contact rows for a
// given source string. TEST ONLY: production code must not call this;
// it bypasses the tombstone contract and the crm_contact_id /
// match_status preservation rules. Used by integration tests that
// seed rows under a synthetic source and need targeted cleanup.
func (r *ExternalContactRepository) DeleteBySourceForTest(ctx context.Context, source string) error {
	_, err := r.queries.DeleteExternalContactsBySourceForTest(ctx, source)
	return err
}

// ListAnarlogTitleGroups returns the normalized-token groups of
// unmatched anarlog_title weak candidates for the discovery surface,
// ranked by member-row evidence count. Each group carries its distinct
// session titles (joined from meeting_note) for display.
func (r *ExternalContactRepository) ListAnarlogTitleGroups(ctx context.Context) ([]AnarlogTitleGroup, error) {
	rows, err := r.queries.ListAnarlogTitleGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("list anarlog_title groups: %w", err)
	}
	out := make([]AnarlogTitleGroup, 0, len(rows))
	for _, row := range rows {
		memberIDs := make([]uuid.UUID, 0, len(row.MemberIds))
		for _, mid := range row.MemberIds {
			if mid.Valid {
				memberIDs = append(memberIDs, uuid.UUID(mid.Bytes))
			}
		}
		titles := row.SessionTitles
		if titles == nil {
			titles = []string{}
		}
		out = append(out, AnarlogTitleGroup{
			NormalizedToken: row.NormalizedToken,
			TokenDisplay:    row.TokenDisplay,
			EvidenceCount:   row.EvidenceCount,
			MemberIDs:       memberIDs,
			SessionTitles:   titles,
		})
	}
	return out, nil
}

// FindAnarlogTitleSiblingsByToken returns every live unmatched
// anarlog_title sibling row for a normalized token, ordered by id ASC so
// the lowest-id row is a deterministic representative for the resolve
// path. An empty slice means the token group is already resolved (or
// never existed) — the resolve service maps that to a 404.
func (r *ExternalContactRepository) FindAnarlogTitleSiblingsByToken(ctx context.Context, normalizedToken string) ([]ExternalContact, error) {
	dbContacts, err := r.queries.FindAnarlogTitleSiblingsByToken(ctx, normalizedToken)
	if err != nil {
		return nil, fmt.Errorf("find anarlog_title siblings: %w", err)
	}
	contacts := make([]ExternalContact, 0, len(dbContacts))
	for _, dbContact := range dbContacts {
		contact, convErr := convertDbExternalContact(dbContact)
		if convErr != nil {
			continue
		}
		contacts = append(contacts, *contact)
	}
	return contacts, nil
}

// MarkAnarlogTitleSiblingsImportedByToken flips every live unmatched
// sibling for the token to 'imported' and points it at the newly created
// CRM contact, in a single atomic statement. Returns the number of rows
// marked so the caller can detect a concurrent resolve (zero rows).
func (r *ExternalContactRepository) MarkAnarlogTitleSiblingsImportedByToken(ctx context.Context, normalizedToken string, contactID uuid.UUID) (int64, error) {
	rows, err := r.queries.MarkAnarlogTitleSiblingsImportedByToken(ctx, db.MarkAnarlogTitleSiblingsImportedByTokenParams{
		CrmContactID:    uuidToPgUUID(contactID),
		NormalizedToken: normalizedToken,
	})
	if err != nil {
		return 0, fmt.Errorf("mark anarlog_title siblings imported: %w", err)
	}
	return rows, nil
}

// MarkAnarlogTitleSiblingsMatchedByToken flips every live unmatched
// sibling for the token to 'matched' and points it at the linked CRM
// contact, in a single atomic statement. Returns the number of rows
// marked so the caller can detect a concurrent resolve (zero rows).
func (r *ExternalContactRepository) MarkAnarlogTitleSiblingsMatchedByToken(ctx context.Context, normalizedToken string, contactID uuid.UUID) (int64, error) {
	rows, err := r.queries.MarkAnarlogTitleSiblingsMatchedByToken(ctx, db.MarkAnarlogTitleSiblingsMatchedByTokenParams{
		CrmContactID:    uuidToPgUUID(contactID),
		NormalizedToken: normalizedToken,
	})
	if err != nil {
		return 0, fmt.Errorf("mark anarlog_title siblings matched: %w", err)
	}
	return rows, nil
}

// MarkAnarlogTitleSiblingsIgnoredByToken flips every live unmatched
// sibling for the token to 'ignored' ("Not a person"), in a single
// atomic statement. No crm_contact_id is set.
func (r *ExternalContactRepository) MarkAnarlogTitleSiblingsIgnoredByToken(ctx context.Context, normalizedToken string) error {
	if err := r.queries.MarkAnarlogTitleSiblingsIgnoredByToken(ctx, normalizedToken); err != nil {
		return fmt.Errorf("mark anarlog_title siblings ignored: %w", err)
	}
	return nil
}
