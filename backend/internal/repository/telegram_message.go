package repository

import (
	"context"
	"errors"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TelegramMessage represents a stored Telegram message.
//
// ClaimedAt / ClaimedSessionRef carry the row's claim state (spec §3
// Race Mechanics). Set by the aggregator's create path when the row is
// included in a session about to be published; cleared by
// InteractionRecorder when Stage 3 commits OR by ClearStaleClaim when
// the engine detects a stranded claim.
type TelegramMessage struct {
	ID                uuid.UUID
	TelegramMessageID int32
	TelegramChatID    int64
	ChatType          string
	ChatTitle         *string
	MessageText       *string
	MessageType       string
	SentAt            time.Time
	EditedAt          *time.Time
	IsOutgoing        bool
	ReplyToMsgID      *int32
	PeerUserID        *int64
	PeerUsername      *string
	PeerFirstName     *string
	PeerLastName      *string
	PeerPhone         *string
	MatchedContactID  *uuid.UUID
	InteractionID     *uuid.UUID
	ProcessedAt       *time.Time
	ClaimedAt         *time.Time
	ClaimedSessionRef *string
	DeletedAt         *time.Time
	CreatedAt         time.Time
}

// UpsertTelegramMessageParams holds parameters for upserting a message.
type UpsertTelegramMessageParams struct {
	TelegramMessageID  int32
	TelegramChatID     int64
	ChatType           string
	ChatTitle          *string
	MessageText        *string
	MessageType        string
	SentAt             time.Time
	EditedAt           *time.Time
	IsOutgoing         bool
	ReplyToMsgID       *int32
	PeerUserID         *int64
	PeerUsername       *string
	PeerFirstName      *string
	PeerLastName       *string
	PeerPhone          *string
	PeerEntityResolved bool
}

// TelegramMessageRepository handles telegram message persistence.
type TelegramMessageRepository struct {
	queries db.Querier
}

// NewTelegramMessageRepository creates a new telegram message repository.
func NewTelegramMessageRepository(queries db.Querier) *TelegramMessageRepository {
	return &TelegramMessageRepository{queries: queries}
}

func convertDbTelegramMessage(m *db.TelegramMessage) TelegramMessage {
	msg := TelegramMessage{
		TelegramMessageID: m.TelegramMessageID,
		TelegramChatID:    m.TelegramChatID,
		ChatType:          m.ChatType,
		MessageType:       m.MessageType,
		IsOutgoing:        m.IsOutgoing,
	}
	msg.ID = m.ID
	msg.ChatTitle = m.ChatTitle
	msg.MessageText = m.MessageText
	msg.SentAt = m.SentAt
	msg.EditedAt = m.EditedAt
	msg.ReplyToMsgID = m.ReplyToMsgID
	msg.PeerUserID = m.PeerUserID
	msg.PeerUsername = m.PeerUsername
	msg.PeerFirstName = m.PeerFirstName
	msg.PeerLastName = m.PeerLastName
	msg.PeerPhone = m.PeerPhone
	msg.MatchedContactID = m.MatchedContactID
	msg.InteractionID = m.InteractionID
	msg.ProcessedAt = m.ProcessedAt
	msg.ClaimedAt = m.ClaimedAt
	msg.ClaimedSessionRef = m.ClaimedSessionRef
	msg.DeletedAt = m.DeletedAt
	if m.CreatedAt != nil {
		msg.CreatedAt = *m.CreatedAt
	}
	return msg
}

// UpsertMessage creates or updates a telegram message.
func (r *TelegramMessageRepository) UpsertMessage(ctx context.Context, params UpsertTelegramMessageParams) (*TelegramMessage, error) {
	dbMsg, err := r.queries.UpsertTelegramMessage(ctx, db.UpsertTelegramMessageParams{
		TelegramMessageID:  params.TelegramMessageID,
		TelegramChatID:     params.TelegramChatID,
		ChatType:           params.ChatType,
		ChatTitle:          params.ChatTitle,
		MessageText:        params.MessageText,
		MessageType:        params.MessageType,
		SentAt:             params.SentAt,
		EditedAt:           params.EditedAt,
		IsOutgoing:         params.IsOutgoing,
		ReplyToMsgID:       params.ReplyToMsgID,
		PeerUserID:         params.PeerUserID,
		PeerUsername:       params.PeerUsername,
		PeerFirstName:      params.PeerFirstName,
		PeerLastName:       params.PeerLastName,
		PeerPhone:          params.PeerPhone,
		PeerEntityResolved: params.PeerEntityResolved,
	})
	if err != nil {
		return nil, err
	}
	msg := convertDbTelegramMessage(dbMsg)
	return &msg, nil
}

// GetMessage retrieves a message by chat ID and message ID.
func (r *TelegramMessageRepository) GetMessage(ctx context.Context, chatID int64, messageID int32) (*TelegramMessage, error) {
	dbMsg, err := r.queries.GetTelegramMessage(ctx, db.GetTelegramMessageParams{
		TelegramChatID:    chatID,
		TelegramMessageID: messageID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	msg := convertDbTelegramMessage(dbMsg)
	return &msg, nil
}

// SoftDeleteMessages soft-deletes messages by message IDs (no chat filter).
func (r *TelegramMessageRepository) SoftDeleteMessages(ctx context.Context, messageIDs []int32) error {
	return r.queries.SoftDeleteTelegramMessages(ctx, messageIDs)
}

// SoftDeleteChannelMessages soft-deletes messages by chat ID and message IDs.
func (r *TelegramMessageRepository) SoftDeleteChannelMessages(ctx context.Context, chatID int64, messageIDs []int32) error {
	return r.queries.SoftDeleteTelegramChannelMessages(ctx, db.SoftDeleteTelegramChannelMessagesParams{
		TelegramChatID: chatID,
		MessageIds:     messageIDs,
	})
}

// ListUnprocessedByChat returns unprocessed messages for a chat.
func (r *TelegramMessageRepository) ListUnprocessedByChat(ctx context.Context, chatID int64) ([]TelegramMessage, error) {
	dbMsgs, err := r.queries.ListTelegramMessagesByChatUnprocessed(ctx, chatID)
	if err != nil {
		return nil, err
	}
	msgs := make([]TelegramMessage, len(dbMsgs))
	for i, m := range dbMsgs {
		msgs[i] = convertDbTelegramMessage(m)
	}
	return msgs, nil
}

// CountByChat returns message counts grouped by chat ID.
func (r *TelegramMessageRepository) CountByChat(ctx context.Context) (map[int64]int64, error) {
	rows, err := r.queries.CountTelegramMessagesByChat(ctx)
	if err != nil {
		return nil, err
	}
	counts := make(map[int64]int64, len(rows))
	for _, row := range rows {
		counts[row.TelegramChatID] = row.MessageCount
	}
	return counts, nil
}

// PeerEntity captures the entity fields associated with a Telegram peer,
// extracted from the best historical telegram_message row for that peer.
// Used by the live-message handler to backfill sparse entity data that the
// gotd/td dispatcher omits from incoming updates. Blank strings from storage
// are promoted to nil here to match the matcher's nilIfEmpty convention.
type PeerEntity struct {
	PeerUserID    int64
	PeerUsername  *string
	PeerFirstName *string
	PeerLastName  *string
	PeerPhone     *string
}

// UnmatchedPeer holds distinct peer info for identity matching.
type UnmatchedPeer struct {
	PeerUserID    int64
	PeerUsername  *string
	PeerFirstName *string
	PeerLastName  *string
	PeerPhone     *string
}

// PeerMessageCount holds message counts for a peer.
type PeerMessageCount struct {
	PeerUserID    int64
	TotalCount    int64
	OutboundCount int64
	InboundCount  int64
	LastMessageAt time.Time
}

// ListUnprocessedByContactAndChat returns unprocessed messages for a contact+chat.
func (r *TelegramMessageRepository) ListUnprocessedByContactAndChat(ctx context.Context, contactID uuid.UUID, chatID int64) ([]TelegramMessage, error) {
	dbMsgs, err := r.queries.ListUnprocessedTelegramMessagesByContactAndChat(ctx, db.ListUnprocessedTelegramMessagesByContactAndChatParams{
		MatchedContactID: &contactID,
		TelegramChatID:   chatID,
	})
	if err != nil {
		return nil, err
	}
	msgs := make([]TelegramMessage, len(dbMsgs))
	for i, m := range dbMsgs {
		msgs[i] = convertDbTelegramMessage(m)
	}
	return msgs, nil
}

// ListUnprocessedByContact returns all unprocessed messages for a contact.
func (r *TelegramMessageRepository) ListUnprocessedByContact(ctx context.Context, contactID uuid.UUID) ([]TelegramMessage, error) {
	dbMsgs, err := r.queries.ListUnprocessedTelegramMessagesByContact(ctx, &contactID)
	if err != nil {
		return nil, err
	}
	msgs := make([]TelegramMessage, len(dbMsgs))
	for i, m := range dbMsgs {
		msgs[i] = convertDbTelegramMessage(m)
	}
	return msgs, nil
}

// ListDistinctUnmatchedPeers returns distinct peer info for unmatched messages.
func (r *TelegramMessageRepository) ListDistinctUnmatchedPeers(ctx context.Context) ([]UnmatchedPeer, error) {
	rows, err := r.queries.ListDistinctUnmatchedPeers(ctx)
	if err != nil {
		return nil, err
	}
	peers := make([]UnmatchedPeer, len(rows))
	for i, row := range rows {
		peers[i] = UnmatchedPeer{
			PeerUserID:    deref(row.PeerUserID),
			PeerUsername:  row.PeerUsername,
			PeerFirstName: row.PeerFirstName,
			PeerLastName:  row.PeerLastName,
			PeerPhone:     row.PeerPhone,
		}
	}
	return peers, nil
}

// UpdateMessageContact sets matched_contact_id on all messages for a peer.
func (r *TelegramMessageRepository) UpdateMessageContact(ctx context.Context, peerUserID int64, contactID uuid.UUID) error {
	return r.queries.UpdateTelegramMessageContact(ctx, db.UpdateTelegramMessageContactParams{
		MatchedContactID: &contactID,
		PeerUserID:       &peerUserID,
	})
}

// MarkMessagesProcessed sets processed_at + interaction_id AND clears
// claim columns. Non-tx variant; used by the engine's extend/promote/
// bridge paths only (those paths do not claim rows or publish events,
// so no session-scope predicate is needed).
func (r *TelegramMessageRepository) MarkMessagesProcessed(ctx context.Context, messageIDs []uuid.UUID, interactionID uuid.UUID) error {
	return r.queries.MarkTelegramMessagesProcessed(ctx, db.MarkTelegramMessagesProcessedParams{
		InteractionID: &interactionID,
		MessageIds:    messageIDs,
	})
}

// MarkMessagesProcessedTx is the tx-bound variant. Used by
// InteractionRecorder.HandleEvent so the telegram_message.interaction_id
// FK write shares the same tx as the interaction insert (spec §3.4.1
// atomicity contract).
//
// The SQL predicate scopes the update to rows whose
// claimed_session_ref matches sessionRef OR is NULL — defending
// against the stale boundary-shift race (claimed_session_ref =
// 'other-ref' rejects the update) while still working when the engine
// took the non-tx publish path (NULL claimed_session_ref).
//
// Returns the number of rows actually updated. The caller (consumer)
// distinguishes the cases: zero affected on a fresh write means the
// predicate filtered everything out (boundary-shift race) and the
// consumer rolls back the whole tx to prevent a phantom-duplicate
// interaction from committing; zero affected on a replay is expected
// (rows were already linked to the existing interaction on the
// original attempt).
func (r *TelegramMessageRepository) MarkMessagesProcessedTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, interactionID uuid.UUID, sessionRef string) (int64, error) {
	if len(messageIDs) == 0 {
		return 0, nil
	}
	return db.New(tx).MarkTelegramMessagesProcessedForSession(ctx, db.MarkTelegramMessagesProcessedForSessionParams{
		InteractionID: &interactionID,
		MessageIds:    messageIDs,
		SessionRef:    &sessionRef,
	})
}

// ClaimMessages writes claim columns on rows still eligible. Non-tx
// variant; used by tests / batch scripts. Returns the IDs actually
// claimed (RETURNING id).
func (r *TelegramMessageRepository) ClaimMessages(ctx context.Context, messageIDs []uuid.UUID, sessionRef string) ([]uuid.UUID, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	return r.queries.ClaimTelegramMessages(ctx, db.ClaimTelegramMessagesParams{
		SessionRef: &sessionRef,
		MessageIds: messageIDs,
	})
}

// ClaimMessagesTx is the tx-bound variant of ClaimMessages. Used by the
// aggregator engine's create path. Returns IDs actually claimed; caller
// compares against requested-IDs to detect partial-claim races.
func (r *TelegramMessageRepository) ClaimMessagesTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, sessionRef string) ([]uuid.UUID, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}
	return db.New(tx).ClaimTelegramMessages(ctx, db.ClaimTelegramMessagesParams{
		SessionRef: &sessionRef,
		MessageIds: messageIDs,
	})
}

// ClearStaleClaimTx clears claim columns for rows still carrying the
// expected stale session_ref. Used by the engine's defensive recovery
// branch when FindEventBySource returned no row for the claimed session.
func (r *TelegramMessageRepository) ClearStaleClaimTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, expectedSessionRef string) error {
	if len(messageIDs) == 0 {
		return nil
	}
	return db.New(tx).ClearTelegramMessageStaleClaim(ctx, db.ClearTelegramMessageStaleClaimParams{
		MessageIds:         messageIDs,
		ExpectedSessionRef: &expectedSessionRef,
	})
}

// BackdateClaim is a test-only helper that ages the claim_at on the
// given rows past the 5-minute TTL. Production code MUST NOT call this.
func (r *TelegramMessageRepository) BackdateClaim(ctx context.Context, messageIDs []uuid.UUID) error {
	if len(messageIDs) == 0 {
		return nil
	}
	return r.queries.BackdateTelegramMessageClaim(ctx, messageIDs)
}

// TelegramStagingProcessor adapts *TelegramMessageRepository to the
// source-neutral StagingProcessor interface. Concrete instance is
// created in main.go and passed to the registry.
type TelegramStagingProcessor struct{ repo *TelegramMessageRepository }

// NewTelegramStagingProcessor builds the telegram-source staging
// processor adapter.
func NewTelegramStagingProcessor(repo *TelegramMessageRepository) *TelegramStagingProcessor {
	return &TelegramStagingProcessor{repo: repo}
}

// MarkProcessedTx implements StagingProcessor.
func (p *TelegramStagingProcessor) MarkProcessedTx(ctx context.Context, tx pgx.Tx, messageIDs []uuid.UUID, interactionID uuid.UUID, sessionRef string) (int64, error) {
	return p.repo.MarkMessagesProcessedTx(ctx, tx, messageIDs, interactionID, sessionRef)
}

// ListUnprocessedContactIDs returns distinct contact IDs with unprocessed messages.
func (r *TelegramMessageRepository) ListUnprocessedContactIDs(ctx context.Context) ([]uuid.UUID, error) {
	pgIDs, err := r.queries.ListUnprocessedContactIDs(ctx)
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

// CountMessagesByPeer returns message counts grouped by peer.
func (r *TelegramMessageRepository) CountMessagesByPeer(ctx context.Context) ([]PeerMessageCount, error) {
	rows, err := r.queries.CountTelegramMessagesByPeer(ctx)
	if err != nil {
		return nil, err
	}
	counts := make([]PeerMessageCount, len(rows))
	for i, row := range rows {
		counts[i] = PeerMessageCount{
			PeerUserID:    deref(row.PeerUserID),
			TotalCount:    row.TotalCount,
			OutboundCount: row.OutboundCount,
			InboundCount:  row.InboundCount,
		}
		if t, ok := row.LastMessageAt.(time.Time); ok {
			counts[i].LastMessageAt = t
		}
	}
	return counts, nil
}

// FindDistinctUnmatchedPeerUserIDsByUsername returns one row per peer_user_id
// whose unmatched messages carry the given Telegram handle (case-insensitive).
// Used by the rematch service when a "telegram" contact_method is added.
func (r *TelegramMessageRepository) FindDistinctUnmatchedPeerUserIDsByUsername(ctx context.Context, username string) ([]UnmatchedPeer, error) {
	rows, err := r.queries.FindDistinctUnmatchedPeerUserIDsByUsername(ctx, username)
	if err != nil {
		return nil, err
	}
	peers := make([]UnmatchedPeer, 0, len(rows))
	for _, row := range rows {
		peers = append(peers, UnmatchedPeer{
			PeerUserID:   deref(row.PeerUserID),
			PeerUsername: row.PeerUsername,
		})
	}
	return peers, nil
}

// FindDistinctUnmatchedPeerUserIDsByPhone returns one row per peer_user_id
// whose unmatched messages carry the given phone (compared digits-only).
func (r *TelegramMessageRepository) FindDistinctUnmatchedPeerUserIDsByPhone(ctx context.Context, phone string) ([]UnmatchedPeer, error) {
	rows, err := r.queries.FindDistinctUnmatchedPeerUserIDsByPhone(ctx, phone)
	if err != nil {
		return nil, err
	}
	peers := make([]UnmatchedPeer, 0, len(rows))
	for _, row := range rows {
		peers = append(peers, UnmatchedPeer{
			PeerUserID:   deref(row.PeerUserID),
			PeerUsername: row.PeerUsername,
		})
	}
	return peers, nil
}

// CountUnmatchedMessagesByPeer returns the number of messages for a peer that
// are not yet linked to a contact. Read this BEFORE OnPeerLinked so the
// rematch handler reports a meaningful pre-link count.
func (r *TelegramMessageRepository) CountUnmatchedMessagesByPeer(ctx context.Context, peerUserID int64) (int64, error) {
	return r.queries.CountUnmatchedMessagesByPeer(ctx, &peerUserID)
}

// GetPeerEntityByUserID returns the best-known entity data for a Telegram
// peer, selected by preferring rows with non-blank username/phone/name over
// rows with blank fields. Returns (nil, nil) when no non-deleted messages
// exist for the peer — the caller should treat this as "no cached data".
// Blank ("") fields are promoted to nil to match the matcher's convention.
func (r *TelegramMessageRepository) GetPeerEntityByUserID(ctx context.Context, peerUserID int64) (*PeerEntity, error) {
	row, err := r.queries.GetPeerEntityByUserID(ctx, &peerUserID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	entity := &PeerEntity{PeerUserID: peerUserID}
	if row.PeerUsername != nil && *row.PeerUsername != "" {
		v := *row.PeerUsername
		entity.PeerUsername = &v
	}
	if row.PeerFirstName != nil && *row.PeerFirstName != "" {
		v := *row.PeerFirstName
		entity.PeerFirstName = &v
	}
	if row.PeerLastName != nil && *row.PeerLastName != "" {
		v := *row.PeerLastName
		entity.PeerLastName = &v
	}
	if row.PeerPhone != nil && *row.PeerPhone != "" {
		v := *row.PeerPhone
		entity.PeerPhone = &v
	}
	return entity, nil
}

// CountMessagesByPeerID returns message counts for a single peer.
func (r *TelegramMessageRepository) CountMessagesByPeerID(ctx context.Context, peerUserID int64) (*PeerMessageCount, error) {
	row, err := r.queries.CountTelegramMessagesByPeerID(ctx, &peerUserID)
	if err != nil {
		return nil, err
	}
	count := &PeerMessageCount{
		PeerUserID:    peerUserID,
		TotalCount:    row.TotalCount,
		OutboundCount: row.OutboundCount,
		InboundCount:  row.InboundCount,
	}
	if t, ok := row.LastMessageAt.(time.Time); ok {
		count.LastMessageAt = t
	}
	return count, nil
}

// HardDeleteByChatIDRange is a test-only helper that hard-deletes
// telegram_message rows whose telegram_chat_id falls in [lo, hi].
// Integration tests use this for per-run cleanup; soft-delete is unsafe
// because UpsertTelegramMessage does not clear deleted_at on conflict,
// so soft-deleted rows would resurrect as phantoms on subsequent runs.
func (r *TelegramMessageRepository) HardDeleteByChatIDRange(ctx context.Context, lo, hi int64) error {
	return r.queries.HardDeleteTelegramMessagesByChatIDRange(ctx, db.HardDeleteTelegramMessagesByChatIDRangeParams{
		Lo: lo,
		Hi: hi,
	})
}
