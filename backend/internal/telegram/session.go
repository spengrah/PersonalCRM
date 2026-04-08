package telegram

import (
	"context"
	"errors"
	"fmt"

	"personal-crm/backend/internal/crypto"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/gotd/td/session"
)

// DatabaseSessionStorage implements gotd/td session.Storage backed by PostgreSQL.
type DatabaseSessionStorage struct {
	repo      *repository.TelegramSessionRepository
	encryptor *crypto.TokenEncryptor
}

// NewDatabaseSessionStorage creates a new encrypted database session storage.
func NewDatabaseSessionStorage(repo *repository.TelegramSessionRepository, encryptor *crypto.TokenEncryptor) *DatabaseSessionStorage {
	return &DatabaseSessionStorage{repo: repo, encryptor: encryptor}
}

// LoadSession implements session.Storage.
func (s *DatabaseSessionStorage) LoadSession(ctx context.Context) ([]byte, error) {
	sess, err := s.repo.GetSession(ctx)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, session.ErrNotFound
		}
		return nil, fmt.Errorf("load session: %w", err)
	}
	plaintext, err := s.encryptor.Decrypt(sess.SessionDataEncrypted, sess.EncryptionNonce)
	if err != nil {
		return nil, fmt.Errorf("decrypt session: %w", err)
	}
	return []byte(plaintext), nil
}

// StoreSession implements session.Storage.
// Called by gotd/td during key exchange, which can happen BEFORE auth is complete.
// Only updates encrypted session data — does NOT touch auth_state.
func (s *DatabaseSessionStorage) StoreSession(ctx context.Context, data []byte) error {
	ciphertext, nonce, err := s.encryptor.Encrypt(string(data))
	if err != nil {
		return fmt.Errorf("encrypt session: %w", err)
	}
	_, err = s.repo.UpsertSessionData(ctx, repository.UpsertTelegramSessionDataParams{
		SessionDataEncrypted: ciphertext,
		EncryptionNonce:      nonce,
	})
	return err
}
