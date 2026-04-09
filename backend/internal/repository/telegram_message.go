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

// TelegramMessage represents a stored Telegram message.
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
	DeletedAt         *time.Time
	CreatedAt         time.Time
}

// UpsertTelegramMessageParams holds parameters for upserting a message.
type UpsertTelegramMessageParams struct {
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
	if m.ID.Valid {
		msg.ID = uuid.UUID(m.ID.Bytes)
	}
	if m.ChatTitle.Valid {
		msg.ChatTitle = &m.ChatTitle.String
	}
	if m.MessageText.Valid {
		msg.MessageText = &m.MessageText.String
	}
	if m.SentAt.Valid {
		msg.SentAt = m.SentAt.Time
	}
	if m.EditedAt.Valid {
		msg.EditedAt = &m.EditedAt.Time
	}
	if m.ReplyToMsgID.Valid {
		msg.ReplyToMsgID = &m.ReplyToMsgID.Int32
	}
	if m.PeerUserID.Valid {
		msg.PeerUserID = &m.PeerUserID.Int64
	}
	if m.PeerUsername.Valid {
		msg.PeerUsername = &m.PeerUsername.String
	}
	if m.PeerFirstName.Valid {
		msg.PeerFirstName = &m.PeerFirstName.String
	}
	if m.PeerLastName.Valid {
		msg.PeerLastName = &m.PeerLastName.String
	}
	if m.PeerPhone.Valid {
		msg.PeerPhone = &m.PeerPhone.String
	}
	if m.MatchedContactID.Valid {
		id := uuid.UUID(m.MatchedContactID.Bytes)
		msg.MatchedContactID = &id
	}
	if m.InteractionID.Valid {
		id := uuid.UUID(m.InteractionID.Bytes)
		msg.InteractionID = &id
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

// UpsertMessage creates or updates a telegram message.
func (r *TelegramMessageRepository) UpsertMessage(ctx context.Context, params UpsertTelegramMessageParams) (*TelegramMessage, error) {
	dbMsg, err := r.queries.UpsertTelegramMessage(ctx, db.UpsertTelegramMessageParams{
		TelegramMessageID: params.TelegramMessageID,
		TelegramChatID:    params.TelegramChatID,
		ChatType:          params.ChatType,
		ChatTitle:         stringToPgText(params.ChatTitle),
		MessageText:       stringToPgText(params.MessageText),
		MessageType:       params.MessageType,
		SentAt:            timeToPgTimestamptz(&params.SentAt),
		EditedAt:          timeToPgTimestamptz(params.EditedAt),
		IsOutgoing:        params.IsOutgoing,
		ReplyToMsgID:      int32ToPgInt4(params.ReplyToMsgID),
		PeerUserID:        int64ToPgInt8(params.PeerUserID),
		PeerUsername:      stringToPgText(params.PeerUsername),
		PeerFirstName:     stringToPgText(params.PeerFirstName),
		PeerLastName:      stringToPgText(params.PeerLastName),
		PeerPhone:         stringToPgText(params.PeerPhone),
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
		MatchedContactID: uuidToPgUUID(contactID),
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
	dbMsgs, err := r.queries.ListUnprocessedTelegramMessagesByContact(ctx, uuidToPgUUID(contactID))
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
		p := UnmatchedPeer{
			PeerUserID: row.PeerUserID.Int64,
		}
		if row.PeerUsername.Valid {
			p.PeerUsername = &row.PeerUsername.String
		}
		if row.PeerFirstName.Valid {
			p.PeerFirstName = &row.PeerFirstName.String
		}
		if row.PeerLastName.Valid {
			p.PeerLastName = &row.PeerLastName.String
		}
		if row.PeerPhone.Valid {
			p.PeerPhone = &row.PeerPhone.String
		}
		peers[i] = p
	}
	return peers, nil
}

// UpdateMessageContact sets matched_contact_id on all messages for a peer.
func (r *TelegramMessageRepository) UpdateMessageContact(ctx context.Context, peerUserID int64, contactID uuid.UUID) error {
	return r.queries.UpdateTelegramMessageContact(ctx, db.UpdateTelegramMessageContactParams{
		MatchedContactID: uuidToPgUUID(contactID),
		PeerUserID:       int64ToPgInt8(&peerUserID),
	})
}

// MarkMessagesProcessed sets processed_at and interaction_id on messages.
func (r *TelegramMessageRepository) MarkMessagesProcessed(ctx context.Context, messageIDs []uuid.UUID, interactionID uuid.UUID) error {
	pgIDs := make([]pgtype.UUID, len(messageIDs))
	for i, id := range messageIDs {
		pgIDs[i] = uuidToPgUUID(id)
	}
	return r.queries.MarkTelegramMessagesProcessed(ctx, db.MarkTelegramMessagesProcessedParams{
		InteractionID: uuidToPgUUID(interactionID),
		MessageIds:    pgIDs,
	})
}

// ListUnprocessedContactIDs returns distinct contact IDs with unprocessed messages.
func (r *TelegramMessageRepository) ListUnprocessedContactIDs(ctx context.Context) ([]uuid.UUID, error) {
	pgIDs, err := r.queries.ListUnprocessedContactIDs(ctx)
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

// CountMessagesByPeer returns message counts grouped by peer.
func (r *TelegramMessageRepository) CountMessagesByPeer(ctx context.Context) ([]PeerMessageCount, error) {
	rows, err := r.queries.CountTelegramMessagesByPeer(ctx)
	if err != nil {
		return nil, err
	}
	counts := make([]PeerMessageCount, len(rows))
	for i, row := range rows {
		counts[i] = PeerMessageCount{
			PeerUserID:    row.PeerUserID.Int64,
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

// GetMessageByReplyTo retrieves a message by chat ID and message ID (for reply resolution).
func (r *TelegramMessageRepository) GetMessageByReplyTo(ctx context.Context, chatID int64, messageID int32) (*TelegramMessage, error) {
	dbMsg, err := r.queries.GetTelegramMessageByReplyTo(ctx, db.GetTelegramMessageByReplyToParams{
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

// int32ToPgInt4 already defined in telegram_chat_config.go
