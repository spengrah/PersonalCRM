package service

import (
	"context"
	"fmt"
	"strings"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
)

// TestSeedService is the home for the PREFIX shape of /test/cleanup — the sweep
// for rows the PRODUCT itself creates during a test. Provisioning is declared,
// and the declared path drives internal/synthetic/declare from the handler under
// its own documented layering exception.
//
// The prefix sweep is not a vestige of that migration: a contact a test creates
// through the product's own contact form carries the test's prefix in its name
// and belongs to no declared namespace, so a name-keyed sweep is the only route
// that recovers it — and, unlike the product's soft delete, it hard-deletes and
// cascades. The delete tx + its queries live in the repository so the handler
// never calls db.New(tx) queries directly.
//
// It does NOT import the synthetic package: service must not, or it would close a
// service→synthetic→replay→service cycle.
type TestSeedService struct {
	database *db.Database
}

// NewTestSeedService constructs the test-seed service.
func NewTestSeedService(database *db.Database) *TestSeedService {
	return &TestSeedService{database: database}
}

// --- /test/cleanup ---------------------------------------------------------

// CleanupResult reports the prefix-delete count (preserving the HTTP response
// shape).
type CleanupResult struct {
	DeletedContacts int64
}

// Cleanup hard-deletes the contacts whose name carries the test's prefix. The
// delete tx + queries live in the repository (SyntheticSupportRepository.
// CleanupByPrefix) so the handler never calls db.New(tx) queries directly. The
// LIKE-wildcard escaping stays at the service boundary.
func (s *TestSeedService) Cleanup(ctx context.Context, prefix string) (CleanupResult, error) {
	var res CleanupResult

	tx, err := s.database.Pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("cleanup: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	escapedPrefix := escapeSQLLikeWildcards(prefix)
	support := repository.NewSyntheticSupportRepository(db.New(tx))
	deleted, err := support.CleanupByPrefix(ctx, escapedPrefix)
	if err != nil {
		return res, fmt.Errorf("cleanup: prefix deletes: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("cleanup: commit: %w", err)
	}

	res.DeletedContacts = deleted.DeletedContacts

	return res, nil
}

// escapeSQLLikeWildcards escapes SQL LIKE pattern wildcards (% and _) so a
// caller-supplied prefix is matched literally (prevents LIKE-wildcard injection).
func escapeSQLLikeWildcards(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}
