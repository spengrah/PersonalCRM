package repository

import (
	"context"

	"github.com/google/uuid"
)

// CountOAuthCredentialWithNullTokenTypeForTest reports whether the stored
// token_type column for one credential is SQL NULL. Test-only support: the
// OAuthCredential struct flattens NULL and the empty string to the same Go value, so the
// integration test that pins NULL semantics must read the column itself.
// Id-scoped so the assertion cannot be satisfied by a sibling test's row.
func (r *OAuthRepository) CountOAuthCredentialWithNullTokenTypeForTest(ctx context.Context, id uuid.UUID) (int64, error) {
	return r.queries.CountOAuthCredentialWithNullTokenType(ctx, id)
}
