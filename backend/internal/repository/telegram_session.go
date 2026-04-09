package repository

import (
	"context"
	"errors"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// TelegramSession represents a Telegram MTProto session
type TelegramSession struct {
	SessionDataEncrypted []byte
	EncryptionNonce      []byte
	PhoneNumber          *string
	TelegramUserID       *int64
	Username             *string
	AuthState            string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// UpsertTelegramSessionParams holds parameters for upserting a full session
type UpsertTelegramSessionParams struct {
	SessionDataEncrypted []byte
	EncryptionNonce      []byte
	PhoneNumber          *string
	TelegramUserID       *int64
	Username             *string
	AuthState            string
}

// UpsertTelegramSessionDataParams holds parameters for upserting session data only
type UpsertTelegramSessionDataParams struct {
	SessionDataEncrypted []byte
	EncryptionNonce      []byte
}

// UpdateTelegramUserInfoParams holds parameters for updating user info
type UpdateTelegramUserInfoParams struct {
	TelegramUserID *int64
	Username       *string
	PhoneNumber    *string
}

// TelegramSessionRepository handles telegram session persistence
type TelegramSessionRepository struct {
	queries db.Querier
}

// NewTelegramSessionRepository creates a new telegram session repository
func NewTelegramSessionRepository(queries db.Querier) *TelegramSessionRepository {
	return &TelegramSessionRepository{queries: queries}
}

func convertDbTelegramSession(s *db.TelegramSession) TelegramSession {
	sess := TelegramSession{
		SessionDataEncrypted: s.SessionDataEncrypted,
		EncryptionNonce:      s.EncryptionNonce,
		AuthState:            s.AuthState,
	}
	if s.PhoneNumber.Valid {
		sess.PhoneNumber = &s.PhoneNumber.String
	}
	if s.TelegramUserID.Valid {
		sess.TelegramUserID = &s.TelegramUserID.Int64
	}
	if s.Username.Valid {
		sess.Username = &s.Username.String
	}
	if s.CreatedAt.Valid {
		sess.CreatedAt = s.CreatedAt.Time
	}
	if s.UpdatedAt.Valid {
		sess.UpdatedAt = s.UpdatedAt.Time
	}
	return sess
}

// GetSession retrieves the singleton telegram session
func (r *TelegramSessionRepository) GetSession(ctx context.Context) (*TelegramSession, error) {
	dbSess, err := r.queries.GetTelegramSession(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	sess := convertDbTelegramSession(dbSess)
	return &sess, nil
}

// UpsertSession creates or updates the full telegram session
func (r *TelegramSessionRepository) UpsertSession(ctx context.Context, params UpsertTelegramSessionParams) (*TelegramSession, error) {
	dbSess, err := r.queries.UpsertTelegramSession(ctx, db.UpsertTelegramSessionParams{
		SessionDataEncrypted: params.SessionDataEncrypted,
		EncryptionNonce:      params.EncryptionNonce,
		PhoneNumber:          stringToPgText(params.PhoneNumber),
		TelegramUserID:       int64ToPgInt8(params.TelegramUserID),
		Username:             stringToPgText(params.Username),
		AuthState:            params.AuthState,
	})
	if err != nil {
		return nil, err
	}
	sess := convertDbTelegramSession(dbSess)
	return &sess, nil
}

// UpsertSessionData updates only the encrypted session data, preserving auth_state.
// Used by gotd/td session.Storage during key exchange (before auth is complete).
func (r *TelegramSessionRepository) UpsertSessionData(ctx context.Context, params UpsertTelegramSessionDataParams) (*TelegramSession, error) {
	dbSess, err := r.queries.UpsertTelegramSessionData(ctx, db.UpsertTelegramSessionDataParams{
		SessionDataEncrypted: params.SessionDataEncrypted,
		EncryptionNonce:      params.EncryptionNonce,
	})
	if err != nil {
		return nil, err
	}
	sess := convertDbTelegramSession(dbSess)
	return &sess, nil
}

// UpdateAuthState updates the auth state of the telegram session
func (r *TelegramSessionRepository) UpdateAuthState(ctx context.Context, authState string) (*TelegramSession, error) {
	dbSess, err := r.queries.UpdateTelegramSessionAuthState(ctx, authState)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	sess := convertDbTelegramSession(dbSess)
	return &sess, nil
}

// UpdateUserInfo updates user info fields on the telegram session
func (r *TelegramSessionRepository) UpdateUserInfo(ctx context.Context, params UpdateTelegramUserInfoParams) (*TelegramSession, error) {
	dbSess, err := r.queries.UpdateTelegramSessionUserInfo(ctx, db.UpdateTelegramSessionUserInfoParams{
		TelegramUserID: int64ToPgInt8(params.TelegramUserID),
		Username:       stringToPgText(params.Username),
		PhoneNumber:    stringToPgText(params.PhoneNumber),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}
	sess := convertDbTelegramSession(dbSess)
	return &sess, nil
}

// DeleteSession deletes the telegram session
func (r *TelegramSessionRepository) DeleteSession(ctx context.Context) error {
	return r.queries.DeleteTelegramSession(ctx)
}

// int64ToPgInt8 converts an *int64 to pgtype.Int8
func int64ToPgInt8(v *int64) pgtype.Int8 {
	if v == nil {
		return pgtype.Int8{Valid: false}
	}
	return pgtype.Int8{Int64: *v, Valid: true}
}
