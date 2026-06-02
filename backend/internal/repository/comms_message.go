package repository

import (
	"context"
	"errors"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// CommsMessage is the in-memory representation of a comms_message row — the
// shared cross-source content store (Gmail integration phase 1; gchat/telegram/
// messages migrate onto it later). One row = one message x one qualifying
// contact (per-participant granularity). SourceMetadata carries raw JSON bytes
// end-to-end; callers marshal/unmarshal it.
type CommsMessage struct {
	ID                uuid.UUID
	Source            string
	ExternalID        string
	ThreadID          *string
	Subject           *string
	Body              *string
	Snippet           *string
	PeerHandle        *string
	PeerNormalized    *string
	Direction         string
	SentAt            time.Time
	AccountID         *string
	SourceMetadata    []byte
	MatchedContactID  uuid.UUID
	InteractionID     *uuid.UUID
	ClaimedAt         *time.Time
	ClaimedSessionRef *string
	ProcessedAt       *time.Time
	DeletedAt         *time.Time
	CreatedAt         time.Time
}

// UpsertCommsMessageParams is the input for UpsertMessage. Content fields map
// 1:1 to the columns; GmailMessageID is a non-column text param used only in
// the provenance-merge DO UPDATE (it keys the per-account gmail-id map). When
// AccountID is nil the merge files the gmail id under the '__unknown__' key.
type UpsertCommsMessageParams struct {
	Source           string
	ExternalID       string
	ThreadID         *string
	Subject          *string
	Body             *string
	Snippet          *string
	PeerHandle       *string
	PeerNormalized   *string
	Direction        string
	SentAt           time.Time
	AccountID        *string
	SourceMetadata   []byte
	MatchedContactID uuid.UUID
	GmailMessageID   *string
}

// EmailIdentity is a (normalized email, contact) pair from
// ListEmailIdentitiesForSync. The mapping is many-to-one: a shared address maps
// to several contacts, one EmailIdentity per pair.
type EmailIdentity struct {
	ValueNormalized string
	ContactID       uuid.UUID
}

// CommsMessageRepository wraps the sqlc-generated comms_message queries.
type CommsMessageRepository struct {
	queries db.Querier
}

// NewCommsMessageRepository creates a new comms_message repository.
func NewCommsMessageRepository(queries db.Querier) *CommsMessageRepository {
	return &CommsMessageRepository{queries: queries}
}

func convertDbCommsMessage(m *db.CommsMessage) CommsMessage {
	msg := CommsMessage{
		Source:         m.Source,
		ExternalID:     m.ExternalID,
		Direction:      m.Direction,
		SourceMetadata: m.SourceMetadata,
	}
	if m.ID.Valid {
		msg.ID = uuid.UUID(m.ID.Bytes)
	}
	if m.ThreadID.Valid {
		msg.ThreadID = &m.ThreadID.String
	}
	if m.Subject.Valid {
		msg.Subject = &m.Subject.String
	}
	if m.Body.Valid {
		msg.Body = &m.Body.String
	}
	if m.Snippet.Valid {
		msg.Snippet = &m.Snippet.String
	}
	if m.PeerHandle.Valid {
		msg.PeerHandle = &m.PeerHandle.String
	}
	if m.PeerNormalized.Valid {
		msg.PeerNormalized = &m.PeerNormalized.String
	}
	if m.SentAt.Valid {
		msg.SentAt = m.SentAt.Time
	}
	if m.AccountID.Valid {
		msg.AccountID = &m.AccountID.String
	}
	if m.MatchedContactID.Valid {
		msg.MatchedContactID = uuid.UUID(m.MatchedContactID.Bytes)
	}
	if m.InteractionID.Valid {
		id := uuid.UUID(m.InteractionID.Bytes)
		msg.InteractionID = &id
	}
	if m.ClaimedAt.Valid {
		msg.ClaimedAt = &m.ClaimedAt.Time
	}
	if m.ClaimedSessionRef.Valid {
		msg.ClaimedSessionRef = &m.ClaimedSessionRef.String
	}
	if m.ProcessedAt.Valid {
		msg.ProcessedAt = &m.ProcessedAt.Time
	}
	if m.DeletedAt.Valid {
		msg.DeletedAt = &m.DeletedAt.Time
	}
	if m.CreatedAt.Valid {
		msg.CreatedAt = m.CreatedAt.Time
	}
	return msg
}

// buildUpsertCommsMessageParams centralizes the pgtype conversion shared
// between the tx and non-tx upsert paths, so both variants stay in lockstep.
// GmailMessageID is a plain text param in the generated query (non-nullable
// cast); a nil pointer maps to the empty string.
func buildUpsertCommsMessageParams(params UpsertCommsMessageParams) db.UpsertCommsMessageParams {
	gmailMessageID := ""
	if params.GmailMessageID != nil {
		gmailMessageID = *params.GmailMessageID
	}
	return db.UpsertCommsMessageParams{
		Source:           params.Source,
		ExternalID:       params.ExternalID,
		ThreadID:         stringToPgText(params.ThreadID),
		Subject:          stringToPgText(params.Subject),
		Body:             stringToPgText(params.Body),
		Snippet:          stringToPgText(params.Snippet),
		PeerHandle:       stringToPgText(params.PeerHandle),
		PeerNormalized:   stringToPgText(params.PeerNormalized),
		Direction:        params.Direction,
		SentAt:           timeToPgTimestamptz(&params.SentAt),
		AccountID:        stringToPgText(params.AccountID),
		SourceMetadata:   params.SourceMetadata,
		MatchedContactID: uuidToPgUUID(params.MatchedContactID),
		GmailMessageID:   gmailMessageID,
	}
}

// UpsertMessage inserts a content row or, on the partial-unique conflict,
// merges provenance by set-union (content fields are immutable on conflict;
// first writer wins). See the query comment for the merge semantics.
func (r *CommsMessageRepository) UpsertMessage(ctx context.Context, params UpsertCommsMessageParams) (*CommsMessage, error) {
	dbMsg, err := r.queries.UpsertCommsMessage(ctx, buildUpsertCommsMessageParams(params))
	if err != nil {
		return nil, err
	}
	msg := convertDbCommsMessage(dbMsg)
	return &msg, nil
}

// UpsertMessageTx is the tx-bound variant of UpsertMessage. The provider
// (phase 2) uses it for publish-before-mutate ordering so the content write
// commits atomically with the event-log insert. Caller owns the tx lifecycle.
func (r *CommsMessageRepository) UpsertMessageTx(ctx context.Context, tx pgx.Tx, params UpsertCommsMessageParams) (*CommsMessage, error) {
	dbMsg, err := db.New(tx).UpsertCommsMessage(ctx, buildUpsertCommsMessageParams(params))
	if err != nil {
		return nil, err
	}
	msg := convertDbCommsMessage(dbMsg)
	return &msg, nil
}

// GetMessage retrieves a content row by its natural key (source, external_id,
// contact). Returns db.ErrNotFound on miss.
func (r *CommsMessageRepository) GetMessage(ctx context.Context, source, externalID string, contactID uuid.UUID) (*CommsMessage, error) {
	dbMsg, err := r.queries.GetCommsMessage(ctx, db.GetCommsMessageParams{
		Source:           source,
		ExternalID:       externalID,
		MatchedContactID: uuidToPgUUID(contactID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	msg := convertDbCommsMessage(dbMsg)
	return &msg, nil
}

// GetByID retrieves a content row by id. Returns db.ErrNotFound on miss.
func (r *CommsMessageRepository) GetByID(ctx context.Context, id uuid.UUID) (*CommsMessage, error) {
	dbMsg, err := r.queries.GetCommsMessageByID(ctx, uuidToPgUUID(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	msg := convertDbCommsMessage(dbMsg)
	return &msg, nil
}

// ListByContact returns a contact's content rows, newest first.
func (r *CommsMessageRepository) ListByContact(ctx context.Context, contactID uuid.UUID) ([]CommsMessage, error) {
	dbMsgs, err := r.queries.ListCommsMessagesByContact(ctx, uuidToPgUUID(contactID))
	if err != nil {
		return nil, err
	}
	msgs := make([]CommsMessage, len(dbMsgs))
	for i, m := range dbMsgs {
		msgs[i] = convertDbCommsMessage(m)
	}
	return msgs, nil
}

// MarkProcessedTx links content rows to the aggregated interaction and sets
// processed_at, returning the number of rows actually updated. This is the
// exact StagingProcessor signature. sessionRef is part of the shared interface
// but intentionally unused here: email aggregation uses a deterministic
// source_ref and never claims rows, so there is no boundary-shift race for it
// to defend against. Empty messageIDs short-circuits to (0, nil).
func (r *CommsMessageRepository) MarkProcessedTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, interactionID uuid.UUID, sessionRef string) (int64, error) {
	_ = sessionRef // accepted for interface compatibility; unused in the email predicate
	if len(messageIDs) == 0 {
		return 0, nil
	}
	pgIDs := make([]pgtype.UUID, len(messageIDs))
	for i, id := range messageIDs {
		pgIDs[i] = uuidToPgUUID(id)
	}
	return db.New(tx).MarkCommsMessagesProcessed(ctx, db.MarkCommsMessagesProcessedParams{
		InteractionID: uuidToPgUUID(interactionID),
		MessageIds:    pgIDs,
	})
}

// ListEmailIdentitiesForSync returns every (normalized email, contact) pair for
// non-deleted contacts. The mapping is many-to-one: a shared address maps to
// several contacts, one pair each (spec §3.1). The Gmail provider (phase 2)
// builds its known-contact map from this.
func (r *CommsMessageRepository) ListEmailIdentitiesForSync(ctx context.Context) ([]EmailIdentity, error) {
	rows, err := r.queries.ListEmailIdentitiesForSync(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]EmailIdentity, 0, len(rows))
	for _, row := range rows {
		if !row.ContactID.Valid {
			continue
		}
		out = append(out, EmailIdentity{
			ValueNormalized: row.ValueNormalized,
			ContactID:       uuid.UUID(row.ContactID.Bytes),
		})
	}
	return out, nil
}

// HardDeleteByContact is a test-only helper that hard-deletes comms_message
// rows by matched_contact_id. Used by integration tests for per-run cleanup;
// soft-delete is unsafe because the upsert does not clear deleted_at on
// conflict, so soft-deleted rows would resurrect across runs. Production code
// MUST NOT call this.
func (r *CommsMessageRepository) HardDeleteByContact(ctx context.Context, contactID uuid.UUID) error {
	return r.queries.HardDeleteCommsMessagesByContact(ctx, uuidToPgUUID(contactID))
}

// CommsStagingProcessor adapts *CommsMessageRepository to the source-neutral
// StagingProcessor interface. Registered into the StagingProcessorRegistry in
// phase 5 (main.go wiring) — not here, because no email consumer dispatches to
// it until phase 3.
type CommsStagingProcessor struct{ repo *CommsMessageRepository }

// NewCommsStagingProcessor builds the email-source staging processor adapter.
func NewCommsStagingProcessor(repo *CommsMessageRepository) *CommsStagingProcessor {
	return &CommsStagingProcessor{repo: repo}
}

// MarkProcessedTx implements StagingProcessor.
func (p *CommsStagingProcessor) MarkProcessedTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, interactionID uuid.UUID, sessionRef string) (int64, error) {
	return p.repo.MarkProcessedTx(ctx, tx, messageIDs, interactionID, sessionRef)
}
