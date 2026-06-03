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

// CommsMessage is the in-memory representation of a comms_message row — the
// shared cross-source content store (email uses it now; gchat/telegram/messages
// migrate onto it later). One row = one message x one qualifying contact
// (per-participant granularity). SourceMetadata carries raw JSON bytes
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
// 1:1 to the columns. AccountID + GmailMessageID drive the provenance merge on
// BOTH the insert and the conflict paths: AccountID is unioned into
// source_metadata.observed_accounts[] and GmailMessageID is filed under
// source_metadata.account_gmail_ids.<account_id>. When AccountID is nil the
// gmail id is filed under the '__unknown__' key. Any non-provenance keys the
// caller puts in SourceMetadata (html body, attachments[], labels) are
// preserved.
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
// cast); a nil pointer maps to the empty string. SourceMetadata defaults to an
// empty JSON object when nil/empty: the query casts it with `::jsonb` and folds
// the observing account's provenance into it, so it must always be valid JSON.
func buildUpsertCommsMessageParams(params UpsertCommsMessageParams) db.UpsertCommsMessageParams {
	gmailMessageID := ""
	if params.GmailMessageID != nil {
		gmailMessageID = *params.GmailMessageID
	}
	sourceMetadata := params.SourceMetadata
	if len(sourceMetadata) == 0 {
		sourceMetadata = []byte("{}")
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
		SourceMetadata:   sourceMetadata,
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

// UpsertMessageTx is the tx-bound variant of UpsertMessage. The provider uses
// it for publish-before-mutate ordering so the content write commits atomically
// with the event-log insert. Caller owns the tx lifecycle.
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

// GetMessageTx is the tx-bound variant of GetMessage. The email-interaction
// consumer uses it so the content-row read shares the worker's tx — the
// whole read → branch → write → mark sequence runs on one consistent
// snapshot (the "one tx" contract). Reuses the GetCommsMessage query via
// db.New(tx); no new SQL. Returns db.ErrNotFound on miss.
func (r *CommsMessageRepository) GetMessageTx(ctx context.Context, tx pgx.Tx, source, externalID string, contactID uuid.UUID) (*CommsMessage, error) {
	dbMsg, err := db.New(tx).GetCommsMessage(ctx, db.GetCommsMessageParams{
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
// several contacts, one pair each (spec §3.1). The Gmail provider builds its
// known-contact map from this.
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

// CommsMessageParticipantRow is one recent email content row projected for the
// correspondence-enrichment scan: the qualifying contact (the co-occurring
// contact), when it was sent, and the raw source_metadata JSON (carrying the
// from/to/cc/bcc participant lists + their display names). The producer
// unmarshals SourceMetadata itself.
type CommsMessageParticipantRow struct {
	MatchedContactID uuid.UUID
	SentAt           time.Time
	SourceMetadata   []byte
}

// ListParticipantsSince streams recent email content rows (sent_at >= since)
// for the correspondence-enrichment producer. Newest first; bounded by since.
func (r *CommsMessageRepository) ListParticipantsSince(ctx context.Context, since time.Time) ([]CommsMessageParticipantRow, error) {
	rows, err := r.queries.ListCommsMessageParticipantsSince(ctx, timeToPgTimestamptz(&since))
	if err != nil {
		return nil, err
	}
	out := make([]CommsMessageParticipantRow, 0, len(rows))
	for _, row := range rows {
		pr := CommsMessageParticipantRow{SourceMetadata: row.SourceMetadata}
		if row.MatchedContactID.Valid {
			pr.MatchedContactID = uuid.UUID(row.MatchedContactID.Bytes)
		}
		if row.SentAt.Valid {
			pr.SentAt = row.SentAt.Time
		}
		out = append(out, pr)
	}
	return out, nil
}

// MissingParticipantNamesRow is one keyset-paged row the historical
// display-name re-derivation must re-fetch: the row id (and keyset cursor),
// the connected account that observed it, and the stored source_metadata
// (carrying account_gmail_ids to resolve the per-mailbox gmail id).
type MissingParticipantNamesRow struct {
	ID             uuid.UUID
	AccountID      *string
	SourceMetadata []byte
}

// ListMissingParticipantNames keyset-pages email rows (sent_at >= since,
// id > afterID) that lack the from_name key, in id order, capped at batchSize.
// The runner advances afterID = max(id) of each returned batch regardless of
// per-row outcome, so a skipped/failed row never blocks later rows.
func (r *CommsMessageRepository) ListMissingParticipantNames(ctx context.Context, since time.Time, afterID uuid.UUID, batchSize int32) ([]MissingParticipantNamesRow, error) {
	rows, err := r.queries.ListCommsMessagesMissingParticipantNames(ctx, db.ListCommsMessagesMissingParticipantNamesParams{
		Since:     timeToPgTimestamptz(&since),
		AfterID:   uuidToPgUUID(afterID),
		BatchSize: batchSize,
	})
	if err != nil {
		return nil, err
	}
	out := make([]MissingParticipantNamesRow, 0, len(rows))
	for _, row := range rows {
		mr := MissingParticipantNamesRow{SourceMetadata: row.SourceMetadata}
		if row.ID.Valid {
			mr.ID = uuid.UUID(row.ID.Bytes)
		}
		if row.AccountID.Valid {
			mr.AccountID = &row.AccountID.String
		}
		out = append(out, mr)
	}
	return out, nil
}

// ParticipantNames is the set of re-derived display names for one message,
// index-aligned with the row's stored from/to/cc/bcc address lists.
type ParticipantNames struct {
	FromName string
	ToNames  []string
	CcNames  []string
	BccNames []string
}

// BackfillParticipantNames additively merges the re-derived display names onto
// an existing row's source_metadata, preserving all existing content +
// provenance keys. The *_names slices are marshalled to non-NULL JSON arrays
// ([] when empty) so the SQL jsonb_set never writes a JSON null. Returns the
// number of rows affected (0 when the row already has names — idempotent).
func (r *CommsMessageRepository) BackfillParticipantNames(ctx context.Context, id uuid.UUID, names ParticipantNames) (int64, error) {
	toJSON := func(s []string) []byte {
		if s == nil {
			s = []string{}
		}
		b, err := json.Marshal(s)
		if err != nil {
			return []byte("[]")
		}
		return b
	}
	return r.queries.BackfillCommsMessageParticipantNames(ctx, db.BackfillCommsMessageParticipantNamesParams{
		ID:       uuidToPgUUID(id),
		FromName: names.FromName,
		ToNames:  toJSON(names.ToNames),
		CcNames:  toJSON(names.CcNames),
		BccNames: toJSON(names.BccNames),
	})
}

// CommsStagingProcessor adapts *CommsMessageRepository to the source-neutral
// StagingProcessor interface. It is wired into the StagingProcessorRegistry by
// main.go only once an email consumer exists to dispatch through it; until then
// the adapter is provided but unregistered (the registry is consulted solely by
// the interaction-recorder consumer, so registering it early would be inert).
// See .ai/spec/2026-06-01-gmail-integration-design.md §8.
type CommsStagingProcessor struct{ repo *CommsMessageRepository }

// NewCommsStagingProcessor builds the email-source staging processor adapter.
func NewCommsStagingProcessor(repo *CommsMessageRepository) *CommsStagingProcessor {
	return &CommsStagingProcessor{repo: repo}
}

// MarkProcessedTx implements StagingProcessor.
func (p *CommsStagingProcessor) MarkProcessedTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, interactionID uuid.UUID, sessionRef string) (int64, error) {
	return p.repo.MarkProcessedTx(ctx, tx, messageIDs, interactionID, sessionRef)
}
