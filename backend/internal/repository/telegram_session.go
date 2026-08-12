package repository

import (
	"context"
	"errors"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/jackc/pgx/v5"
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
	sess.PhoneNumber = s.PhoneNumber
	sess.TelegramUserID = s.TelegramUserID
	sess.Username = s.Username
	if s.CreatedAt != nil {
		sess.CreatedAt = *s.CreatedAt
	}
	if s.UpdatedAt != nil {
		sess.UpdatedAt = *s.UpdatedAt
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
		PhoneNumber:          params.PhoneNumber,
		TelegramUserID:       params.TelegramUserID,
		Username:             params.Username,
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
		TelegramUserID: params.TelegramUserID,
		Username:       params.Username,
		PhoneNumber:    params.PhoneNumber,
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
