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
)

// Linkage state constants for meeting_note.linkage_state. Match the
// CHECK constraint defined in migration 053. Kept here so the inline
// handler in service/ingest.go does not embed magic strings.
const (
	LinkageStateLinked               = "linked"
	LinkageStateLinkedImpromptu      = "linked_impromptu"
	LinkageStateOrphanTitleAugmented = "orphan_title_augmented" // reserved for the title-augmentation flow
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
//
// ConflictCandidates carries the raw JSONB snapshot of the per-candidate
// participant-overlap table recorded at the moment linkage_state was
// set to conflict_pending; nil when the column is SQL NULL. The
// repository deliberately keeps this as raw bytes so callers that don't
// inspect the snapshot pay no Marshal/Unmarshal cost; service-layer
// readers decode via json.Unmarshal into ConflictCandidateSummary on
// demand.
type MeetingNote struct {
	ID                 uuid.UUID
	AnarlogSessionID   uuid.UUID
	Title              *string
	Summary            *string
	Memo               *string
	Participants       []string
	MacHostID          *uuid.UUID
	LinkedKind         *string
	LinkedID           *uuid.UUID
	LinkageState       string
	InputHash          string
	ResolvedSetHash    string
	LastContentHash    *string
	MeetingAt          time.Time
	DeletedAt          *time.Time
	CreatedAt          time.Time
	ConflictCandidates []byte
}

// InsertMeetingNoteParams captures the per-row values the inline
// handler supplies on first-insert. Hashes default to empty string only
// at the migration boundary; the handler ALWAYS supplies real values.
//
// ConflictCandidates is the raw JSONB snapshot written when the new
// linkage_state is 'conflict_pending'; nil otherwise. Always written
// atomically with linkage_state so the resolve-link handler never sees
// a conflict_pending row with a NULL snapshot in steady state.
type InsertMeetingNoteParams struct {
	AnarlogSessionID   uuid.UUID
	Title              *string
	Summary            *string
	Memo               *string
	Participants       []string
	MacHostID          *uuid.UUID
	LinkedKind         *string
	LinkedID           *uuid.UUID
	LinkageState       string
	InputHash          string
	ResolvedSetHash    string
	LastContentHash    *string
	MeetingAt          time.Time
	ConflictCandidates []byte
}

// UpdateMeetingNoteOnResyncParams is the value bag for both the
// carry-forward and re-link branches of the re-sync algorithm. The
// caller picks the right linkage values (carry-forward preserves
// prior; re-link computes fresh).
//
// ConflictCandidates handling per branch:
//   - Carry-forward: pass prior.ConflictCandidates verbatim so a
//     pre-existing conflict_pending snapshot is preserved.
//   - Re-link, new state = conflict_pending: pass the freshly marshaled
//     snapshot bytes.
//   - Re-link, new state ≠ conflict_pending: pass nil so the snapshot
//     is cleared atomically with the state change.
type UpdateMeetingNoteOnResyncParams struct {
	ID                 uuid.UUID
	Title              *string
	Summary            *string
	Memo               *string
	Participants       []string
	LinkedKind         *string
	LinkedID           *uuid.UUID
	LinkageState       string
	InputHash          string
	ResolvedSetHash    string
	LastContentHash    *string
	MeetingAt          time.Time
	ConflictCandidates []byte
}

// ReviveMeetingNoteParams is the same shape as the on-resync update,
// but the underlying query also clears deleted_at. Used when a tombstoned
// row re-receives meeting_note.recorded with the same source_id.
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
	mn.ID = row.ID
	mn.AnarlogSessionID = row.AnarlogSessionID
	mn.Title = row.Title
	mn.Summary = row.Summary
	mn.Memo = row.Memo
	mn.MacHostID = row.MacHostID
	mn.LinkedKind = row.LinkedKind
	mn.LinkedID = row.LinkedID
	mn.LastContentHash = row.LastContentHash
	mn.MeetingAt = row.MeetingAt.UTC()
	mn.CreatedAt = row.CreatedAt.UTC()
	mn.DeletedAt = utcPtr(row.DeletedAt)
	if len(row.ConflictCandidates) > 0 {
		// Copy the bytes so the caller can hold the slice past the row's
		// scan lifetime without aliasing pgx-owned memory.
		mn.ConflictCandidates = append([]byte(nil), row.ConflictCandidates...)
	}
	if len(row.Participants) > 0 {
		if err := json.Unmarshal(row.Participants, &mn.Participants); err != nil {
			// Participants is a JSONB array we write ourselves via
			// participantsJSON; a decode failure here means the column
			// was corrupted or a non-array shape was inserted by a
			// future writer. Surface the error rather than silently
			// returning an empty list — downstream linkage logic
			// would treat a corrupted row as "no tagged participants"
			// and silently drop the user-visible tags.
			return nil, fmt.Errorf("decode meeting_note.participants for id %s: %w", mn.ID, err)
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
// caller's tx. Caller owns the tx lifecycle. Returns (nil, db.ErrNotFound)
// when a concurrent first-insert won the race against the partial unique
// index (the SQL uses ON CONFLICT DO NOTHING). Callers handle the race by
// re-reading the row with FOR UPDATE and falling through to the update path.
func (r *MeetingNoteRepository) InsertMeetingNoteTx(ctx context.Context, tx pgx.Tx, params InsertMeetingNoteParams) (*MeetingNote, error) {
	partsJSON, err := participantsJSON(params.Participants)
	if err != nil {
		return nil, err
	}
	row, err := db.New(tx).InsertMeetingNote(ctx, db.InsertMeetingNoteParams{
		AnarlogSessionID:   params.AnarlogSessionID,
		Title:              params.Title,
		Summary:            params.Summary,
		Memo:               params.Memo,
		Participants:       partsJSON,
		MacHostID:          params.MacHostID,
		LinkedKind:         params.LinkedKind,
		LinkedID:           params.LinkedID,
		LinkageState:       params.LinkageState,
		InputHash:          params.InputHash,
		ResolvedSetHash:    params.ResolvedSetHash,
		LastContentHash:    params.LastContentHash,
		MeetingAt:          params.MeetingAt,
		ConflictCandidates: params.ConflictCandidates,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
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
		ID:                 params.ID,
		Title:              params.Title,
		Summary:            params.Summary,
		Memo:               params.Memo,
		Participants:       partsJSON,
		LinkedKind:         params.LinkedKind,
		LinkedID:           params.LinkedID,
		LinkageState:       params.LinkageState,
		InputHash:          params.InputHash,
		ResolvedSetHash:    params.ResolvedSetHash,
		LastContentHash:    params.LastContentHash,
		MeetingAt:          params.MeetingAt,
		ConflictCandidates: params.ConflictCandidates,
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
		ID:                 params.ID,
		Title:              params.Title,
		Summary:            params.Summary,
		Memo:               params.Memo,
		Participants:       partsJSON,
		LinkedKind:         params.LinkedKind,
		LinkedID:           params.LinkedID,
		LinkageState:       params.LinkageState,
		InputHash:          params.InputHash,
		ResolvedSetHash:    params.ResolvedSetHash,
		LastContentHash:    params.LastContentHash,
		MeetingAt:          params.MeetingAt,
		ConflictCandidates: params.ConflictCandidates,
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
	row, err := db.New(tx).GetMeetingNoteBySessionID(ctx, sessionID)
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
	row, err := db.New(tx).GetMeetingNoteBySessionIDForUpdate(ctx, sessionID)
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
	row, err := db.New(tx).GetTombstonedMeetingNoteBySessionID(ctx, sessionID)
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
	return db.New(tx).SoftDeleteMeetingNoteBySessionID(ctx, sessionID)
}

// GetMeetingNoteByID returns a single live meeting_note row by primary
// key. Non-tx variant used by the resolve-link handler's pre-validate
// path before opening the FOR UPDATE tx, and by handler-call-path code
// that doesn't need a long-running tx.
func (r *MeetingNoteRepository) GetMeetingNoteByID(ctx context.Context, id uuid.UUID) (*MeetingNote, error) {
	row, err := r.queries.GetMeetingNoteByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	return convertDbMeetingNote(row)
}

// GetMeetingNoteByIDTx is the tx-bound variant of GetMeetingNoteByID.
func (r *MeetingNoteRepository) GetMeetingNoteByIDTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*MeetingNote, error) {
	row, err := db.New(tx).GetMeetingNoteByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	return convertDbMeetingNote(row)
}

// GetMeetingNoteByIDForUpdateTx reads a live meeting_note row by
// primary key and acquires a row-level lock for the caller's tx. Used
// by the resolve-link flow so concurrent resolve attempts on the same
// row serialize behind the first writer.
func (r *MeetingNoteRepository) GetMeetingNoteByIDForUpdateTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*MeetingNote, error) {
	row, err := db.New(tx).GetMeetingNoteByIDForUpdate(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	return convertDbMeetingNote(row)
}

// ListMeetingNotesNeedingAttention returns every live meeting_note row
// whose linkage_state is one of ('conflict_pending',
// 'orphan_needs_review'). When hostID is non-nil the query filters to
// rows owned by that mac_host. Ordered by meeting_at DESC so the
// newest entries surface first.
func (r *MeetingNoteRepository) ListMeetingNotesNeedingAttention(ctx context.Context, hostID *uuid.UUID) ([]MeetingNote, error) {
	rows, err := r.queries.ListMeetingNotesNeedingAttention(ctx, hostID)
	if err != nil {
		return nil, err
	}
	out := make([]MeetingNote, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		mn, convErr := convertDbMeetingNote(row)
		if convErr != nil {
			return nil, convErr
		}
		if mn == nil {
			continue
		}
		out = append(out, *mn)
	}
	return out, nil
}

// ResolveMeetingNoteToLinkedTx sets (linked_kind, linked_id,
// linkage_state='linked', conflict_candidates=NULL) on a row currently
// in linkage_state = 'conflict_pending'. Returns db.ErrNotFound when
// the row isn't in conflict_pending (caller maps to 409 Conflict) — the
// SQL state-guard returns zero rows in that case.
func (r *MeetingNoteRepository) ResolveMeetingNoteToLinkedTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, kind string, linkedID uuid.UUID) (*MeetingNote, error) {
	row, err := db.New(tx).ResolveMeetingNoteToLinked(ctx, db.ResolveMeetingNoteToLinkedParams{
		ID:         id,
		LinkedKind: &kind,
		LinkedID:   &linkedID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	return convertDbMeetingNote(row)
}

// ClearMeetingNoteConflictTx clears (linked_kind, linked_id,
// conflict_candidates) and sets linkage_state + resolved_set_hash to
// the caller-supplied values. State-guarded to the attention states
// (conflict_pending or orphan_needs_review) — returns db.ErrNotFound
// when the row has moved on to a terminal state (caller maps to 409).
// Despite the name, this also promotes orphan_needs_review rows (the
// "Log as impromptu" path forces linked_impromptu); the name is kept
// to avoid rippling a rename through the generated db package.
//
// The caller passes the freshly-computed resolved_set_hash so the next
// daemon-side carry-forward correctly preserves the user's decision
// when the matching inputs haven't drifted.
func (r *MeetingNoteRepository) ClearMeetingNoteConflictTx(ctx context.Context, tx pgx.Tx, id uuid.UUID, newState string, newResolvedSetHash string) (*MeetingNote, error) {
	row, err := db.New(tx).ClearMeetingNoteConflict(ctx, db.ClearMeetingNoteConflictParams{
		ID:                 id,
		NewState:           newState,
		NewResolvedSetHash: newResolvedSetHash,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	return convertDbMeetingNote(row)
}

// ListKnownMeetingNoteSessionIDsByHost returns (source_id,
// last_content_hash) for every live meeting_note row owned by the
// given mac_host. Powers the anarlog_sessions arm of /known-ids.
// Returns the SAME KnownExternalContactID DTO shape so the service
// layer can dispatch uniformly across sources.
func (r *MeetingNoteRepository) ListKnownMeetingNoteSessionIDsByHost(ctx context.Context, hostID uuid.UUID) ([]KnownExternalContactID, error) {
	rows, err := r.queries.ListKnownMeetingNoteIDsByHost(ctx, &hostID)
	if err != nil {
		return nil, err
	}
	out := make([]KnownExternalContactID, 0, len(rows))
	for _, row := range rows {
		entry := KnownExternalContactID{SourceID: row.SourceID, LastContentHash: row.LastContentHash}
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

// TestHardDeleteByHostID is a test-only cleanup helper that hard-deletes
// every meeting_note row owned by a given mac_host. Used when the test
// seeds rows with system-generated session UUIDs (no exploitable prefix).
// Production code must NOT call this.
func (r *MeetingNoteRepository) TestHardDeleteByHostID(ctx context.Context, hostID uuid.UUID) error {
	return r.queries.TestHardDeleteMeetingNotesByHostID(ctx, &hostID)
}
