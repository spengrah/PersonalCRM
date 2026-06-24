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

// TelegramChatConfig represents a Telegram chat configuration
type TelegramChatConfig struct {
	ID               uuid.UUID
	TelegramChatID   int64
	ChatTitle        *string
	ChatType         string
	MemberCount      *int32
	Status           string
	BackfillCursor   *int32
	BackfillComplete bool
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// UpsertTelegramChatConfigParams holds parameters for upserting a chat config
type UpsertTelegramChatConfigParams struct {
	TelegramChatID int64
	ChatTitle      *string
	ChatType       string
	MemberCount    *int32
	Status         string
}

// TelegramChatConfigRepository handles telegram chat config persistence
type TelegramChatConfigRepository struct {
	queries db.Querier
}

// NewTelegramChatConfigRepository creates a new telegram chat config repository
func NewTelegramChatConfigRepository(queries db.Querier) *TelegramChatConfigRepository {
	return &TelegramChatConfigRepository{queries: queries}
}

func convertDbTelegramChatConfig(c *db.TelegramChatConfig) TelegramChatConfig {
	cfg := TelegramChatConfig{
		TelegramChatID:   c.TelegramChatID,
		ChatType:         c.ChatType,
		Status:           c.Status,
		BackfillComplete: c.BackfillComplete,
	}
	if c.ID.Valid {
		cfg.ID = uuid.UUID(c.ID.Bytes)
	}
	if c.ChatTitle.Valid {
		cfg.ChatTitle = &c.ChatTitle.String
	}
	if c.MemberCount.Valid {
		cfg.MemberCount = &c.MemberCount.Int32
	}
	if c.BackfillCursor.Valid {
		cfg.BackfillCursor = &c.BackfillCursor.Int32
	}
	if c.CreatedAt.Valid {
		cfg.CreatedAt = c.CreatedAt.Time
	}
	if c.UpdatedAt.Valid {
		cfg.UpdatedAt = c.UpdatedAt.Time
	}
	return cfg
}

// GetConfig retrieves a chat config by Telegram chat ID
func (r *TelegramChatConfigRepository) GetConfig(ctx context.Context, telegramChatID int64) (*TelegramChatConfig, error) {
	dbCfg, err := r.queries.GetTelegramChatConfig(ctx, telegramChatID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	cfg := convertDbTelegramChatConfig(dbCfg)
	return &cfg, nil
}

// UpsertConfig creates or updates a chat config
func (r *TelegramChatConfigRepository) UpsertConfig(ctx context.Context, params UpsertTelegramChatConfigParams) (*TelegramChatConfig, error) {
	dbCfg, err := r.queries.UpsertTelegramChatConfig(ctx, db.UpsertTelegramChatConfigParams{
		TelegramChatID: params.TelegramChatID,
		ChatTitle:      stringToPgText(params.ChatTitle),
		ChatType:       params.ChatType,
		MemberCount:    int32ToPgInt4(params.MemberCount),
		Status:         params.Status,
	})
	if err != nil {
		return nil, err
	}
	cfg := convertDbTelegramChatConfig(dbCfg)
	return &cfg, nil
}

// UpdateStatus updates the status of a chat config
func (r *TelegramChatConfigRepository) UpdateStatus(ctx context.Context, telegramChatID int64, status string) (*TelegramChatConfig, error) {
	dbCfg, err := r.queries.UpdateTelegramChatConfigStatus(ctx, db.UpdateTelegramChatConfigStatusParams{
		Status:         status,
		TelegramChatID: telegramChatID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	cfg := convertDbTelegramChatConfig(dbCfg)
	return &cfg, nil
}

// ListConfigs retrieves all chat configs
func (r *TelegramChatConfigRepository) ListConfigs(ctx context.Context) ([]TelegramChatConfig, error) {
	dbCfgs, err := r.queries.ListTelegramChatConfigs(ctx)
	if err != nil {
		return nil, err
	}
	cfgs := make([]TelegramChatConfig, len(dbCfgs))
	for i, c := range dbCfgs {
		cfgs[i] = convertDbTelegramChatConfig(c)
	}
	return cfgs, nil
}

// UpdateBackfillCursor updates the backfill cursor for a chat.
func (r *TelegramChatConfigRepository) UpdateBackfillCursor(ctx context.Context, telegramChatID int64, cursor int32) error {
	return r.queries.UpdateTelegramChatConfigBackfillCursor(ctx, db.UpdateTelegramChatConfigBackfillCursorParams{
		BackfillCursor: pgtype.Int4{Int32: cursor, Valid: true},
		TelegramChatID: telegramChatID,
	})
}

// UpdateBackfillComplete marks a chat's backfill as complete.
func (r *TelegramChatConfigRepository) UpdateBackfillComplete(ctx context.Context, telegramChatID int64) error {
	return r.queries.UpdateTelegramChatConfigBackfillComplete(ctx, telegramChatID)
}

// ResetBackfill resets backfill state for a chat (for retroactive backfill).
func (r *TelegramChatConfigRepository) ResetBackfill(ctx context.Context, telegramChatID int64) error {
	return r.queries.ResetTelegramChatConfigBackfill(ctx, telegramChatID)
}

// ListForBackfill returns chats that need backfill (backfill_complete = false).
func (r *TelegramChatConfigRepository) ListForBackfill(ctx context.Context) ([]TelegramChatConfig, error) {
	dbCfgs, err := r.queries.ListTelegramChatConfigsForBackfill(ctx)
	if err != nil {
		return nil, err
	}
	cfgs := make([]TelegramChatConfig, len(dbCfgs))
	for i, c := range dbCfgs {
		cfgs[i] = convertDbTelegramChatConfig(c)
	}
	return cfgs, nil
}

// UpdateMemberCount updates the member count for a chat.
func (r *TelegramChatConfigRepository) UpdateMemberCount(ctx context.Context, telegramChatID int64, memberCount int32) error {
	return r.queries.UpdateTelegramChatConfigMemberCount(ctx, db.UpdateTelegramChatConfigMemberCountParams{
		MemberCount:    pgtype.Int4{Int32: memberCount, Valid: true},
		TelegramChatID: telegramChatID,
	})
}

// DeleteConfig deletes a chat config
func (r *TelegramChatConfigRepository) DeleteConfig(ctx context.Context, telegramChatID int64) error {
	return r.queries.DeleteTelegramChatConfig(ctx, telegramChatID)
}
