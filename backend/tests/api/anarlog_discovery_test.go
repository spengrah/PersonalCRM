package api

import (
	"context"
	"os"
	"testing"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/anarlog"
	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// discoveryTestEnv bundles the minimal wiring the discovery integration
// tests need: a live DB, the external_contact repo, a cadence-wired
// ContactService, the discovery service, and a tx-bound seeder.
type discoveryTestEnv struct {
	database     *db.Database
	externalRepo *repository.ExternalContactRepository
	contactRepo  *repository.ContactRepository
	contactSvc   *service.ContactService
	meetingRepo  *repository.MeetingNoteRepository
	svc          *service.AnarlogDiscoveryService
	tokenSuffix  string
}

func setupDiscoveryEnv(t *testing.T) *discoveryTestEnv {
	t.Helper()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	ctx := context.Background()
	require.NoError(t, db.RunMigrations(ctx, databaseURL, getMigrationsPath()))

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	externalRepo := repository.NewExternalContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	meetingRepo := repository.NewMeetingNoteRepository(database.Queries)

	contactSvc := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, contactTaskRepo, nil, service.NewRematchService())
	contactSvc.SetCadenceUpdater(wireCadenceUpdaterForAPITest(t, database, contactSvc))

	svc := service.NewAnarlogDiscoveryService(externalRepo, contactSvc)

	env := &discoveryTestEnv{
		database:     database,
		externalRepo: externalRepo,
		contactRepo:  contactRepo,
		contactSvc:   contactSvc,
		meetingRepo:  meetingRepo,
		svc:          svc,
		// Randomized per-test suffix so concurrent runs against the shared
		// template DB don't collide on normalized tokens.
		tokenSuffix: uuid.NewString()[:8],
	}
	t.Cleanup(func() {
		database.Close()
	})
	return env
}

// seedTitleRow writes one anarlog_title external_contact row for the
// (token, session) pair via the production discovery writer path (no raw
// SQL). Returns the row id.
func (e *discoveryTestEnv) seedTitleRow(t *testing.T, token string, sessionUUID uuid.UUID) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	tx, err := e.database.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()

	writer := anarlog.NewDiscoveryWriter(e.externalRepo)
	require.NoError(t, writer.UpsertTitleCandidateTx(ctx, tx, sessionUUID, token, token))
	require.NoError(t, tx.Commit(ctx))

	// Read the row back so callers can reference its id.
	row, err := e.externalRepo.GetBySource(ctx, "anarlog_title", anarlogTitleSourceID(token, sessionUUID), nil)
	require.NoError(t, err)
	require.NotNil(t, row)
	return row.ID
}

// seedMeetingNote inserts a meeting_note row so the discovery JOIN can
// surface its title as evidence.
func (e *discoveryTestEnv) seedMeetingNote(t *testing.T, sessionUUID uuid.UUID, title string) {
	t.Helper()
	ctx := context.Background()
	tx, err := e.database.Pool.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(ctx) }()
	titleCopy := title
	_, err = e.meetingRepo.InsertMeetingNoteTx(ctx, tx, repository.InsertMeetingNoteParams{
		AnarlogSessionID: sessionUUID,
		Title:            &titleCopy,
		LinkageState:     repository.LinkageStateOrphanNeedsReview,
		// input_hash / resolved_set_hash are constrained to '' or 64-char
		// lowercase hex; empty is allowed and sufficient for an
		// evidence-only fixture.
		InputHash:       "",
		ResolvedSetHash: "",
		MeetingAt:       accelerated.GetCurrentTime(),
	})
	require.NoError(t, err)
	require.NoError(t, tx.Commit(ctx))
	t.Cleanup(func() {
		cleanTx, cErr := e.database.Pool.Begin(ctx)
		if cErr != nil {
			return
		}
		defer func() { _ = cleanTx.Rollback(ctx) }()
		_ = e.meetingRepo.SoftDeleteMeetingNoteBySessionIDTx(ctx, cleanTx, sessionUUID)
		_ = cleanTx.Commit(ctx)
	})
}

// anarlogTitleSourceID mirrors the discovery writer's deterministic
// source_id recipe so tests can read back a seeded row.
func anarlogTitleSourceID(normalizedToken string, sessionUUID uuid.UUID) string {
	return anarlog.ComputeAnarlogTitleSourceIDForTest(normalizedToken, sessionUUID)
}

func (e *discoveryTestEnv) findGroup(t *testing.T, token string) *service.DiscoveryGroup {
	t.Helper()
	groups, err := e.svc.ListGroups(context.Background())
	require.NoError(t, err)
	for i := range groups {
		if groups[i].NormalizedToken == token {
			return &groups[i]
		}
	}
	return nil
}

func TestAnarlogDiscovery_ListGroups(t *testing.T) {
	t.Parallel()
	env := setupDiscoveryEnv(t)
	ctx := context.Background()

	tokenA := "alpha" + env.tokenSuffix
	tokenB := "beta" + env.tokenSuffix
	sess1 := uuid.New()
	sess2 := uuid.New()
	sess3 := uuid.New()

	// tokenA appears in two sessions (evidence_count=2), tokenB in one.
	env.seedTitleRow(t, tokenA, sess1)
	env.seedTitleRow(t, tokenA, sess2)
	env.seedTitleRow(t, tokenB, sess3)
	env.seedMeetingNote(t, sess1, "Design sync "+env.tokenSuffix)
	env.seedMeetingNote(t, sess2, "Roadmap "+env.tokenSuffix)
	// sess3 has no meeting_note → tokenB still surfaces via member row.

	t.Cleanup(func() {
		_ = env.externalRepo.MarkAnarlogTitleSiblingsIgnoredByToken(ctx, tokenA)
		_ = env.externalRepo.MarkAnarlogTitleSiblingsIgnoredByToken(ctx, tokenB)
	})

	groupA := env.findGroup(t, tokenA)
	require.NotNil(t, groupA)
	require.Equal(t, int64(2), groupA.EvidenceCount)
	require.Len(t, groupA.SessionTitles, 2)

	groupB := env.findGroup(t, tokenB)
	require.NotNil(t, groupB)
	require.Equal(t, int64(1), groupB.EvidenceCount)
	// tokenB's session has no meeting_note → empty (non-nil) titles.
	require.Empty(t, groupB.SessionTitles)
}

func TestAnarlogDiscovery_ResolveImport(t *testing.T) {
	t.Parallel()
	env := setupDiscoveryEnv(t)
	ctx := context.Background()
	token := "gamma" + env.tokenSuffix
	sess1, sess2 := uuid.New(), uuid.New()
	id1 := env.seedTitleRow(t, token, sess1)
	id2 := env.seedTitleRow(t, token, sess2)

	cad := "monthly"
	res, err := env.svc.ResolveToken(ctx, service.ResolveTokenRequest{
		NormalizedToken: token,
		Action:          service.DiscoveryActionImport,
		Cadence:         &cad,
	})
	require.NoError(t, err)
	require.NotNil(t, res.ContactID)
	t.Cleanup(func() { _ = env.contactRepo.HardDeleteContact(ctx, *res.ContactID) })

	// Both siblings flipped to imported, pointing at the new contact.
	for _, id := range []uuid.UUID{id1, id2} {
		row, gErr := env.externalRepo.GetByID(ctx, id)
		require.NoError(t, gErr)
		require.Equal(t, repository.MatchStatusImported, row.MatchStatus)
		require.NotNil(t, row.CRMContactID)
		require.Equal(t, *res.ContactID, *row.CRMContactID)
	}

	// The created contact carries the cadence + a contact_by derived from it.
	contact, err := env.contactSvc.GetContact(ctx, *res.ContactID)
	require.NoError(t, err)
	require.NotNil(t, contact.Cadence)
	require.Equal(t, "monthly", *contact.Cadence)
	require.NotNil(t, contact.ContactBy)
}

func TestAnarlogDiscovery_ResolveIgnore(t *testing.T) {
	t.Parallel()
	env := setupDiscoveryEnv(t)
	ctx := context.Background()
	token := "delta" + env.tokenSuffix
	id := env.seedTitleRow(t, token, uuid.New())

	res, err := env.svc.ResolveToken(ctx, service.ResolveTokenRequest{
		NormalizedToken: token,
		Action:          service.DiscoveryActionIgnore,
	})
	require.NoError(t, err)
	require.Nil(t, res.ContactID)

	row, err := env.externalRepo.GetByID(ctx, id)
	require.NoError(t, err)
	require.Equal(t, repository.MatchStatusIgnored, row.MatchStatus)

	// A second resolve finds zero live siblings → token-group-not-found.
	_, err = env.svc.ResolveToken(ctx, service.ResolveTokenRequest{
		NormalizedToken: token,
		Action:          service.DiscoveryActionIgnore,
	})
	require.ErrorIs(t, err, service.ErrTokenGroupNotFound)
}

func TestAnarlogDiscovery_ResolveLinkMissingContact(t *testing.T) {
	t.Parallel()
	env := setupDiscoveryEnv(t)
	ctx := context.Background()
	token := "epsilon" + env.tokenSuffix
	env.seedTitleRow(t, token, uuid.New())
	t.Cleanup(func() { _ = env.externalRepo.MarkAnarlogTitleSiblingsIgnoredByToken(ctx, token) })

	missing := uuid.New()
	_, err := env.svc.ResolveToken(ctx, service.ResolveTokenRequest{
		NormalizedToken: token,
		Action:          service.DiscoveryActionLink,
		CRMContactID:    &missing,
	})
	require.ErrorIs(t, err, service.ErrDiscoveryContactMissing)
}

func TestAnarlogDiscovery_ResolveLinkPreservesProfileAndCadence(t *testing.T) {
	t.Parallel()
	env := setupDiscoveryEnv(t)
	ctx := context.Background()

	// Seed a contact with profile fields + a known cadence/contact_by.
	loc := "Berlin"
	how := "conference"
	startCadence := "weekly"
	contact, _, err := env.contactSvc.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Original Name " + env.tokenSuffix,
		Location: &loc,
		HowMet:   &how,
		Cadence:  &startCadence,
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.contactRepo.HardDeleteContact(ctx, contact.ID) })

	token := "zeta" + env.tokenSuffix
	id := env.seedTitleRow(t, token, uuid.New())

	newName := "Linked Name " + env.tokenSuffix
	newCadence := "monthly"
	res, err := env.svc.ResolveToken(ctx, service.ResolveTokenRequest{
		NormalizedToken: token,
		Action:          service.DiscoveryActionLink,
		Name:            &newName,
		Cadence:         &newCadence,
		CRMContactID:    &contact.ID,
	})
	require.NoError(t, err)
	require.NotNil(t, res.ContactID)
	require.Equal(t, contact.ID, *res.ContactID)

	// Sibling flipped to matched.
	row, err := env.externalRepo.GetByID(ctx, id)
	require.NoError(t, err)
	require.Equal(t, repository.MatchStatusMatched, row.MatchStatus)
	require.Equal(t, contact.ID, *row.CRMContactID)

	// Name + cadence updated; other profile fields preserved; contact_by
	// recomputed through the sole-writer path.
	updated, err := env.contactSvc.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.Equal(t, newName, updated.FullName)
	require.Equal(t, "monthly", *updated.Cadence)
	require.Equal(t, loc, *updated.Location)
	require.Equal(t, how, *updated.HowMet)

	cadenceType, err := cadence.ParseCadence("monthly")
	require.NoError(t, err)
	base := updated.CreatedAt
	if updated.LastContacted != nil {
		base = *updated.LastContacted
	}
	wantBy := cadence.CalculateContactBy(base, cadenceType)
	require.NotNil(t, updated.ContactBy)
	// Compare on the calendar date (the column is a DATE; TZ wrapping
	// differs between the computed value and the DB read-back). The guard
	// is that contact_by is derived from the new cadence via the
	// sole-writer path, NOT corrupted/blanked by a bypass.
	require.Equal(t, wantBy.Format("2006-01-02"), updated.ContactBy.Format("2006-01-02"))
}

func TestAnarlogDiscovery_LinkNameOnlyPreservesContactBy(t *testing.T) {
	t.Parallel()
	env := setupDiscoveryEnv(t)
	ctx := context.Background()

	startCadence := "weekly"
	contact, _, err := env.contactSvc.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Name Only " + env.tokenSuffix,
		Cadence:  &startCadence,
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.contactRepo.HardDeleteContact(ctx, contact.ID) })

	before, err := env.contactSvc.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, before.ContactBy)

	token := "eta" + env.tokenSuffix
	env.seedTitleRow(t, token, uuid.New())

	newName := "Renamed " + env.tokenSuffix
	_, err = env.svc.ResolveToken(ctx, service.ResolveTokenRequest{
		NormalizedToken: token,
		Action:          service.DiscoveryActionLink,
		Name:            &newName, // no cadence
		CRMContactID:    &contact.ID,
	})
	require.NoError(t, err)

	after, err := env.contactSvc.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.Equal(t, newName, after.FullName)
	// cadence unchanged → contact_by recompute is a no-op.
	require.Equal(t, "weekly", *after.Cadence)
	require.Equal(t, before.ContactBy.Format("2006-01-02"), after.ContactBy.Format("2006-01-02"))
}

func TestAnarlogDiscovery_DuplicateRowExcludedFromMark(t *testing.T) {
	t.Parallel()
	env := setupDiscoveryEnv(t)
	ctx := context.Background()
	token := "theta" + env.tokenSuffix
	liveID := env.seedTitleRow(t, token, uuid.New())
	dupID := env.seedTitleRow(t, token, uuid.New())

	// Mark dupID as a duplicate of liveID so it is excluded from the
	// live-sibling predicate.
	require.NoError(t, env.externalRepo.MarkAsDuplicate(ctx, dupID, liveID))

	res, err := env.svc.ResolveToken(ctx, service.ResolveTokenRequest{
		NormalizedToken: token,
		Action:          service.DiscoveryActionImport,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = env.contactRepo.HardDeleteContact(ctx, *res.ContactID) })

	live, err := env.externalRepo.GetByID(ctx, liveID)
	require.NoError(t, err)
	require.Equal(t, repository.MatchStatusImported, live.MatchStatus)

	// The duplicate row is untouched (still unmatched).
	dup, err := env.externalRepo.GetByID(ctx, dupID)
	require.NoError(t, err)
	require.Equal(t, repository.MatchStatusUnmatched, dup.MatchStatus)
}
