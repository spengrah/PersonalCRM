package repository

import (
	"context"
	"errors"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MessagesMessage is the in-memory representation of a messages_message
// staging row. Field shape parallels TelegramMessage minus telegram-
// specific peer entity fields: the messages source matches via
// canonicalized phone/email rather than platform user IDs, so there's no
// peer-entity concept.
//
// Discovery / peer-enrichment helpers (analogues of
// ListDistinctUnmatchedPeers, GetPeerEntityByUserID, etc.) are NOT
// provided: the messages source matches via canonicalized phone/email at
// the Pi ingest path, and the daemon-side filter (spec §2) guarantees
// rows reach this table only when matched_contact_id is set.
type MessagesMessage struct {
	ID                uuid.UUID
	Guid              string
	ChatGuid          string
	PeerHandle        string
	PeerNormalized    *string
	Text              *string
	MessageType       string
	SentAt            time.Time
	IsOutgoing        bool
	IsGroupChat       bool
	ReplyToGuid       *string
	MatchedContactID  *uuid.UUID
	InteractionID     *uuid.UUID
	MacHostID         *uuid.UUID
	ProcessedAt       *time.Time
	ClaimedAt         *time.Time
	ClaimedSessionRef *string
	DeletedAt         *time.Time
	CreatedAt         time.Time
}

// UpsertMessagesMessageParams is the input for UpsertMessage. Fields map
// 1:1 to the columns in the upsert query.
type UpsertMessagesMessageParams struct {
	Guid             string
	ChatGuid         string
	PeerHandle       string
	PeerNormalized   *string
	Text             *string
	MessageType      string
	SentAt           time.Time
	IsOutgoing       bool
	IsGroupChat      bool
	ReplyToGuid      *string
	MatchedContactID *uuid.UUID
	MacHostID        *uuid.UUID
}

// MessagesMessageRepository wraps the sqlc-generated messages_message queries.
type MessagesMessageRepository struct {
	queries db.Querier
}

// NewMessagesMessageRepository creates a new messages_message repository.
func NewMessagesMessageRepository(queries db.Querier) *MessagesMessageRepository {
	return &MessagesMessageRepository{queries: queries}
}

func convertDbMessagesMessage(m *db.MessagesMessage) MessagesMessage {
	msg := MessagesMessage{
		Guid:        m.Guid,
		ChatGuid:    m.ChatGuid,
		PeerHandle:  m.PeerHandle,
		MessageType: m.MessageType,
		IsOutgoing:  m.IsOutgoing,
		IsGroupChat: m.IsGroupChat,
	}
	msg.ID = m.ID
	msg.PeerNormalized = m.PeerNormalized
	msg.Text = m.Text
	msg.SentAt = m.SentAt
	msg.ReplyToGuid = m.ReplyToGuid
	msg.MatchedContactID = m.MatchedContactID
	msg.InteractionID = m.InteractionID
	msg.MacHostID = m.MacHostID
	msg.ProcessedAt = m.ProcessedAt
	msg.ClaimedAt = m.ClaimedAt
	msg.ClaimedSessionRef = m.ClaimedSessionRef
	msg.DeletedAt = m.DeletedAt
	if m.CreatedAt != nil {
		msg.CreatedAt = *m.CreatedAt
	}
	return msg
}

// UpsertMessage creates or updates a messages_message row by guid.
func (r *MessagesMessageRepository) UpsertMessage(ctx context.Context, params UpsertMessagesMessageParams) (*MessagesMessage, error) {
	dbMsg, err := r.queries.UpsertMessagesMessage(ctx, buildUpsertMessagesMessageParams(params))
	if err != nil {
		return nil, err
	}
	msg := convertDbMessagesMessage(dbMsg)
	return &msg, nil
}

// UpsertMessageTx is the tx-bound variant of UpsertMessage. Used by the
// ingest service so the staging-row write commits atomically with the
// raw event-log insert + River aggregator-job enqueue (spec §3 Stage 1
// "single tx"). Caller owns the tx lifecycle.
func (r *MessagesMessageRepository) UpsertMessageTx(ctx context.Context, tx pgx.Tx, params UpsertMessagesMessageParams) (*MessagesMessage, error) {
	dbMsg, err := db.New(tx).UpsertMessagesMessage(ctx, buildUpsertMessagesMessageParams(params))
	if err != nil {
		return nil, err
	}
	msg := convertDbMessagesMessage(dbMsg)
	return &msg, nil
}

// buildUpsertMessagesMessageParams centralizes the field mapping
// shared between the tx and non-tx upsert paths. Keeping this in one
// place ensures both variants stay in lockstep — drift would silently
// produce different on-disk shapes depending on caller.
func buildUpsertMessagesMessageParams(params UpsertMessagesMessageParams) db.UpsertMessagesMessageParams {
	return db.UpsertMessagesMessageParams{
		Guid:             params.Guid,
		ChatGuid:         params.ChatGuid,
		PeerHandle:       params.PeerHandle,
		PeerNormalized:   params.PeerNormalized,
		Text:             params.Text,
		MessageType:      params.MessageType,
		SentAt:           params.SentAt,
		IsOutgoing:       params.IsOutgoing,
		IsGroupChat:      params.IsGroupChat,
		ReplyToGuid:      params.ReplyToGuid,
		MatchedContactID: params.MatchedContactID,
		MacHostID:        params.MacHostID,
	}
}

// GetMessage retrieves a message by guid. Returns db.ErrNotFound on miss.
func (r *MessagesMessageRepository) GetMessage(ctx context.Context, guid string) (*MessagesMessage, error) {
	dbMsg, err := r.queries.GetMessagesMessage(ctx, guid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	msg := convertDbMessagesMessage(dbMsg)
	return &msg, nil
}

// GetMessageByReplyTarget retrieves a message scoped to a chat_guid.
// Returns db.ErrNotFound on miss.
func (r *MessagesMessageRepository) GetMessageByReplyTarget(ctx context.Context, chatGuid, guid string) (*MessagesMessage, error) {
	dbMsg, err := r.queries.GetMessagesMessageByReplyTarget(ctx, db.GetMessagesMessageByReplyTargetParams{
		ChatGuid: chatGuid,
		Guid:     guid,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	msg := convertDbMessagesMessage(dbMsg)
	return &msg, nil
}

// ListUnprocessedContactIDs returns distinct contact IDs with at least
// one eligible (unprocessed AND unclaimed-or-stale) row.
func (r *MessagesMessageRepository) ListUnprocessedContactIDs(ctx context.Context) ([]uuid.UUID, error) {
	pgIDs, err := r.queries.ListUnprocessedMessagesContactIDs(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0, len(pgIDs))
	for _, pgID := range pgIDs {
		if pgID != nil {
			ids = append(ids, *pgID)
		}
	}
	return ids, nil
}

// ListUnprocessedByContact returns eligible rows for a contact.
func (r *MessagesMessageRepository) ListUnprocessedByContact(ctx context.Context, contactID uuid.UUID) ([]MessagesMessage, error) {
	dbMsgs, err := r.queries.ListUnprocessedMessagesByContact(ctx, &contactID)
	if err != nil {
		return nil, err
	}
	msgs := make([]MessagesMessage, len(dbMsgs))
	for i, m := range dbMsgs {
		msgs[i] = convertDbMessagesMessage(m)
	}
	return msgs, nil
}

// ListUnprocessedChatsByContact returns the distinct chat_guid values
// for which the contact has at least one eligible (unprocessed AND
// unclaimed-or-stale) row. Drives the messaging aggregator worker's
// per-chat loop (the chat-aware AggregateForContact path is what
// preserves the extend/bridge/coalesce contract from spec §3 Stage 2).
//
// Returned values are ordered by chat_guid ASC so concurrent workers
// tend to collide on the same chat first; this improves the partial-
// claim retry path's convergence properties.
func (r *MessagesMessageRepository) ListUnprocessedChatsByContact(ctx context.Context, contactID uuid.UUID) ([]string, error) {
	return r.queries.ListUnprocessedMessagesChatsByContact(ctx, &contactID)
}

// UpdateMatchedContactParams is the input for UpdateMatchedContact.
type UpdateMatchedContactParams struct {
	ID               uuid.UUID
	MatchedContactID uuid.UUID
	PeerNormalized   string
}

// ListStranded returns staging rows whose matched_contact_id is NULL
// — i.e., rows the ingest service accepted but couldn't match to a
// contact. Used by the crm-admin --messages-rematch-stranded utility
// to retroactively match rows after a contact_method gets added.
func (r *MessagesMessageRepository) ListStranded(ctx context.Context) ([]MessagesMessage, error) {
	dbMsgs, err := r.queries.ListStrandedMessagesMessages(ctx)
	if err != nil {
		return nil, err
	}
	msgs := make([]MessagesMessage, len(dbMsgs))
	for i, m := range dbMsgs {
		msgs[i] = convertDbMessagesMessage(m)
	}
	return msgs, nil
}

// UpdateMatchedContact sets matched_contact_id + peer_normalized on a
// stranded row. The SQL predicate scopes the update to rows still
// unmatched + unprocessed; concurrent ingest of a never-stranded row
// (one whose peer just got matched) will not be overwritten by this
// admin path. Idempotent — re-running on an already-matched row is a
// no-op.
func (r *MessagesMessageRepository) UpdateMatchedContact(ctx context.Context, params UpdateMatchedContactParams) error {
	return r.queries.UpdateMatchedContactForStrandedMessage(ctx, db.UpdateMatchedContactForStrandedMessageParams{
		ID:               params.ID,
		MatchedContactID: &params.MatchedContactID,
		PeerNormalized:   &params.PeerNormalized,
	})
}

// ListUnprocessedByContactAndChat returns eligible rows for a contact +
// chat_guid pair.
func (r *MessagesMessageRepository) ListUnprocessedByContactAndChat(ctx context.Context, contactID uuid.UUID, chatGuid string) ([]MessagesMessage, error) {
	dbMsgs, err := r.queries.ListUnprocessedMessagesByContactAndChat(ctx, db.ListUnprocessedMessagesByContactAndChatParams{
		MatchedContactID: &contactID,
		ChatGuid:         chatGuid,
	})
	if err != nil {
		return nil, err
	}
	msgs := make([]MessagesMessage, len(dbMsgs))
	for i, m := range dbMsgs {
		msgs[i] = convertDbMessagesMessage(m)
	}
	return msgs, nil
}

// MarkMessagesProcessed sets processed_at + interaction_id + clears
// claim columns. Non-tx variant; used by the engine's extend/promote/
// bridge paths only (those paths do not claim rows or publish events).
func (r *MessagesMessageRepository) MarkMessagesProcessed(ctx context.Context, messageIDs []uuid.UUID, interactionID uuid.UUID) error {
	if len(messageIDs) == 0 {
		return nil
	}
	return r.queries.MarkMessagesMessagesProcessed(ctx, db.MarkMessagesMessagesProcessedParams{
		InteractionID: &interactionID,
		MessageIds:    messageIDs,
	})
}

// MarkMessagesProcessedTx is the tx-bound variant. The SQL predicate
// scopes the update to rows whose claimed_session_ref matches
// sessionRef OR is NULL — defending against the stale boundary-shift
// race (claimed_session_ref = 'other-ref' rejects the update) while
// still working when the engine took the non-tx publish path
// (NULL claimed_session_ref).
//
// Returns the number of rows actually updated so the caller can log
// a warning when zero rows matched.
func (r *MessagesMessageRepository) MarkMessagesProcessedTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, interactionID uuid.UUID, sessionRef string) (int64, error) {
	if len(messageIDs) == 0 {
		return 0, nil
	}
	return db.New(tx).MarkMessagesMessagesProcessedForSession(ctx, db.MarkMessagesMessagesProcessedForSessionParams{
		InteractionID: &interactionID,
		MessageIds:    messageIDs,
		SessionRef:    &sessionRef,
	})
}

// ClaimMessages writes claim columns on rows still eligible. Non-tx
// variant; used by tests / batch scripts. Returns the IDs actually
// claimed.
func (r *MessagesMessageRepository) ClaimMessages(ctx context.Context, messageIDs []uuid.UUID, sessionRef string) ([]uuid.UUID, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	return r.queries.ClaimMessagesMessages(ctx, db.ClaimMessagesMessagesParams{
		SessionRef: &sessionRef,
		MessageIds: messageIDs,
	})
}

// ClaimMessagesTx is the tx-bound variant of ClaimMessages. Used by the
// aggregator engine's create path. Returns the IDs actually claimed (via
// RETURNING id); caller compares against the requested set to detect
// partial-claim races.
func (r *MessagesMessageRepository) ClaimMessagesTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, sessionRef string) ([]uuid.UUID, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	return db.New(tx).ClaimMessagesMessages(ctx, db.ClaimMessagesMessagesParams{
		SessionRef: &sessionRef,
		MessageIds: messageIDs,
	})
}

// ClearStaleClaimTx clears claim columns for rows still carrying the
// expected stale session_ref. Used by the engine's defensive recovery
// branch when FindEventBySource returned no row for the claimed session.
func (r *MessagesMessageRepository) ClearStaleClaimTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, expectedSessionRef string) error {
	if len(messageIDs) == 0 {
		return nil
	}
	return db.New(tx).ClearMessagesMessageStaleClaim(ctx, db.ClearMessagesMessageStaleClaimParams{
		MessageIds:         messageIDs,
		ExpectedSessionRef: &expectedSessionRef,
	})
}

// BackdateClaim is a test-only helper that ages the claim_at on the
// given rows past the 5-minute TTL. Production code MUST NOT call this.
func (r *MessagesMessageRepository) BackdateClaim(ctx context.Context, messageIDs []uuid.UUID) error {
	if len(messageIDs) == 0 {
		return nil
	}
	return r.queries.BackdateMessagesMessageClaim(ctx, messageIDs)
}

// HardDeleteByMacHost is a test-only helper that hard-deletes
// messages_message rows by mac_host_id. Used by integration tests for
// per-run cleanup; soft-delete is unsafe because the upsert does not
// clear deleted_at on conflict.
func (r *MessagesMessageRepository) HardDeleteByMacHost(ctx context.Context, macHostID uuid.UUID) error {
	return r.queries.HardDeleteMessagesMessagesByMacHost(ctx, &macHostID)
}

// MessagesStagingProcessor adapts *MessagesMessageRepository to the
// source-neutral StagingProcessor interface. Concrete instance is
// created in main.go and passed to the registry.
type MessagesStagingProcessor struct{ repo *MessagesMessageRepository }

// NewMessagesStagingProcessor builds the messages-source staging
// processor adapter.
func NewMessagesStagingProcessor(repo *MessagesMessageRepository) *MessagesStagingProcessor {
	return &MessagesStagingProcessor{repo: repo}
}

// MarkProcessedTx implements StagingProcessor.
func (p *MessagesStagingProcessor) MarkProcessedTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, interactionID uuid.UUID, sessionRef string) (int64, error) {
	return p.repo.MarkMessagesProcessedTx(ctx, tx, messageIDs, interactionID, sessionRef)
}
