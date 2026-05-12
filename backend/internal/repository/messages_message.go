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
	if m.ID.Valid {
		msg.ID = uuid.UUID(m.ID.Bytes)
	}
	if m.PeerNormalized.Valid {
		msg.PeerNormalized = &m.PeerNormalized.String
	}
	if m.Text.Valid {
		msg.Text = &m.Text.String
	}
	if m.SentAt.Valid {
		msg.SentAt = m.SentAt.Time
	}
	if m.ReplyToGuid.Valid {
		msg.ReplyToGuid = &m.ReplyToGuid.String
	}
	if m.MatchedContactID.Valid {
		id := uuid.UUID(m.MatchedContactID.Bytes)
		msg.MatchedContactID = &id
	}
	if m.InteractionID.Valid {
		id := uuid.UUID(m.InteractionID.Bytes)
		msg.InteractionID = &id
	}
	if m.MacHostID.Valid {
		id := uuid.UUID(m.MacHostID.Bytes)
		msg.MacHostID = &id
	}
	if m.ProcessedAt.Valid {
		msg.ProcessedAt = &m.ProcessedAt.Time
	}
	if m.ClaimedAt.Valid {
		msg.ClaimedAt = &m.ClaimedAt.Time
	}
	if m.ClaimedSessionRef.Valid {
		msg.ClaimedSessionRef = &m.ClaimedSessionRef.String
	}
	if m.DeletedAt.Valid {
		msg.DeletedAt = &m.DeletedAt.Time
	}
	if m.CreatedAt.Valid {
		msg.CreatedAt = m.CreatedAt.Time
	}
	return msg
}

// UpsertMessage creates or updates a messages_message row by guid.
func (r *MessagesMessageRepository) UpsertMessage(ctx context.Context, params UpsertMessagesMessageParams) (*MessagesMessage, error) {
	var matchedContactID pgtype.UUID
	if params.MatchedContactID != nil {
		matchedContactID = uuidToPgUUID(*params.MatchedContactID)
	}
	var macHostID pgtype.UUID
	if params.MacHostID != nil {
		macHostID = uuidToPgUUID(*params.MacHostID)
	}
	dbMsg, err := r.queries.UpsertMessagesMessage(ctx, db.UpsertMessagesMessageParams{
		Guid:             params.Guid,
		ChatGuid:         params.ChatGuid,
		PeerHandle:       params.PeerHandle,
		PeerNormalized:   stringToPgText(params.PeerNormalized),
		Text:             stringToPgText(params.Text),
		MessageType:      params.MessageType,
		SentAt:           timeToPgTimestamptz(&params.SentAt),
		IsOutgoing:       params.IsOutgoing,
		IsGroupChat:      params.IsGroupChat,
		ReplyToGuid:      stringToPgText(params.ReplyToGuid),
		MatchedContactID: matchedContactID,
		MacHostID:        macHostID,
	})
	if err != nil {
		return nil, err
	}
	msg := convertDbMessagesMessage(dbMsg)
	return &msg, nil
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
		if pgID.Valid {
			ids = append(ids, uuid.UUID(pgID.Bytes))
		}
	}
	return ids, nil
}

// ListUnprocessedByContact returns eligible rows for a contact.
func (r *MessagesMessageRepository) ListUnprocessedByContact(ctx context.Context, contactID uuid.UUID) ([]MessagesMessage, error) {
	dbMsgs, err := r.queries.ListUnprocessedMessagesByContact(ctx, uuidToPgUUID(contactID))
	if err != nil {
		return nil, err
	}
	msgs := make([]MessagesMessage, len(dbMsgs))
	for i, m := range dbMsgs {
		msgs[i] = convertDbMessagesMessage(m)
	}
	return msgs, nil
}

// ListUnprocessedByContactAndChat returns eligible rows for a contact +
// chat_guid pair.
func (r *MessagesMessageRepository) ListUnprocessedByContactAndChat(ctx context.Context, contactID uuid.UUID, chatGuid string) ([]MessagesMessage, error) {
	dbMsgs, err := r.queries.ListUnprocessedMessagesByContactAndChat(ctx, db.ListUnprocessedMessagesByContactAndChatParams{
		MatchedContactID: uuidToPgUUID(contactID),
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
	pgIDs := make([]pgtype.UUID, len(messageIDs))
	for i, id := range messageIDs {
		pgIDs[i] = uuidToPgUUID(id)
	}
	return r.queries.MarkMessagesMessagesProcessed(ctx, db.MarkMessagesMessagesProcessedParams{
		InteractionID: uuidToPgUUID(interactionID),
		MessageIds:    pgIDs,
	})
}

// MarkMessagesProcessedTx is the tx-bound, session-scoped variant. The
// SQL WHERE clause includes `claimed_session_ref = sessionRef AND
// processed_at IS NULL`, so a stranded old-event consumer cannot
// overwrite rows already processed by a newer-event consumer (the
// boundary-shift race).
func (r *MessagesMessageRepository) MarkMessagesProcessedTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, interactionID uuid.UUID, sessionRef string) error {
	if len(messageIDs) == 0 {
		return nil
	}
	pgIDs := make([]pgtype.UUID, len(messageIDs))
	for i, id := range messageIDs {
		pgIDs[i] = uuidToPgUUID(id)
	}
	return db.New(tx).MarkMessagesMessagesProcessedForSession(ctx, db.MarkMessagesMessagesProcessedForSessionParams{
		InteractionID: uuidToPgUUID(interactionID),
		MessageIds:    pgIDs,
		SessionRef:    pgtype.Text{String: sessionRef, Valid: true},
	})
}

// ClaimMessages writes claim columns on rows still eligible. Non-tx
// variant; used by tests / batch scripts. Returns the IDs actually
// claimed.
func (r *MessagesMessageRepository) ClaimMessages(ctx context.Context, messageIDs []uuid.UUID, sessionRef string) ([]uuid.UUID, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	pgIDs := make([]pgtype.UUID, len(messageIDs))
	for i, id := range messageIDs {
		pgIDs[i] = uuidToPgUUID(id)
	}
	claimed, err := r.queries.ClaimMessagesMessages(ctx, db.ClaimMessagesMessagesParams{
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

// ClaimMessagesTx is the tx-bound variant of ClaimMessages. Used by the
// aggregator engine's create path. Returns the IDs actually claimed (via
// RETURNING id); caller compares against the requested set to detect
// partial-claim races.
func (r *MessagesMessageRepository) ClaimMessagesTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, sessionRef string) ([]uuid.UUID, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	pgIDs := make([]pgtype.UUID, len(messageIDs))
	for i, id := range messageIDs {
		pgIDs[i] = uuidToPgUUID(id)
	}
	claimed, err := db.New(tx).ClaimMessagesMessages(ctx, db.ClaimMessagesMessagesParams{
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

// ClearStaleClaimTx clears claim columns for rows still carrying the
// expected stale session_ref. Used by the engine's defensive recovery
// branch when FindEventBySource returned no row for the claimed session.
func (r *MessagesMessageRepository) ClearStaleClaimTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, expectedSessionRef string) error {
	if len(messageIDs) == 0 {
		return nil
	}
	pgIDs := make([]pgtype.UUID, len(messageIDs))
	for i, id := range messageIDs {
		pgIDs[i] = uuidToPgUUID(id)
	}
	return db.New(tx).ClearMessagesMessageStaleClaim(ctx, db.ClearMessagesMessageStaleClaimParams{
		MessageIds:         pgIDs,
		ExpectedSessionRef: pgtype.Text{String: expectedSessionRef, Valid: true},
	})
}

// BackdateClaim is a test-only helper that ages the claim_at on the
// given rows past the 5-minute TTL. Production code MUST NOT call this.
func (r *MessagesMessageRepository) BackdateClaim(ctx context.Context, messageIDs []uuid.UUID) error {
	if len(messageIDs) == 0 {
		return nil
	}
	pgIDs := make([]pgtype.UUID, len(messageIDs))
	for i, id := range messageIDs {
		pgIDs[i] = uuidToPgUUID(id)
	}
	return r.queries.BackdateMessagesMessageClaim(ctx, pgIDs)
}

// HardDeleteByMacHost is a test-only helper that hard-deletes
// messages_message rows by mac_host_id. Used by integration tests for
// per-run cleanup; soft-delete is unsafe because the upsert does not
// clear deleted_at on conflict.
func (r *MessagesMessageRepository) HardDeleteByMacHost(ctx context.Context, macHostID uuid.UUID) error {
	return r.queries.HardDeleteMessagesMessagesByMacHost(ctx, uuidToPgUUID(macHostID))
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
func (p *MessagesStagingProcessor) MarkProcessedTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, interactionID uuid.UUID, sessionRef string) error {
	return p.repo.MarkMessagesProcessedTx(ctx, tx, messageIDs, interactionID, sessionRef)
}
