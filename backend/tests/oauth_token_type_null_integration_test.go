package tests

import (
	"context"
	"os"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/stretchr/testify/require"
)

// TestOAuthCredential_TokenTypeStoredNullNotEmptyString verifies the §5.1
// conditional-nullable-literal fix in OAuthRepository.Upsert: an empty
// TokenType must round-trip as SQL NULL (via nilIfEmpty), never as the
// two-character empty string. The stored struct flattens both NULL and ""
// back to "Bearer" on read (see convertDbOAuthCredential), so the only way to
// discriminate them is to assert directly on the stored column — which is
// exactly what CountOAuthCredentialWithNullTokenTypeForTest does.
func TestOAuthCredential_TokenTypeStoredNullNotEmptyString(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	repo := repository.NewOAuthRepository(database.Queries)

	provider := syntheticNS(t) + "-token-type-null-provider"
	accountID := syntheticNS(t) + "-token-type-null-account"

	cred, err := repo.Upsert(ctx, repository.UpsertOAuthCredentialRequest{
		Provider:              provider,
		AccountID:             accountID,
		AccessTokenEncrypted:  []byte("access-ciphertext"),
		RefreshTokenEncrypted: []byte("refresh-ciphertext"),
		EncryptionNonce:       []byte("nonce-bytes-12"),
		TokenType:             "", // must land as SQL NULL, not ""
		Scopes:                []string{"scope-a"},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = repo.Delete(context.Background(), cred.ID)
	})

	n, err := repo.CountOAuthCredentialWithNullTokenTypeForTest(ctx, cred.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "empty TokenType must be stored as SQL NULL")
}
