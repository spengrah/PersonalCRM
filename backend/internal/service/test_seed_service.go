package service

import (
	"context"
	"fmt"
	"strings"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
)

// TestSeedService is the home for the PREFIX shape of /test/cleanup. Every
// bespoke /test/seed/* body is gone: provisioning is declared, and the declared
// path drives internal/synthetic/declare from the handler under its own
// documented layering exception.
//
// The prefix sweep still has work to do, so it is not a vestige: a test's own
// rows — contacts it created through the product's own API, notes it wrote —
// carry its prefix and belong to no declared namespace, and the prefix-delete tx +
// its queries live in the repository so the handler never calls db.New(tx)
// queries directly.
//
// It does NOT import the synthetic package: service must not, or it would close a
// service→synthetic→replay→service cycle.
type TestSeedService struct {
	database        *db.Database
	meetingNoteRepo *repository.MeetingNoteRepository
}

// NewTestSeedService constructs the test-seed service.
func NewTestSeedService(
	database *db.Database,
	meetingNoteRepo *repository.MeetingNoteRepository,
) *TestSeedService {
	return &TestSeedService{
		database:        database,
		meetingNoteRepo: meetingNoteRepo,
	}
}

// --- /test/cleanup ---------------------------------------------------------

// CleanupResult reports the per-table prefix-delete counts (preserving the HTTP
// response shape).
type CleanupResult struct {
	DeletedContacts         int64
	DeletedExternalContacts int64
	DeletedCalendarEvents   int64
}

// Cleanup deletes prefix-keyed test data (contacts, external contacts, calendar
// events) atomically, plus host-scoped meeting notes. The prefix-delete tx +
// queries now live in the repository (SyntheticSupportRepository.CleanupByPrefix)
// — the handler no longer calls db.New(tx) queries directly (the layer fix). The
// LIKE-wildcard escaping stays at the service boundary, exactly where the handler
// did it before.
func (s *TestSeedService) Cleanup(ctx context.Context, prefix string, hostID *uuid.UUID) (CleanupResult, error) {
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
	res.DeletedExternalContacts = deleted.DeletedExternalContacts
	res.DeletedCalendarEvents = deleted.DeletedCalendarEvents

	// Meeting notes are seeded with random session UUIDs scoped to a host, so
	// cleanup is by host id rather than by prefix.
	if hostID != nil {
		if err := s.meetingNoteRepo.TestHardDeleteByHostID(ctx, *hostID); err != nil {
			return res, fmt.Errorf("cleanup: delete meeting notes: %w", err)
		}
	}

	return res, nil
}

// escapeSQLLikeWildcards escapes SQL LIKE pattern wildcards (% and _) so a
// caller-supplied prefix is matched literally (prevents LIKE-wildcard injection).
// Moved verbatim from the /cleanup handler as part of the layer fix.
func escapeSQLLikeWildcards(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}
