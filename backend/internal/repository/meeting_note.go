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

// Linkage state constants for meeting_note.linkage_state. Match the
// CHECK constraint defined in migration 053. Kept here so the inline
// handler in service/ingest.go does not embed magic strings.
const (
	LinkageStateLinked               = "linked"
	LinkageStateLinkedImpromptu      = "linked_impromptu"
	LinkageStateOrphanTitleAugmented = "orphan_title_augmented" // reserved for PR 4
	LinkageStateOrphanNeedsReview    = "orphan_needs_review"
	LinkageStateConflictPending      = "conflict_pending"
)

// Linked-kind constants. Match the CHECK constraint defined in migration
// 053. PhoneCall is reserved for the deferred phase 1.5 phone_call table.
const (
	LinkedKindEvent     = "event"
	LinkedKindPhoneCall = "phone_call"
)

// MeetingNote is the repository-layer view of a meeting_note row.
// Participants is stored as a JSONB array of anarlog_human UUID strings
// (the payload shape); typed as []string in Go so the caller does not
// have to allocate a uuid.UUID slice for the linkage logic that only
// needs identifier-string comparisons.
type MeetingNote struct {
	ID               uuid.UUID
	AnarlogSessionID uuid.UUID
	Title            *string
	Summary          *string
	Memo             *string
	Participants     []string
	MacHostID        *uuid.UUID
	LinkedKind       *string
	LinkedID         *uuid.UUID
	LinkageState     string
	InputHash        string
	ResolvedSetHash  string
	LastContentHash  *string
	MeetingAt        time.Time
	DeletedAt        *time.Time
	CreatedAt        time.Time
}

// InsertMeetingNoteParams captures the per-row values the inline
// handler supplies on first-insert. Hashes default to empty string only
// at the migration boundary; the handler ALWAYS supplies real values.
type InsertMeetingNoteParams struct {
	AnarlogSessionID uuid.UUID
	Title            *string
	Summary          *string
	Memo             *string
	Participants     []string
	MacHostID        *uuid.UUID
	LinkedKind       *string
	LinkedID         *uuid.UUID
	LinkageState     string
	InputHash        string
	ResolvedSetHash  string
	LastContentHash  *string
	MeetingAt        time.Time
}

// UpdateMeetingNoteOnResyncParams is the value bag for both the
// carry-forward and re-link branches of the re-sync algorithm. The
// caller picks the right linkage values (carry-forward preserves
// prior; re-link computes fresh).
type UpdateMeetingNoteOnResyncParams struct {
	ID              uuid.UUID
	Title           *string
	Summary         *string
	Memo            *string
	Participants    []string
	LinkedKind      *string
	LinkedID        *uuid.UUID
	LinkageState    string
	InputHash       string
	ResolvedSetHash string
	LastContentHash *string
	MeetingAt       time.Time
}

// ReviveMeetingNoteParams is the same shape as the on-resync update,
// but the underlying query also clears deleted_at. Used when a tombstoned
// row re-receives meeting_note.recorded with the same source_id
// (round-1 P0#1 delete-revive semantics).
type ReviveMeetingNoteParams = UpdateMeetingNoteOnResyncParams

// MeetingNoteRepository owns persistence for the meeting_note table.
type MeetingNoteRepository struct {
	queries db.Querier
}

// NewMeetingNoteRepository builds a MeetingNoteRepository over the
// given querier.
func NewMeetingNoteRepository(queries db.Querier) *MeetingNoteRepository {
	return &MeetingNoteRepository{queries: queries}
}

// convertDbMeetingNote translates a generated db.MeetingNote row into the
// repository's typed view.
func convertDbMeetingNote(row *db.MeetingNote) (*MeetingNote, error) {
	if row == nil {
		return nil, nil
	}
	mn := &MeetingNote{
		LinkageState:    row.LinkageState,
		InputHash:       row.InputHash,
		ResolvedSetHash: row.ResolvedSetHash,
	}
	if row.ID.Valid {
		mn.ID = uuid.UUID(row.ID.Bytes)
	}
	if row.AnarlogSessionID.Valid {
		mn.AnarlogSessionID = uuid.UUID(row.AnarlogSessionID.Bytes)
	}
	if row.Title.Valid {
		mn.Title = &row.Title.String
	}
	if row.Summary.Valid {
		mn.Summary = &row.Summary.String
	}
	if row.Memo.Valid {
		mn.Memo = &row.Memo.String
	}
	if row.MacHostID.Valid {
		id := uuid.UUID(row.MacHostID.Bytes)
		mn.MacHostID = &id
	}
	if row.LinkedKind.Valid {
		mn.LinkedKind = &row.LinkedKind.String
	}
	if row.LinkedID.Valid {
		id := uuid.UUID(row.LinkedID.Bytes)
		mn.LinkedID = &id
	}
	if row.LastContentHash.Valid {
		mn.LastContentHash = &row.LastContentHash.String
	}
	if row.MeetingAt.Valid {
		mn.MeetingAt = row.MeetingAt.Time.UTC()
	}
	if row.CreatedAt.Valid {
		mn.CreatedAt = row.CreatedAt.Time.UTC()
	}
	mn.DeletedAt = pgTimestamptzToTimePtr(row.DeletedAt)
	if len(row.Participants) > 0 {
		if err := json.Unmarshal(row.Participants, &mn.Participants); err != nil {
			mn.Participants = []string{}
		}
	} else {
		mn.Participants = []string{}
	}
	return mn, nil
}

// participantsJSON encodes []string into a JSONB-compatible byte slice.
// Nil/empty slices marshal as `[]` so the column never holds NULL.
func participantsJSON(parts []string) ([]byte, error) {
	if parts == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(parts)
}

// InsertMeetingNoteTx inserts a first-insert meeting_note row inside the
// caller's tx. Caller owns the tx lifecycle.
func (r *MeetingNoteRepository) InsertMeetingNoteTx(ctx context.Context, tx pgx.Tx, params InsertMeetingNoteParams) (*MeetingNote, error) {
	partsJSON, err := participantsJSON(params.Participants)
	if err != nil {
		return nil, err
	}
	row, err := db.New(tx).InsertMeetingNote(ctx, db.InsertMeetingNoteParams{
		AnarlogSessionID: uuidToPgUUID(params.AnarlogSessionID),
		Title:            stringToPgText(params.Title),
		Summary:          stringToPgText(params.Summary),
		Memo:             stringToPgText(params.Memo),
		Participants:     partsJSON,
		MacHostID:        nullableUUIDToPg(params.MacHostID),
		LinkedKind:       stringToPgText(params.LinkedKind),
		LinkedID:         nullableUUIDToPg(params.LinkedID),
		LinkageState:     params.LinkageState,
		InputHash:        params.InputHash,
		ResolvedSetHash:  params.ResolvedSetHash,
		LastContentHash:  stringToPgText(params.LastContentHash),
		MeetingAt:        pgtype.Timestamptz{Time: params.MeetingAt, Valid: true},
	})
	if err != nil {
		return nil, err
	}
	return convertDbMeetingNote(row)
}

// UpdateMeetingNoteOnResyncTx updates a live meeting_note row with the
// new content + linkage values. Used by both the carry-forward and
// re-link branches (the caller picks the values).
func (r *MeetingNoteRepository) UpdateMeetingNoteOnResyncTx(ctx context.Context, tx pgx.Tx, params UpdateMeetingNoteOnResyncParams) (*MeetingNote, error) {
	partsJSON, err := participantsJSON(params.Participants)
	if err != nil {
		return nil, err
	}
	row, err := db.New(tx).UpdateMeetingNoteOnResync(ctx, db.UpdateMeetingNoteOnResyncParams{
		ID:              uuidToPgUUID(params.ID),
		Title:           stringToPgText(params.Title),
		Summary:         stringToPgText(params.Summary),
		Memo:            stringToPgText(params.Memo),
		Participants:    partsJSON,
		LinkedKind:      stringToPgText(params.LinkedKind),
		LinkedID:        nullableUUIDToPg(params.LinkedID),
		LinkageState:    params.LinkageState,
		InputHash:       params.InputHash,
		ResolvedSetHash: params.ResolvedSetHash,
		LastContentHash: stringToPgText(params.LastContentHash),
		MeetingAt:       pgtype.Timestamptz{Time: params.MeetingAt, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	return convertDbMeetingNote(row)
}

// ReviveMeetingNoteTx clears deleted_at and writes the new content +
// linkage values in a single statement. Idempotent across concurrent
// revive races via the defensive WHERE deleted_at IS NOT NULL.
func (r *MeetingNoteRepository) ReviveMeetingNoteTx(ctx context.Context, tx pgx.Tx, params ReviveMeetingNoteParams) (*MeetingNote, error) {
	partsJSON, err := participantsJSON(params.Participants)
	if err != nil {
		return nil, err
	}
	row, err := db.New(tx).ReviveMeetingNote(ctx, db.ReviveMeetingNoteParams{
		ID:              uuidToPgUUID(params.ID),
		Title:           stringToPgText(params.Title),
		Summary:         stringToPgText(params.Summary),
		Memo:            stringToPgText(params.Memo),
		Participants:    partsJSON,
		LinkedKind:      stringToPgText(params.LinkedKind),
		LinkedID:        nullableUUIDToPg(params.LinkedID),
		LinkageState:    params.LinkageState,
		InputHash:       params.InputHash,
		ResolvedSetHash: params.ResolvedSetHash,
		LastContentHash: stringToPgText(params.LastContentHash),
		MeetingAt:       pgtype.Timestamptz{Time: params.MeetingAt, Valid: true},
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	return convertDbMeetingNote(row)
}

// GetMeetingNoteBySessionIDTx returns the live meeting_note row for a
// given anarlog session UUID. Returns (nil, db.ErrNotFound) when no
// live row exists. Live-only — tombstoned rows are NOT returned.
func (r *MeetingNoteRepository) GetMeetingNoteBySessionIDTx(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) (*MeetingNote, error) {
	row, err := db.New(tx).GetMeetingNoteBySessionID(ctx, uuidToPgUUID(sessionID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	return convertDbMeetingNote(row)
}

// GetMeetingNoteBySessionIDForUpdateTx returns the meeting_note row
// (including tombstoned) for a given session UUID and acquires a
// row-level lock for the caller's tx. Used by the inline handler to
// serialize concurrent re-syncs for the same session UUID.
func (r *MeetingNoteRepository) GetMeetingNoteBySessionIDForUpdateTx(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) (*MeetingNote, error) {
	row, err := db.New(tx).GetMeetingNoteBySessionIDForUpdate(ctx, uuidToPgUUID(sessionID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	return convertDbMeetingNote(row)
}

// GetTombstonedMeetingNoteBySessionIDTx returns the tombstoned row for
// a given session UUID, or (nil, db.ErrNotFound) when none exists.
// Used by the revive-bypass probe in the dispatch loop.
func (r *MeetingNoteRepository) GetTombstonedMeetingNoteBySessionIDTx(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) (*MeetingNote, error) {
	row, err := db.New(tx).GetTombstonedMeetingNoteBySessionID(ctx, uuidToPgUUID(sessionID))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	return convertDbMeetingNote(row)
}

// SoftDeleteMeetingNoteBySessionIDTx tombstones the live row for a
// given session UUID. Idempotent (no-op when no live row exists).
func (r *MeetingNoteRepository) SoftDeleteMeetingNoteBySessionIDTx(ctx context.Context, tx pgx.Tx, sessionID uuid.UUID) error {
	return db.New(tx).SoftDeleteMeetingNoteBySessionID(ctx, uuidToPgUUID(sessionID))
}

// ListKnownMeetingNoteSessionIDsByHost returns (source_id,
// last_content_hash) for every live meeting_note row owned by the
// given mac_host. Powers the anarlog_sessions arm of /known-ids.
// Returns the SAME KnownExternalContactID DTO shape so the service
// layer can dispatch uniformly across sources.
func (r *MeetingNoteRepository) ListKnownMeetingNoteSessionIDsByHost(ctx context.Context, hostID uuid.UUID) ([]KnownExternalContactID, error) {
	rows, err := r.queries.ListKnownMeetingNoteIDsByHost(ctx, uuidToPgUUID(hostID))
	if err != nil {
		return nil, err
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

// TestHardDeleteBySessionIDPrefix is a test-only cleanup helper that
// hard-deletes meeting_note rows whose session UUID (as text) starts
// with the given prefix. Bypasses the tombstone contract — production
// code must NOT call this.
func (r *MeetingNoteRepository) TestHardDeleteBySessionIDPrefix(ctx context.Context, prefix string) error {
	return r.queries.TestHardDeleteMeetingNotesBySessionIDPrefix(ctx, prefix)
}

// nullableUUIDToPg converts a *uuid.UUID into a pgtype.UUID with
// Valid=false when the pointer is nil.
func nullableUUIDToPg(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{Valid: false}
	}
	return pgtype.UUID{Bytes: *id, Valid: true}
}
