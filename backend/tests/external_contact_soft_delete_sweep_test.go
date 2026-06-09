package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// extSweepFixtures bundles two rows (live + tombstoned) plus the
// repository so every audited read query can run a uniform assertion:
// the live row is returned, the tombstoned row is not.
type extSweepFixtures struct {
	repo        *repository.ExternalContactRepository
	live        *repository.ExternalContact
	tombstoned  *repository.ExternalContact
	matchingCRM uuid.UUID
}

// seedExtSweepFixtures inserts one live + one tombstoned external_contact row
// sharing source / matching attributes (so every read query has a non-trivial
// candidate set). The tombstoned row is created live then soft-deleted via
// SoftDeleteTx. Cleanup uses prefix-hard-delete.
func seedExtSweepFixtures(t *testing.T) *extSweepFixtures {
	t.Helper()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	repo := repository.NewExternalContactRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)

	// Unique prefix per test run so concurrent tests don't collide.
	prefix := "sweep-" + syntheticNS(t) + "-"

	// Seed a CRM contact the live external_contact row links to. The
	// ListForCRMContact assertion needs this association.
	crm, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Sweep Test " + prefix,
	})
	require.NoError(t, err)

	syncedAt := accelerated.GetCurrentTime()
	emailValue := "sweep-shared-" + prefix + "@example.invalid"
	liveReq := repository.UpsertExternalContactRequest{
		Source:   "gcontacts", // arbitrary; the soft-delete filter is source-agnostic
		SourceID: prefix + "live",
		Emails:   []repository.EmailEntry{{Value: emailValue}},
		SyncedAt: &syncedAt,
	}
	live, err := repo.Upsert(ctx, liveReq)
	require.NoError(t, err)
	// Mark live row as matched so the per-CRM-contact + unmatched
	// queries each see the expected partition.
	matched, err := repo.UpdateMatch(ctx, live.ID, &crm.ID, repository.MatchStatusMatched)
	require.NoError(t, err)
	live = matched

	tombReq := repository.UpsertExternalContactRequest{
		Source:   "gcontacts",
		SourceID: prefix + "tombstone",
		Emails:   []repository.EmailEntry{{Value: emailValue}},
		SyncedAt: &syncedAt,
	}
	tomb, err := repo.Upsert(ctx, tombReq)
	require.NoError(t, err)
	// Tombstone via SoftDeleteTx. Per .ai/rules/core.md we use the
	// repository's sqlc-backed tx method, not raw SQL.
	tx, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, repo.SoftDeleteTx(ctx, tx, tomb.ID))
	require.NoError(t, tx.Commit(ctx))

	// Re-fetch the tombstoned row via the tombstone-aware GetBySource
	// so the test holds onto the post-soft-delete struct.
	post, err := repo.GetBySource(ctx, "gcontacts", tombReq.SourceID, nil)
	require.NoError(t, err)
	require.NotNil(t, post)
	require.NotNil(t, post.DeletedAt, "fixture must be tombstoned after SoftDeleteTx")
	tomb = post

	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// Hard delete the rows we seeded (covers tombstoned + live).
		_ = database.Queries.TestDeleteExternalContactsBySourceIDPrefix(cleanCtx, prefix)
		_ = contactRepo.HardDeleteContact(cleanCtx, crm.ID)
	})

	return &extSweepFixtures{
		repo:        repo,
		live:        live,
		tombstoned:  tomb,
		matchingCRM: crm.ID,
	}
}

func TestExternalContactSweep_GetByID_LiveRowReturned(t *testing.T) {
	t.Parallel()
	fx := seedExtSweepFixtures(t)
	row, err := fx.repo.GetByID(context.Background(), fx.live.ID)
	require.NoError(t, err)
	require.NotNil(t, row)
	require.Equal(t, fx.live.ID, row.ID)
}

func TestExternalContactSweep_GetByID_TombstonedRowAbsent(t *testing.T) {
	t.Parallel()
	fx := seedExtSweepFixtures(t)
	row, err := fx.repo.GetByID(context.Background(), fx.tombstoned.ID)
	require.NoError(t, err)
	require.Nil(t, row, "GetByID must not return a tombstoned row through normal flows")
}

func TestExternalContactSweep_GetBySource_TombstoneAware(t *testing.T) {
	t.Parallel()
	fx := seedExtSweepFixtures(t)
	// Live row reachable.
	live, err := fx.repo.GetBySource(context.Background(), fx.live.Source, fx.live.SourceID, nil)
	require.NoError(t, err)
	require.NotNil(t, live)
	require.Nil(t, live.DeletedAt)
	// Tombstoned row INTENTIONALLY visible via GetBySource — the
	// mac-daemon revive path depends on this. Callers that want
	// live-only rows must filter on DeletedAt themselves.
	tomb, err := fx.repo.GetBySource(context.Background(), fx.tombstoned.Source, fx.tombstoned.SourceID, nil)
	require.NoError(t, err)
	require.NotNil(t, tomb)
	require.NotNil(t, tomb.DeletedAt)
}

func TestExternalContactSweep_FindBySourceAndSourceID_FiltersTombstone(t *testing.T) {
	t.Parallel()
	fx := seedExtSweepFixtures(t)
	// Use the tombstoned row's source_id; FindBySourceAndSourceID is
	// the rematch query and must NEVER return a tombstoned candidate.
	results, err := fx.repo.FindBySourceAndSourceID(
		context.Background(), fx.tombstoned.Source, fx.tombstoned.SourceID)
	require.NoError(t, err)
	for _, r := range results {
		require.NotEqual(t, fx.tombstoned.ID, r.ID, "rematch must not surface a tombstoned candidate")
	}
}

func TestExternalContactSweep_ListUnmatched_FiltersTombstone(t *testing.T) {
	t.Parallel()
	// Re-seed but with both rows unmatched so the predicate fires.
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	repo := repository.NewExternalContactRepository(database.Queries)
	prefix := "sweep-unm-" + syntheticNS(t) + "-"
	syncedAt := accelerated.GetCurrentTime()

	live, err := repo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:   "gcontacts",
		SourceID: prefix + "live",
		SyncedAt: &syncedAt,
	})
	require.NoError(t, err)
	tomb, err := repo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:   "gcontacts",
		SourceID: prefix + "tomb",
		SyncedAt: &syncedAt,
	})
	require.NoError(t, err)
	tx, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, repo.SoftDeleteTx(ctx, tx, tomb.ID))
	require.NoError(t, tx.Commit(ctx))
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = database.Queries.TestDeleteExternalContactsBySourceIDPrefix(cleanCtx, prefix)
	})

	listed, err := repo.ListUnmatched(ctx, "gcontacts", 1000, 0, false)
	require.NoError(t, err)
	foundLive := false
	for _, r := range listed {
		if r.ID == tomb.ID {
			t.Fatalf("ListUnmatched must not return tombstoned row")
		}
		if r.ID == live.ID {
			foundLive = true
		}
	}
	require.True(t, foundLive, "ListUnmatched must return the live row")
}

func TestExternalContactSweep_ListAllUnmatched_FiltersTombstone(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	t.Parallel()
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	repo := repository.NewExternalContactRepository(database.Queries)
	prefix := "sweep-allunm-" + syntheticNS(t) + "-"
	syncedAt := accelerated.GetCurrentTime()
	live, err := repo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source: "gcontacts", SourceID: prefix + "live", SyncedAt: &syncedAt,
	})
	require.NoError(t, err)
	tomb, err := repo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source: "gcontacts", SourceID: prefix + "tomb", SyncedAt: &syncedAt,
	})
	require.NoError(t, err)
	tx, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, repo.SoftDeleteTx(ctx, tx, tomb.ID))
	require.NoError(t, tx.Commit(ctx))
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = database.Queries.TestDeleteExternalContactsBySourceIDPrefix(cleanCtx, prefix)
	})

	listed, err := repo.ListAllUnmatched(ctx, 1000, 0, false)
	require.NoError(t, err)
	foundLive := false
	for _, r := range listed {
		if r.ID == tomb.ID {
			t.Fatalf("ListAllUnmatched must not return tombstoned row")
		}
		if r.ID == live.ID {
			foundLive = true
		}
	}
	require.True(t, foundLive)
}

func TestExternalContactSweep_CountUnmatched_ExcludesTombstone(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	t.Parallel()
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	repo := repository.NewExternalContactRepository(database.Queries)
	// Use a unique source per-run so counts are bounded. The namespace token
	// is alphanumeric, so it needs no hyphen stripping.
	source := "sweep-src-" + syntheticNS(t)
	prefix := "sweep-cnt-" + syntheticNS(t) + "-"
	syncedAt := accelerated.GetCurrentTime()
	_, err = repo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source: source, SourceID: prefix + "live-a", SyncedAt: &syncedAt,
	})
	require.NoError(t, err)
	_, err = repo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source: source, SourceID: prefix + "live-b", SyncedAt: &syncedAt,
	})
	require.NoError(t, err)
	tomb, err := repo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source: source, SourceID: prefix + "tomb", SyncedAt: &syncedAt,
	})
	require.NoError(t, err)
	tx, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, repo.SoftDeleteTx(ctx, tx, tomb.ID))
	require.NoError(t, tx.Commit(ctx))
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = database.Queries.TestDeleteExternalContactsBySourceIDPrefix(cleanCtx, prefix)
	})

	count, err := repo.CountUnmatched(ctx, source, false)
	require.NoError(t, err)
	require.Equal(t, int64(2), count, "tombstoned row must not be counted")
}

// NOTE: this func stays SERIAL. CountHiddenUnresolvedTelegram is a DB-wide
// COUNT(*) over source='telegram' with no prefix parameter, so it cannot be
// scoped to this test's rows. The before/after delta is racy under t.Parallel()
// because a concurrent telegram test can create/remove a matching hidden
// unresolved row between the baseline and final reads. The other 11 funcs in
// this file are prefix-scoped and flip.
func TestExternalContactUnmatched_HidesUnresolvedTelegramByDefault(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	repo := repository.NewExternalContactRepository(database.Queries)
	prefix := "tg-hidden-" + syntheticNS(t) + "-"
	syncedAt := accelerated.GetCurrentTime()
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = database.Queries.TestDeleteExternalContactsBySourceIDPrefix(cleanCtx, prefix)
	})

	// CountHiddenUnresolvedTelegram is a DB-wide COUNT with no prefix filter, so
	// under t.Parallel() it sees other tests' rows. Capture baselines before
	// seeding and assert the DELTA this test contributes (exactly one hidden
	// unresolved telegram row), not an absolute count.
	baseTelegram, err := repo.CountHiddenUnresolvedTelegram(ctx, "telegram")
	require.NoError(t, err)
	baseAll, err := repo.CountHiddenUnresolvedTelegram(ctx, "")
	require.NoError(t, err)
	baseGcontacts, err := repo.CountHiddenUnresolvedTelegram(ctx, "gcontacts")
	require.NoError(t, err)

	hidden, err := repo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:   "telegram",
		SourceID: prefix + "hidden",
		SyncedAt: &syncedAt,
	})
	require.NoError(t, err)
	withUsername, err := repo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:   "telegram",
		SourceID: prefix + "username",
		Metadata: map[string]any{"username": "@visible"},
		SyncedAt: &syncedAt,
	})
	require.NoError(t, err)
	displayName := "Visible Telegram"
	withName, err := repo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:      "telegram",
		SourceID:    prefix + "name",
		DisplayName: &displayName,
		SyncedAt:    &syncedAt,
	})
	require.NoError(t, err)
	withEmail, err := repo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source:   "telegram",
		SourceID: prefix + "email",
		Emails:   []repository.EmailEntry{{Value: prefix + "visible@example.invalid"}},
		SyncedAt: &syncedAt,
	})
	require.NoError(t, err)

	defaultRows, err := repo.ListUnmatched(ctx, "telegram", 1000, 0, false)
	require.NoError(t, err)
	defaultIDs := externalIDs(defaultRows)
	require.NotContains(t, defaultIDs, hidden.ID)
	require.Contains(t, defaultIDs, withUsername.ID)
	require.Contains(t, defaultIDs, withName.ID)
	require.Contains(t, defaultIDs, withEmail.ID)

	includedRows, err := repo.ListUnmatched(ctx, "telegram", 1000, 0, true)
	require.NoError(t, err)
	require.Contains(t, externalIDs(includedRows), hidden.ID)

	hiddenCount, err := repo.CountHiddenUnresolvedTelegram(ctx, "telegram")
	require.NoError(t, err)
	require.Equal(t, int64(1), hiddenCount-baseTelegram, "this test adds exactly one hidden unresolved telegram row")
	hiddenCountAll, err := repo.CountHiddenUnresolvedTelegram(ctx, "")
	require.NoError(t, err)
	require.Equal(t, int64(1), hiddenCountAll-baseAll, "same row counts under the unfiltered source")
	hiddenCountOther, err := repo.CountHiddenUnresolvedTelegram(ctx, "gcontacts")
	require.NoError(t, err)
	require.Equal(t, int64(0), hiddenCountOther-baseGcontacts, "this test adds no gcontacts hidden rows")

	hiddenAfterList, err := repo.GetByID(ctx, hidden.ID)
	require.NoError(t, err)
	require.NotNil(t, hiddenAfterList)
	require.Equal(t, repository.MatchStatusUnmatched, hiddenAfterList.MatchStatus)
}

func TestExternalContactSweep_FindByNormalizedEmail_FiltersTombstone(t *testing.T) {
	t.Parallel()
	fx := seedExtSweepFixtures(t)
	// The seed planted the same email on both rows. The matched (live)
	// row's email is at index 0.
	require.NotEmpty(t, fx.live.Emails)
	target := fx.live.Emails[0].Value
	results, err := fx.repo.FindByNormalizedEmail(context.Background(), target)
	require.NoError(t, err)
	for _, r := range results {
		require.NotEqual(t, fx.tombstoned.ID, r.ID, "duplicate-email lookup must skip tombstoned rows")
	}
}

func TestExternalContactSweep_ListForCRMContact_FiltersTombstone(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	t.Parallel()
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	repo := repository.NewExternalContactRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	prefix := "sweep-crm-" + syntheticNS(t) + "-"
	syncedAt := accelerated.GetCurrentTime()

	crm, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: "Sweep CRM " + prefix})
	require.NoError(t, err)

	live, err := repo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source: "gcontacts", SourceID: prefix + "live", SyncedAt: &syncedAt,
	})
	require.NoError(t, err)
	_, err = repo.UpdateMatch(ctx, live.ID, &crm.ID, repository.MatchStatusMatched)
	require.NoError(t, err)
	tomb, err := repo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source: "gcontacts", SourceID: prefix + "tomb", SyncedAt: &syncedAt,
	})
	require.NoError(t, err)
	_, err = repo.UpdateMatch(ctx, tomb.ID, &crm.ID, repository.MatchStatusMatched)
	require.NoError(t, err)
	tx, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, repo.SoftDeleteTx(ctx, tx, tomb.ID))
	require.NoError(t, tx.Commit(ctx))
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = database.Queries.TestDeleteExternalContactsBySourceIDPrefix(cleanCtx, prefix)
		_ = contactRepo.HardDeleteContact(cleanCtx, crm.ID)
	})

	results, err := repo.ListForCRMContact(ctx, crm.ID)
	require.NoError(t, err)
	require.Len(t, results, 1, "tombstoned external_contact must not surface in ListForCRMContact")
	require.Equal(t, live.ID, results[0].ID)
}

func externalIDs(rows []repository.ExternalContact) map[uuid.UUID]bool {
	out := make(map[uuid.UUID]bool, len(rows))
	for _, row := range rows {
		out[row.ID] = true
	}
	return out
}

func TestExternalContactSweep_RoundTrip_ReviveAfterSoftDelete(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	t.Parallel()
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	repo := repository.NewExternalContactRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	prefix := "sweep-rt-" + syntheticNS(t) + "-"
	syncedAt := accelerated.GetCurrentTime()

	crm, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: "RT " + prefix})
	require.NoError(t, err)
	row, err := repo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source: "icloud_contacts", SourceID: prefix + "live", SyncedAt: &syncedAt,
	})
	require.NoError(t, err)
	_, err = repo.UpdateMatch(ctx, row.ID, &crm.ID, repository.MatchStatusMatched)
	require.NoError(t, err)
	tx, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, repo.SoftDeleteTx(ctx, tx, row.ID))
	require.NoError(t, tx.Commit(ctx))
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = database.Queries.TestDeleteExternalContactsBySourceIDPrefix(cleanCtx, prefix)
		_ = contactRepo.HardDeleteContact(cleanCtx, crm.ID)
	})

	tombstoned, err := repo.GetBySource(ctx, "icloud_contacts", row.SourceID, nil)
	require.NoError(t, err)
	require.NotNil(t, tombstoned)
	require.NotNil(t, tombstoned.DeletedAt)
	preservedCRM := tombstoned.CRMContactID
	preservedStatus := tombstoned.MatchStatus

	// Revive.
	tx, err = database.Pool.Begin(ctx)
	require.NoError(t, err)
	revived, err := repo.ReviveTx(ctx, tx, tombstoned.ID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	require.NotNil(t, revived)
	require.Nil(t, revived.DeletedAt)
	require.Equal(t, preservedCRM, revived.CRMContactID, "revive must preserve crm_contact_id")
	require.Equal(t, preservedStatus, revived.MatchStatus, "revive must preserve match_status")
}

func TestExternalContactSweep_GetByID_AccountIDNullSemantics(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	t.Parallel()
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	repo := repository.NewExternalContactRepository(database.Queries)
	prefix := "sweep-acct-" + syntheticNS(t) + "-"
	syncedAt := accelerated.GetCurrentTime()

	// account_id = NULL (icloud_contacts shape).
	_, err = repo.Upsert(ctx, repository.UpsertExternalContactRequest{
		Source: "icloud_contacts", SourceID: prefix + "x", SyncedAt: &syncedAt,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = database.Queries.TestDeleteExternalContactsBySourceIDPrefix(cleanCtx, prefix)
	})

	// Lookup with accountID=nil must find the row.
	found, err := repo.GetBySource(ctx, "icloud_contacts", prefix+"x", nil)
	require.NoError(t, err)
	require.NotNil(t, found)
	require.Nil(t, found.AccountID)

	// Lookup with accountID="some-acct" must NOT find the same row.
	otherAcct := "some-acct"
	notFound, err := repo.GetBySource(ctx, "icloud_contacts", prefix+"x", &otherAcct)
	require.NoError(t, err)
	require.Nil(t, notFound, "GetBySource with a non-null accountID must not match a NULL-account row")
}
