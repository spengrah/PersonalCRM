package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CommsMessage is the in-memory representation of a comms_message row — the
// shared cross-source content store (email uses it now; gchat/telegram/messages
// migrate onto it later). One row = one message x one qualifying contact
// (per-participant granularity). SourceMetadata carries raw JSON bytes
// end-to-end; callers marshal/unmarshal it.
//
// MatchedContactID is a POINTER because a chat source may stage a message
// before its peer is resolved to a contact (migration 076). nil is legal only
// for source='whatsapp' — the DB CHECK comms_message_contact_source_check
// enforces it — and such a row is invisible to every eligible/aggregation query
// until AttachUnmatchedByPeer fills it in.
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
	MatchedContactID  *uuid.UUID
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

// GChatIdentity is a (normalized address, contact, source_type) triple from
// ListGChatIdentitiesForSync. Dual-source: SourceType is "gchat" or "email"
// (GChat sender addresses ARE emails, so the provider considers both a
// dedicated gchat method and any plain email method). Many-to-one: a shared
// address maps to several contacts, one GChatIdentity per (address, contact,
// type) row.
type GChatIdentity struct {
	ValueNormalized string
	ContactID       uuid.UUID
	SourceType      string
}

// UpsertChatMessageParams is the input for UpsertChatMessage — the chat-source
// staging write. Unlike UpsertCommsMessageParams it carries no Subject and no
// GmailMessageID: chat messages have no subject, and a chat source observes
// from exactly one linked device, so there is no per-account provenance to
// merge. MatchedContactID nil means the peer is not (yet) a contact; the DB
// CHECK restricts that to source='whatsapp'.
type UpsertChatMessageParams struct {
	Source           string
	ExternalID       string
	ThreadID         *string
	Body             *string
	Snippet          *string
	PeerHandle       *string
	PeerNormalized   *string
	Direction        string
	SentAt           time.Time
	AccountID        *string
	SourceMetadata   []byte
	MatchedContactID *uuid.UUID
}

// UnmatchedPeerCount is one peer's discovery aggregate over unmatched staging
// rows. PeerNormalized is nil when the peer's phone was never resolvable (a
// LID-only peer); LastPushName is nil when no row for the peer carried one.
type UnmatchedPeerCount struct {
	PeerHandle     string
	PeerNormalized *string
	TotalCount     int64
	InboundCount   int64
	OutboundCount  int64
	LastMessageAt  time.Time
	LastPushName   *string
}

// CommsMessageRepository wraps the sqlc-generated comms_message queries.
type CommsMessageRepository struct {
	queries db.Querier
	// pool is optional; when non-nil it enables own-tx methods
	// (AttachUnmatchedByPeer, whose two statements must commit together).
	// Nil in most test constructors — those methods return an error when it
	// is unset. Same convention as ContactRepository.SetPool.
	pool *pgxpool.Pool
	// attachFailpointForTest, when non-nil, is invoked between the
	// soft-delete and attach statements of AttachUnmatchedByPeer; a non-nil
	// return aborts the transaction. Test-only hook (same convention as
	// repointBarrierForTest below) that makes the atomicity of the pair
	// observable. Nil in production.
	attachFailpointForTest func() error
	// repointBarrierForTest, when non-nil, is invoked between the dedup and
	// repoint statements of RepointContactForMergeTx's FIRST savepoint attempt
	// only. Test-only hook (same convention as
	// ContactService.InjectMergeCommitBarrierForTest) that lets a test commit
	// a colliding row from a second connection at exactly the point where the
	// READ COMMITTED race fires, making the savepoint-retry path
	// deterministic. Nil in production.
	repointBarrierForTest func()
}

// NewCommsMessageRepository creates a new comms_message repository.
func NewCommsMessageRepository(queries db.Querier) *CommsMessageRepository {
	return &CommsMessageRepository{queries: queries}
}

// commsMessageDedupIndex is the partial unique index
// (source, external_id, matched_contact_id) WHERE deleted_at IS NULL that the
// merge repoint can collide with when a concurrent ingest commits a target-
// contact row between the dedup and repoint statements.
const commsMessageDedupIndex = "idx_comms_message_dedup"

// SetRepointBarrierForTest installs the test-only barrier hook. Must only be
// called before the repository is used concurrently; no synchronization.
func (r *CommsMessageRepository) SetRepointBarrierForTest(fn func()) {
	r.repointBarrierForTest = fn
}

// SetPool injects the pgxpool used by own-tx methods (AttachUnmatchedByPeer).
// Optional; safe to leave unset for code paths that don't need them.
func (r *CommsMessageRepository) SetPool(pool *pgxpool.Pool) {
	r.pool = pool
}

// SetAttachFailpointForTest installs the test-only failpoint that runs between
// AttachUnmatchedByPeer's two statements. Must only be called before the
// repository is used concurrently; no synchronization.
func (r *CommsMessageRepository) SetAttachFailpointForTest(fn func() error) {
	r.attachFailpointForTest = fn
}

// isCommsDedupUniqueViolation reports whether err is a 23505 raised against
// idx_comms_message_dedup. Scoped narrowly so unrelated unique violations
// still propagate.
func isCommsDedupUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == pgerrcode.UniqueViolation && pgErr.ConstraintName == commsMessageDedupIndex
}

// RepointContactForMergeTx re-points the merge source contact's comms_message
// rows to the target inside the caller's tx. Two ordered steps: (1) soft-
// delete the source's live rows whose (source, external_id) the target ALSO
// has live (the email-fanout dedup collision, D-index idx_comms_message_dedup),
// then (2) re-point the remainder (incl. previously soft-deleted rows — the
// partial index ignores them).
//
// The pair runs in a savepoint (nested tx) with ONE retry scoped to a 23505
// on idx_comms_message_dedup: at READ COMMITTED, a concurrent ingest can
// commit a target-contact row for the same (source, external_id) between the
// two statements, making the repoint collide even though the dedup statement
// saw nothing. The savepoint confines the abort, and the retry's dedup
// statement takes a fresh snapshot that sees the raced row. A second
// collision (sustained concurrent inserts) propagates and fails the merge
// safely — full rollback, user retries.
func (r *CommsMessageRepository) RepointContactForMergeTx(ctx context.Context, tx pgx.Tx, sourceContactID, targetContactID uuid.UUID) error {
	attempt := func(withBarrier bool) error {
		sp, err := tx.Begin(ctx)
		if err != nil {
			return fmt.Errorf("open comms repoint savepoint: %w", err)
		}
		q := db.New(sp)
		if err := q.SoftDeleteDuplicateCommsMessagesForMerge(ctx, db.SoftDeleteDuplicateCommsMessagesForMergeParams{
			SourceContactID: uuidToPgUUID(sourceContactID),
			TargetContactID: uuidToPgUUID(targetContactID),
		}); err != nil {
			if rbErr := sp.Rollback(ctx); rbErr != nil {
				return fmt.Errorf("rollback comms repoint savepoint: %w (dedup: %w)", rbErr, err)
			}
			return fmt.Errorf("soft-delete duplicate comms messages: %w", err)
		}
		if withBarrier && r.repointBarrierForTest != nil {
			r.repointBarrierForTest()
		}
		if err := q.RepointCommsMessageContact(ctx, db.RepointCommsMessageContactParams{
			SourceContactID: uuidToPgUUID(sourceContactID),
			TargetContactID: uuidToPgUUID(targetContactID),
		}); err != nil {
			if rbErr := sp.Rollback(ctx); rbErr != nil {
				return fmt.Errorf("rollback comms repoint savepoint: %w (repoint: %w)", rbErr, err)
			}
			return fmt.Errorf("repoint comms messages: %w", err)
		}
		if err := sp.Commit(ctx); err != nil {
			return fmt.Errorf("commit comms repoint savepoint: %w", err)
		}
		return nil
	}

	err := attempt(true)
	if err == nil {
		return nil
	}
	if !isCommsDedupUniqueViolation(err) {
		return err
	}
	// Raced insert committed between the dedup and repoint statements; the
	// savepoint rollback confined the abort. Retry once — the fresh dedup
	// snapshot sees the raced row and tombstones the collision first.
	if retryErr := attempt(false); retryErr != nil {
		return fmt.Errorf("comms repoint retry after dedup collision: %w", retryErr)
	}
	return nil
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
		id := uuid.UUID(m.MatchedContactID.Bytes)
		msg.MatchedContactID = &id
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

// ListGChatIdentitiesForSync returns every (normalized address, contact,
// source_type) triple for non-deleted contacts whose contact_method type is
// 'gchat' or 'email'. The mapping is many-to-one (shared address → multiple
// contacts). The GChat provider builds its dual-source known-contact map from
// this. Rows with an invalid contact_id are skipped defensively.
func (r *CommsMessageRepository) ListGChatIdentitiesForSync(ctx context.Context) ([]GChatIdentity, error) {
	rows, err := r.queries.ListGChatIdentitiesForSync(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]GChatIdentity, 0, len(rows))
	for _, row := range rows {
		if !row.ContactID.Valid {
			continue
		}
		out = append(out, GChatIdentity{
			ValueNormalized: row.ValueNormalized,
			ContactID:       uuid.UUID(row.ContactID.Bytes),
			SourceType:      row.SourceType,
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

// =====================================================================
// Source-parameterized aggregation-engine methods. These back the GChat
// (and future Telegram/Messages) MessageStore adapter, which pins a
// `source` and erases the ForSource suffix. The comms_message table is
// multi-source, so the source is an explicit param here (unlike the
// single-source messages_message / telegram_message repos). Reuse the
// conversions.go helpers (uuidToPgUUID, stringToPgText, etc.).
// =====================================================================

// ListUnprocessedContactIDsForSource returns distinct contact IDs with at
// least one eligible (unprocessed AND unclaimed-or-stale) row for the source.
func (r *CommsMessageRepository) ListUnprocessedContactIDsForSource(ctx context.Context, source string) ([]uuid.UUID, error) {
	pgIDs, err := r.queries.ListUnprocessedCommsContactIDs(ctx, source)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(pgIDs))
	for _, pgID := range pgIDs {
		if pgID.Valid {
			ids = append(ids, uuid.UUID(pgID.Bytes))
		}
	}
	return ids, nil
}

// ListUnprocessedByContactForSource returns eligible rows for a contact within
// the source.
func (r *CommsMessageRepository) ListUnprocessedByContactForSource(ctx context.Context, source string, contactID uuid.UUID) ([]CommsMessage, error) {
	dbMsgs, err := r.queries.ListUnprocessedCommsByContact(ctx, db.ListUnprocessedCommsByContactParams{
		Source:           source,
		MatchedContactID: uuidToPgUUID(contactID),
	})
	if err != nil {
		return nil, err
	}
	msgs := make([]CommsMessage, len(dbMsgs))
	for i, m := range dbMsgs {
		msgs[i] = convertDbCommsMessage(m)
	}
	return msgs, nil
}

// ListUnprocessedByContactAndChatForSource returns eligible rows for a
// (contact, chat) pair within the source. chatID is stored in thread_id.
func (r *CommsMessageRepository) ListUnprocessedByContactAndChatForSource(ctx context.Context, source string, contactID uuid.UUID, chatID string) ([]CommsMessage, error) {
	dbMsgs, err := r.queries.ListUnprocessedCommsByContactAndChat(ctx, db.ListUnprocessedCommsByContactAndChatParams{
		Source:           source,
		MatchedContactID: uuidToPgUUID(contactID),
		ThreadID:         pgtype.Text{String: chatID, Valid: true},
	})
	if err != nil {
		return nil, err
	}
	msgs := make([]CommsMessage, len(dbMsgs))
	for i, m := range dbMsgs {
		msgs[i] = convertDbCommsMessage(m)
	}
	return msgs, nil
}

// ListUnprocessedChatsByContactForSource returns the distinct chat scopes
// (thread_id values) for which the contact has at least one eligible row
// within the source. Drives the messaging aggregator worker's per-chat loop.
// thread_id is nullable on comms_message; NULL values are filtered out
// defensively (chat sources always write it non-null).
func (r *CommsMessageRepository) ListUnprocessedChatsByContactForSource(ctx context.Context, source string, contactID uuid.UUID) ([]string, error) {
	rows, err := r.queries.ListUnprocessedCommsChatsByContact(ctx, db.ListUnprocessedCommsChatsByContactParams{
		Source:           source,
		MatchedContactID: uuidToPgUUID(contactID),
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, t := range rows {
		if t.Valid && t.String != "" {
			out = append(out, t.String)
		}
	}
	return out, nil
}

// GetMessageByReplyTargetForSource resolves the row a reply points at, scoped
// to a (source, contact, chat) triple, by the target message's own external_id.
// Scoping to contactID is load-bearing: comms_message is per-contact, so an
// unscoped lookup could return a different contact's fanned-out row. Returns
// db.ErrNotFound on miss.
func (r *CommsMessageRepository) GetMessageByReplyTargetForSource(ctx context.Context, source string, contactID uuid.UUID, chatID, replyTargetID string) (*CommsMessage, error) {
	dbMsg, err := r.queries.GetCommsMessageByReplyTarget(ctx, db.GetCommsMessageByReplyTargetParams{
		Source:           source,
		MatchedContactID: uuidToPgUUID(contactID),
		ThreadID:         pgtype.Text{String: chatID, Valid: true},
		ReplyTargetID:    replyTargetID,
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

// MarkMessagesProcessed sets processed_at + interaction_id + clears claim
// columns. Non-tx variant; used by the engine's extend/promote/bridge paths
// only (those paths do not claim rows or publish events). No source param: id =
// ANY(...) is PK-scoped and the engine lists/processes per-source. Empty
// messageIDs short-circuits to nil.
func (r *CommsMessageRepository) MarkMessagesProcessed(ctx context.Context, messageIDs []uuid.UUID, interactionID uuid.UUID) error {
	if len(messageIDs) == 0 {
		return nil
	}
	pgIDs := make([]pgtype.UUID, len(messageIDs))
	for i, id := range messageIDs {
		pgIDs[i] = uuidToPgUUID(id)
	}
	_, err := r.queries.MarkCommsMessagesProcessed(ctx, db.MarkCommsMessagesProcessedParams{
		InteractionID: uuidToPgUUID(interactionID),
		MessageIds:    pgIDs,
	})
	return err
}

// MarkProcessedForSessionTx is the tx-bound, session-scoped mark-processed for
// the chat create-path. The SQL predicate scopes the update to rows whose
// claimed_session_ref matches sessionRef OR is NULL — defending against the
// stale boundary-shift race while still working when the engine took the non-tx
// publish path (NULL claimed_session_ref). Returns rows actually updated so the
// recorder can detect the zero-rows boundary-shift case. Empty messageIDs
// short-circuits to (0, nil).
func (r *CommsMessageRepository) MarkProcessedForSessionTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, interactionID uuid.UUID, sessionRef string) (int64, error) {
	if len(messageIDs) == 0 {
		return 0, nil
	}
	pgIDs := make([]pgtype.UUID, len(messageIDs))
	for i, id := range messageIDs {
		pgIDs[i] = uuidToPgUUID(id)
	}
	return db.New(tx).MarkCommsMessagesProcessedForSession(ctx, db.MarkCommsMessagesProcessedForSessionParams{
		InteractionID: uuidToPgUUID(interactionID),
		MessageIds:    pgIDs,
		SessionRef:    pgtype.Text{String: sessionRef, Valid: true},
	})
}

// ClaimMessagesTx writes claim columns on rows still eligible. Used by the
// aggregator engine's create-path tx. Returns the IDs actually claimed (via
// RETURNING id); the caller compares against the requested set to detect
// partial-claim races. Empty messageIDs short-circuits to nil.
func (r *CommsMessageRepository) ClaimMessagesTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, sessionRef string) ([]uuid.UUID, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	pgIDs := make([]pgtype.UUID, len(messageIDs))
	for i, id := range messageIDs {
		pgIDs[i] = uuidToPgUUID(id)
	}
	claimed, err := db.New(tx).ClaimCommsMessages(ctx, db.ClaimCommsMessagesParams{
		SessionRef: pgtype.Text{String: sessionRef, Valid: true},
		MessageIds: pgIDs,
	})
	if err != nil {
		return nil, err
	}
	out := make([]uuid.UUID, 0, len(claimed))
	for _, id := range claimed {
		if id.Valid {
			out = append(out, uuid.UUID(id.Bytes))
		}
	}
	return out, nil
}

// ClearStaleClaimTx clears claim columns for rows still carrying the expected
// stale session_ref. Used by the engine's defensive recovery branch when
// FindEventBySource returned no row for the claimed session. Empty messageIDs
// short-circuits to nil.
func (r *CommsMessageRepository) ClearStaleClaimTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, expectedSessionRef string) error {
	if len(messageIDs) == 0 {
		return nil
	}
	pgIDs := make([]pgtype.UUID, len(messageIDs))
	for i, id := range messageIDs {
		pgIDs[i] = uuidToPgUUID(id)
	}
	return db.New(tx).ClearStaleCommsClaim(ctx, db.ClearStaleCommsClaimParams{
		MessageIds:         pgIDs,
		ExpectedSessionRef: pgtype.Text{String: expectedSessionRef, Valid: true},
	})
}

// BackdateClaim is a test-only helper that ages the claim on the given rows
// past the 5-minute TTL so a fresh aggregate pass can re-claim them.
// Production code MUST NOT call this.
func (r *CommsMessageRepository) BackdateClaim(ctx context.Context, messageIDs []uuid.UUID) error {
	if len(messageIDs) == 0 {
		return nil
	}
	pgIDs := make([]pgtype.UUID, len(messageIDs))
	for i, id := range messageIDs {
		pgIDs[i] = uuidToPgUUID(id)
	}
	return r.queries.BackdateCommsMessageClaim(ctx, pgIDs)
}

// SoftDeleteByID is a test-only helper that soft-deletes a single
// comms_message row by id (simulating an upstream provider delete). Used by the
// delete-no-op aggregation test. There is no production chat delete path yet.
func (r *CommsMessageRepository) SoftDeleteByID(ctx context.Context, id uuid.UUID) error {
	return r.queries.SoftDeleteCommsMessageByID(ctx, uuidToPgUUID(id))
}

// ApplyEditByExternalID applies an upstream edit to every stored row for
// (source, external_id): updates body + snippet, pushes the prior body onto
// source_metadata.previous_bodies[] (capped at the last 3), and records
// last_update_time + edited_at. The SQL ::timestamptz recency guard decides
// whether the edit is newer than the stored row, so this is idempotent under
// concurrent same-edit observation. body/snippet are pointers (nil → SQL NULL);
// editedAt and lastUpdateTime are RFC-3339 strings (lastUpdateTime is the
// message's LastUpdateTime, cast ::timestamptz in the query). Returns the
// number of rows updated (0 when the stored last_update_time is already >= the
// new one). Production code MUST NOT compare lastUpdateTime in Go.
func (r *CommsMessageRepository) ApplyEditByExternalID(ctx context.Context, source, externalID string, body, snippet *string, editedAt, lastUpdateTime string) (int64, error) {
	return r.queries.ApplyCommsMessageEditByExternalID(ctx, db.ApplyCommsMessageEditByExternalIDParams{
		Source:         source,
		ExternalID:     externalID,
		Body:           stringToPgText(body),
		Snippet:        stringToPgText(snippet),
		EditedAt:       editedAt,
		LastUpdateTime: lastUpdateTime,
	})
}

// SoftDeleteByExternalID soft-deletes every stored row for (source,
// external_id) — the production chat delete path. Idempotent: an already-deleted
// message affects 0 rows. Returns the number of rows soft-deleted.
func (r *CommsMessageRepository) SoftDeleteByExternalID(ctx context.Context, source, externalID string, now time.Time) (int64, error) {
	return r.queries.SoftDeleteCommsMessagesByExternalID(ctx, db.SoftDeleteCommsMessagesByExternalIDParams{
		Source:     source,
		ExternalID: externalID,
		Now:        timeToPgTimestamptz(&now),
	})
}

// GetLatestByExternalID reads one stored row for (source, external_id), newest
// first, to supply the current body for the provider's bodyDiffers
// no-op-avoidance pre-check. Returns db.ErrNotFound on miss. The returned row's
// last_update_time MUST NOT be string-compared in Go to decide recency — that
// decision belongs to the SQL ::timestamptz guard in ApplyEditByExternalID.
func (r *CommsMessageRepository) GetLatestByExternalID(ctx context.Context, source, externalID string) (*CommsMessage, error) {
	dbMsg, err := r.queries.GetCommsMessageLatestByExternalID(ctx, db.GetCommsMessageLatestByExternalIDParams{
		Source:     source,
		ExternalID: externalID,
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

// CommsSessionStagingProcessor adapts *CommsMessageRepository to the
// source-neutral StagingProcessor interface for CHAT sources (gchat). It uses
// the SESSION-scoped, claim-clearing MarkProcessedForSessionTx — unlike the
// email CommsStagingProcessor, which uses the non-session MarkProcessedTx.
// The chat create-path claims rows and publishes events; the recorder's
// zero-rows-affected rollback depends on the session predicate, so reusing the
// email processor here would break the boundary-shift defense.
type CommsSessionStagingProcessor struct{ repo *CommsMessageRepository }

// NewCommsSessionStagingProcessor builds the chat-source staging processor.
func NewCommsSessionStagingProcessor(repo *CommsMessageRepository) *CommsSessionStagingProcessor {
	return &CommsSessionStagingProcessor{repo: repo}
}

// MarkProcessedTx implements StagingProcessor (session-scoped).
func (p *CommsSessionStagingProcessor) MarkProcessedTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, interactionID uuid.UUID, sessionRef string) (int64, error) {
	return p.repo.MarkProcessedForSessionTx(ctx, tx, messageIDs, interactionID, sessionRef)
}

// =====================================================================
// Chat-source staging writes that tolerate an unresolved peer (076).
// Callers never see the matched/unmatched query split — Postgres cannot
// switch an ON CONFLICT target at runtime, so the two cases need two
// queries against two different unique indexes, and UpsertChatMessage
// picks by the nil-ness of the contact.
// =====================================================================

// UpsertChatMessage stages one chat message. Content is IMMUTABLE on conflict
// (first writer wins), which is what makes the live ingest path and the
// link-time history backfill idempotent against each other. A nil
// MatchedContactID stages the row unmatched; the DB CHECK
// comms_message_contact_source_check rejects that for any source but whatsapp.
func (r *CommsMessageRepository) UpsertChatMessage(ctx context.Context, params UpsertChatMessageParams) (*CommsMessage, error) {
	sourceMetadata := jsonbOrEmpty(params.SourceMetadata)
	if params.MatchedContactID == nil {
		dbMsg, err := r.queries.UpsertChatCommsMessageUnmatched(ctx, db.UpsertChatCommsMessageUnmatchedParams{
			Source:         params.Source,
			ExternalID:     params.ExternalID,
			ThreadID:       stringToPgText(params.ThreadID),
			Body:           stringToPgText(params.Body),
			Snippet:        stringToPgText(params.Snippet),
			PeerHandle:     stringToPgText(params.PeerHandle),
			PeerNormalized: stringToPgText(params.PeerNormalized),
			Direction:      params.Direction,
			SentAt:         timeToPgTimestamptz(&params.SentAt),
			AccountID:      stringToPgText(params.AccountID),
			SourceMetadata: sourceMetadata,
		})
		if err != nil {
			return nil, err
		}
		msg := convertDbCommsMessage(dbMsg)
		return &msg, nil
	}
	dbMsg, err := r.queries.UpsertChatCommsMessageMatched(ctx, db.UpsertChatCommsMessageMatchedParams{
		Source:           params.Source,
		ExternalID:       params.ExternalID,
		ThreadID:         stringToPgText(params.ThreadID),
		Body:             stringToPgText(params.Body),
		Snippet:          stringToPgText(params.Snippet),
		PeerHandle:       stringToPgText(params.PeerHandle),
		PeerNormalized:   stringToPgText(params.PeerNormalized),
		Direction:        params.Direction,
		SentAt:           timeToPgTimestamptz(&params.SentAt),
		AccountID:        stringToPgText(params.AccountID),
		SourceMetadata:   sourceMetadata,
		MatchedContactID: uuidPtrToPgUUID(params.MatchedContactID),
	})
	if err != nil {
		return nil, err
	}
	msg := convertDbCommsMessage(dbMsg)
	return &msg, nil
}

// AttachUnmatchedByPeer binds a source's unmatched rows for one peer to a
// contact, in ONE transaction:
//
//  1. soft-delete the unmatched rows whose (source, external_id) is ALREADY
//     staged against contactID — attaching them would violate
//     idx_comms_message_dedup and abort the caller's transaction, and skipping
//     them would strand a permanent unmatched duplicate. The chat upsert is
//     content-immutable, so the surviving matched row carries identical content.
//  2. attach everything that remains.
//
// Returns (attached, softDeletedDuplicates). Both selectors nil is a no-op
// (0, 0, nil) rather than a query — matching on nothing would be a full-source
// attach. Requires SetPool.
func (r *CommsMessageRepository) AttachUnmatchedByPeer(ctx context.Context, source string, peerNormalized, peerHandle *string, contactID uuid.UUID) (int64, int64, error) {
	if peerNormalized == nil && peerHandle == nil {
		return 0, 0, nil
	}
	if r.pool == nil {
		return 0, 0, errors.New("attach unmatched comms messages: repository has no pool (call SetPool)")
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("begin attach tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := db.New(tx)
	deduped, err := q.SoftDeleteDuplicateUnmatchedCommsMessages(ctx, db.SoftDeleteDuplicateUnmatchedCommsMessagesParams{
		Source:         source,
		PeerHandle:     stringToPgText(peerHandle),
		PeerNormalized: stringToPgText(peerNormalized),
		ContactID:      uuidToPgUUID(contactID),
	})
	if err != nil {
		return 0, 0, fmt.Errorf("soft-delete duplicate unmatched comms messages: %w", err)
	}
	if r.attachFailpointForTest != nil {
		if err := r.attachFailpointForTest(); err != nil {
			return 0, 0, fmt.Errorf("attach failpoint: %w", err)
		}
	}
	attached, err := q.AttachUnmatchedCommsMessagesByPeer(ctx, db.AttachUnmatchedCommsMessagesByPeerParams{
		ContactID:      uuidToPgUUID(contactID),
		Source:         source,
		PeerHandle:     stringToPgText(peerHandle),
		PeerNormalized: stringToPgText(peerNormalized),
	})
	if err != nil {
		return 0, 0, fmt.Errorf("attach unmatched comms messages: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("commit attach tx: %w", err)
	}
	return attached, deduped, nil
}

// ListUnmatchedPeerCounts returns the discovery aggregate over a source's
// unmatched rows, one entry per peer with at least minMessages of them.
// peerHandle nil means "every peer"; a non-nil value narrows to one.
func (r *CommsMessageRepository) ListUnmatchedPeerCounts(ctx context.Context, source string, peerHandle *string, minMessages int) ([]UnmatchedPeerCount, error) {
	rows, err := r.queries.ListUnmatchedCommsPeerCounts(ctx, db.ListUnmatchedCommsPeerCountsParams{
		Source:      source,
		PeerHandle:  stringToPgText(peerHandle),
		MinMessages: int64(minMessages),
	})
	if err != nil {
		return nil, err
	}
	out := make([]UnmatchedPeerCount, 0, len(rows))
	for _, row := range rows {
		c := UnmatchedPeerCount{
			TotalCount:    row.TotalCount,
			InboundCount:  row.InboundCount,
			OutboundCount: row.OutboundCount,
		}
		if row.PeerHandle.Valid {
			c.PeerHandle = row.PeerHandle.String
		}
		// The query COALESCEs both optional text columns to '' so sqlc can
		// type them concretely; neither is ever legitimately empty.
		if row.PeerNormalized != "" {
			v := row.PeerNormalized
			c.PeerNormalized = &v
		}
		if row.LastPushName != "" {
			v := row.LastPushName
			c.LastPushName = &v
		}
		if row.LastMessageAt.Valid {
			c.LastMessageAt = row.LastMessageAt.Time.UTC()
		}
		out = append(out, c)
	}
	return out, nil
}

// HardDeleteBySourceAndExternalIDPrefix is a test-only helper that hard-deletes
// staging rows by (source, external_id LIKE prefix). It exists for the chat
// staging tests, whose UNMATCHED rows have no contact for
// HardDeleteByContact to scope by — and which must not leak, because 076's
// down migration refuses to revert while any NULL-contact row exists.
// Production code MUST NOT call this.
func (r *CommsMessageRepository) HardDeleteBySourceAndExternalIDPrefix(ctx context.Context, source, externalIDPrefix string) error {
	return r.queries.HardDeleteCommsMessagesBySourceAndExternalIDPrefix(ctx, db.HardDeleteCommsMessagesBySourceAndExternalIDPrefixParams{
		Source:           source,
		ExternalIDPrefix: externalIDPrefix,
	})
}
