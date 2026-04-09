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

// int32ToPgInt4 already defined in telegram_chat_config.go
