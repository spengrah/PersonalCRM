package repository

import (
	"context"
	"errors"
	"time"

	"personal-crm/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// OAuthCredential represents stored OAuth credentials (domain model)
type OAuthCredential struct {
	ID                    uuid.UUID  `json:"id"`
	Provider              string     `json:"provider"`
	AccountID             string     `json:"account_id"`
	AccountName           *string    `json:"account_name,omitempty"`
	AccessTokenEncrypted  []byte     `json:"-"` // Never expose in JSON
	RefreshTokenEncrypted []byte     `json:"-"` // Never expose in JSON
	RefreshTokenNonce     []byte     `json:"-"` // Never expose in JSON; NULL means legacy shared-nonce format
	EncryptionNonce       []byte     `json:"-"` // Never expose in JSON
	TokenType             string     `json:"token_type"`
	ExpiresAt             *time.Time `json:"expires_at,omitempty"`
	Scopes                []string   `json:"scopes,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
	UpdatedAt             time.Time  `json:"updated_at"`
}

// OAuthCredentialStatus represents non-sensitive credential info for display
type OAuthCredentialStatus struct {
	ID          uuid.UUID  `json:"id"`
	Provider    string     `json:"provider"`
	AccountID   string     `json:"account_id"`
	AccountName *string    `json:"account_name,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	Scopes      []string   `json:"scopes,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

// UpsertOAuthCredentialRequest holds parameters for creating/updating credentials
type UpsertOAuthCredentialRequest struct {
	Provider              string
	AccountID             string
	AccountName           *string
	AccessTokenEncrypted  []byte
	RefreshTokenEncrypted []byte
	RefreshTokenNonce     []byte
	EncryptionNonce       []byte
	TokenType             string
	ExpiresAt             *time.Time
	Scopes                []string
}

// UpdateOAuthTokensRequest holds parameters for updating tokens
type UpdateOAuthTokensRequest struct {
	AccessTokenEncrypted  []byte
	RefreshTokenEncrypted []byte
	RefreshTokenNonce     []byte
	EncryptionNonce       []byte
	ExpiresAt             *time.Time
}

// OAuthRepository handles OAuth credential persistence
type OAuthRepository struct {
	queries db.Querier
}

// NewOAuthRepository creates a new OAuth repository
func NewOAuthRepository(queries db.Querier) *OAuthRepository {
	return &OAuthRepository{queries: queries}
}

// convertDbOAuthCredential converts a database credential to a domain model
func convertDbOAuthCredential(dbCred *db.OauthCredential) OAuthCredential {
	cred := OAuthCredential{
		Provider:              dbCred.Provider,
		AccountID:             dbCred.AccountID,
		AccessTokenEncrypted:  dbCred.AccessTokenEncrypted,
		RefreshTokenEncrypted: dbCred.RefreshTokenEncrypted,
		RefreshTokenNonce:     dbCred.RefreshTokenNonce,
		EncryptionNonce:       dbCred.EncryptionNonce,
		TokenType:             "Bearer",
		Scopes:                dbCred.Scopes,
	}

	// Convert UUID
	cred.ID = dbCred.ID

	// Convert nullable fields
	cred.AccountName = dbCred.AccountName
	if dbCred.TokenType != nil {
		cred.TokenType = *dbCred.TokenType
	}

	// Convert timestamps
	cred.ExpiresAt = dbCred.ExpiresAt
	if dbCred.CreatedAt != nil {
		cred.CreatedAt = *dbCred.CreatedAt
	}
	if dbCred.UpdatedAt != nil {
		cred.UpdatedAt = *dbCred.UpdatedAt
	}

	return cred
}

// convertDbOAuthCredentialStatus converts a database status row to a domain model
func convertDbOAuthCredentialStatus(dbRow *db.ListOAuthCredentialStatusesRow) OAuthCredentialStatus {
	status := OAuthCredentialStatus{
		Provider:  dbRow.Provider,
		AccountID: dbRow.AccountID,
		Scopes:    dbRow.Scopes,
	}

	// Convert UUID
	status.ID = dbRow.ID

	// Convert nullable fields
	status.AccountName = dbRow.AccountName

	// Convert timestamps
	status.ExpiresAt = dbRow.ExpiresAt
	if dbRow.CreatedAt != nil {
		status.CreatedAt = *dbRow.CreatedAt
	}
	if dbRow.UpdatedAt != nil {
		status.UpdatedAt = *dbRow.UpdatedAt
	}

	return status
}

// convertDbOAuthCredentialStatusFromGet converts GetOAuthCredentialStatusRow to domain model
func convertDbOAuthCredentialStatusFromGet(dbRow *db.GetOAuthCredentialStatusRow) OAuthCredentialStatus {
	status := OAuthCredentialStatus{
		Provider:  dbRow.Provider,
		AccountID: dbRow.AccountID,
		Scopes:    dbRow.Scopes,
	}

	// Convert UUID
	status.ID = dbRow.ID

	// Convert nullable fields
	status.AccountName = dbRow.AccountName

	// Convert timestamps
	status.ExpiresAt = dbRow.ExpiresAt
	if dbRow.CreatedAt != nil {
		status.CreatedAt = *dbRow.CreatedAt
	}
	if dbRow.UpdatedAt != nil {
		status.UpdatedAt = *dbRow.UpdatedAt
	}

	return status
}

// GetByProviderAndAccount retrieves a credential by provider and account ID
func (r *OAuthRepository) GetByProviderAndAccount(ctx context.Context, provider, accountID string) (*OAuthCredential, error) {
	dbCred, err := r.queries.GetOAuthCredential(ctx, db.GetOAuthCredentialParams{
		Provider:  provider,
		AccountID: accountID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	cred := convertDbOAuthCredential(dbCred)
	return &cred, nil
}

// GetByID retrieves a credential by UUID
func (r *OAuthRepository) GetByID(ctx context.Context, id uuid.UUID) (*OAuthCredential, error) {
	dbCred, err := r.queries.GetOAuthCredentialByID(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	cred := convertDbOAuthCredential(dbCred)
	return &cred, nil
}

// ListByProvider retrieves all credentials for a provider
func (r *OAuthRepository) ListByProvider(ctx context.Context, provider string) ([]OAuthCredential, error) {
	dbCreds, err := r.queries.ListOAuthCredentials(ctx, provider)
	if err != nil {
		return nil, err
	}

	creds := make([]OAuthCredential, len(dbCreds))
	for i, dbCred := range dbCreds {
		creds[i] = convertDbOAuthCredential(dbCred)
	}

	return creds, nil
}

// ListStatusesByProvider retrieves non-sensitive info for all credentials of a provider
func (r *OAuthRepository) ListStatusesByProvider(ctx context.Context, provider string) ([]OAuthCredentialStatus, error) {
	dbRows, err := r.queries.ListOAuthCredentialStatuses(ctx, provider)
	if err != nil {
		return nil, err
	}

	statuses := make([]OAuthCredentialStatus, len(dbRows))
	for i, dbRow := range dbRows {
		statuses[i] = convertDbOAuthCredentialStatus(dbRow)
	}

	return statuses, nil
}

// GetStatus retrieves non-sensitive info for a specific credential
func (r *OAuthRepository) GetStatus(ctx context.Context, id uuid.UUID) (*OAuthCredentialStatus, error) {
	dbRow, err := r.queries.GetOAuthCredentialStatus(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	status := convertDbOAuthCredentialStatusFromGet(dbRow)
	return &status, nil
}

// Upsert creates or updates an OAuth credential
func (r *OAuthRepository) Upsert(ctx context.Context, req UpsertOAuthCredentialRequest) (*OAuthCredential, error) {
	dbCred, err := r.queries.UpsertOAuthCredential(ctx, db.UpsertOAuthCredentialParams{
		Provider:              req.Provider,
		AccountID:             req.AccountID,
		AccountName:           req.AccountName,
		AccessTokenEncrypted:  req.AccessTokenEncrypted,
		RefreshTokenEncrypted: req.RefreshTokenEncrypted,
		RefreshTokenNonce:     req.RefreshTokenNonce,
		EncryptionNonce:       req.EncryptionNonce,
		TokenType:             nilIfEmpty(req.TokenType),
		ExpiresAt:             req.ExpiresAt,
		Scopes:                req.Scopes,
	})
	if err != nil {
		return nil, err
	}

	cred := convertDbOAuthCredential(dbCred)
	return &cred, nil
}

// UpdateTokens updates only the token data for a credential
func (r *OAuthRepository) UpdateTokens(ctx context.Context, id uuid.UUID, req UpdateOAuthTokensRequest) (*OAuthCredential, error) {
	dbCred, err := r.queries.UpdateOAuthCredentialTokens(ctx, db.UpdateOAuthCredentialTokensParams{
		ID:                    id,
		AccessTokenEncrypted:  req.AccessTokenEncrypted,
		RefreshTokenEncrypted: req.RefreshTokenEncrypted,
		RefreshTokenNonce:     req.RefreshTokenNonce,
		EncryptionNonce:       req.EncryptionNonce,
		ExpiresAt:             req.ExpiresAt,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, err
	}

	cred := convertDbOAuthCredential(dbCred)
	return &cred, nil
}

// Delete removes a credential by ID
func (r *OAuthRepository) Delete(ctx context.Context, id uuid.UUID) error {
	return r.queries.DeleteOAuthCredential(ctx, id)
}

// DeleteByProvider removes all credentials for a provider
func (r *OAuthRepository) DeleteByProvider(ctx context.Context, provider string) error {
	return r.queries.DeleteOAuthCredentialByProvider(ctx, provider)
}

// Count returns the number of credentials for a provider
func (r *OAuthRepository) Count(ctx context.Context, provider string) (int64, error) {
	return r.queries.CountOAuthCredentials(ctx, provider)
}
