//go:build integration_testdb

package tests

import (
	"context"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// knowledgeCutoverHarness bundles the wired ContactService + the migration
// service + the repos a test reads back. The KnowledgeCacheUpdater is exposed so
// a test can drive the async HandleEvent path directly (no River). Created
// contacts are tracked for FK-ordered cleanup (assertion → node FK is restrict,
// so assertions clear before nodes).
type knowledgeCutoverHarness struct {
	database      *db.Database
	contactSvc    *service.ContactService
	enrichSvc     *service.EnrichmentService
	assertSvc     *service.AssertService
	migrationSvc  *service.ContactKnowledgeMigrationService
	cacheUpdater  *consumer.KnowledgeCacheUpdater
	contactRepo   *repository.ContactRepository
	assertionRepo *repository.AssertionRepository
	nodeRepo      *repository.NodeRepository
	entityRepo    *repository.EntityRepository
	eventRepo     *repository.EventRepository
	support       *repository.SyntheticSupportRepository

	contactIDs []uuid.UUID
	placeNorms []string
}

func newKnowledgeCutoverHarness(t *testing.T, ctx context.Context) *knowledgeCutoverHarness {
	t.Helper()
	// Per-test DB clone: MigrateContactKnowledgeColumns is a GLOBAL scan, so an
	// isolated DB keeps the backfill from racing (or being polluted by) other
	// tests' contacts/place-nodes — letting every case stay t.Parallel().
	database, cfg := newIsolatedRiverTestDB(t, ctx)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	methodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	taskRepo := repository.NewContactTaskRepository(database.Queries)
	nodeRepo := repository.NewNodeRepository(database.Queries)
	entityRepo := repository.NewEntityRepository(database.Queries)
	predicateRepo := repository.NewPredicateRepository(database.Queries)
	assertionRepo := repository.NewAssertionRepository(database.Queries)
	eventRepo := repository.NewEventRepository(database.Queries)
	support := repository.NewSyntheticSupportRepository(database.Queries)

	// Insert-only river client (no started fetch loop) so AssertService.PublishTx
	// can enqueue assertion.* jobs; the cache is filled by the inline RefreshTx in
	// ContactService, and the async worker path is driven directly via HandleEvent.
	workers := river.NewWorkers()
	river.AddWorker(workers, &knowledgeCacheNoopWorker{})
	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues:   map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency}},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)
	bus := events.NewBus(database.Pool, client, eventRepo)

	assertSvc := service.NewAssertService(database.Pool, nodeRepo, entityRepo, predicateRepo, assertionRepo, bus)
	cacheUpdater := consumer.NewKnowledgeCacheUpdater(assertionRepo, nodeRepo, contactRepo)

	contactSvc := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, taskRepo, nil, nil,
		buildCadenceUpdaterForTest(t, database), assertSvc, cacheUpdater, nil)

	enrichmentRepo := repository.NewEnrichmentRepository(database.Queries)
	enrichSvc := service.NewEnrichmentService(database, contactRepo, methodRepo, enrichmentRepo, nil, nil,
		nil, assertSvc, cacheUpdater)

	migrationSvc := service.NewContactKnowledgeMigrationService(database.Pool, contactRepo, assertSvc)

	h := &knowledgeCutoverHarness{
		database:      database,
		contactSvc:    contactSvc,
		enrichSvc:     enrichSvc,
		assertSvc:     assertSvc,
		migrationSvc:  migrationSvc,
		cacheUpdater:  cacheUpdater,
		contactRepo:   contactRepo,
		assertionRepo: assertionRepo,
		nodeRepo:      nodeRepo,
		entityRepo:    entityRepo,
		eventRepo:     eventRepo,
		support:       support,
	}
	h.registerCleanup(t, ctx)
	return h
}

func (h *knowledgeCutoverHarness) track(id uuid.UUID) { h.contactIDs = append(h.contactIDs, id) }

func (h *knowledgeCutoverHarness) trackPlace(label string) {
	h.placeNorms = append(h.placeNorms, strings.ToLower(strings.TrimSpace(label)))
}

func (h *knowledgeCutoverHarness) registerCleanup(t *testing.T, ctx context.Context) {
	t.Cleanup(func() {
		for _, cid := range h.contactIDs {
			assertions, _ := h.assertionRepo.ListAssertionsBySubject(ctx, cid)
			for _, a := range assertions {
				_ = h.eventRepo.HardDeleteEventsBySourceAndSourceIDPrefix(ctx, "assertion", a.ID.String())
			}
			_, _ = h.support.DeleteAssertionsForNode(ctx, cid)
		}
		var placeNodeIDs []uuid.UUID
		for _, norm := range h.placeNorms {
			entity, err := h.entityRepo.FindEntityBySubtypeName(ctx, repository.EntitySubtypePlace, norm)
			if err == nil {
				placeNodeIDs = append(placeNodeIDs, entity.NodeID)
			}
		}
		_, _ = h.support.DeleteNodesByIds(ctx, placeNodeIDs)
		_, _ = h.support.DeleteNodesByIds(ctx, h.contactIDs)
		for _, cid := range h.contactIDs {
			_ = h.contactRepo.HardDeleteContact(ctx, cid)
		}
	})
}

// seedPreCutoverContact creates a contact + its person node (repo path, which no
// longer writes the cache columns) and then sets the cache columns directly,
// simulating a pre-cutover contact: cache populated, NO knowledge assertions.
func (h *knowledgeCutoverHarness) seedPreCutoverContact(t *testing.T, ctx context.Context, name string, location *string, birthday *time.Time) *repository.Contact {
	t.Helper()
	contact, err := h.contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: name})
	require.NoError(t, err)
	_, err = h.nodeRepo.CreateNode(ctx, contact.ID, repository.NodeTypePerson, name)
	require.NoError(t, err)
	err = pgx.BeginTxFunc(ctx, h.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if err := h.contactRepo.UpdateContactLocationCacheTx(ctx, tx, contact.ID, location); err != nil {
			return err
		}
		return h.contactRepo.UpdateContactBirthdayCacheTx(ctx, tx, contact.ID, birthday)
	})
	require.NoError(t, err)
	refetched, err := h.contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	return refetched
}

func TestContactKnowledgeCutover_CreateWritesAssertionsAndCache(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newKnowledgeCutoverHarness(t, ctx)

	ns := uuid.New().String()[:8]
	loc := "New York " + ns
	bday := time.Date(1990, 3, 14, 0, 0, 0, 0, time.UTC)
	how := "met at a conference " + ns
	h.trackPlace(loc)

	contact, _, err := h.contactSvc.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Knowledge Create " + ns,
		Location: &loc,
		Birthday: &bday,
		HowMet:   &how,
	}, nil)
	require.NoError(t, err)
	h.track(contact.ID)

	// The returned contact reflects the cache columns (filled inline on commit).
	require.NotNil(t, contact.Location)
	assert.Equal(t, loc, *contact.Location)
	require.NotNil(t, contact.Birthday)
	assert.Equal(t, bday, *contact.Birthday)
	require.NotNil(t, contact.HowMet)
	assert.Equal(t, how, *contact.HowMet)

	now := accelerated.GetCurrentTime().UTC()
	livesIn, err := h.assertionRepo.GetCurrentAccepted(ctx, contact.ID, repository.PredicateLivesIn, now)
	require.NoError(t, err)
	require.NotNil(t, livesIn.ObjectNodeID)
	place, err := h.nodeRepo.GetNode(ctx, *livesIn.ObjectNodeID)
	require.NoError(t, err)
	assert.Equal(t, loc, place.CanonicalLabel, "cache location == place node canonical_label")
	assertHasUserProvenance(t, ctx, h.assertionRepo, livesIn.ID)

	birthday, err := h.assertionRepo.GetCurrentAccepted(ctx, contact.ID, repository.PredicateBirthday, now)
	require.NoError(t, err)
	require.NotNil(t, birthday.ValueDate)
	assert.Equal(t, bday, *birthday.ValueDate)

	howMet, err := h.assertionRepo.GetCurrentAccepted(ctx, contact.ID, repository.PredicateHowMet, now)
	require.NoError(t, err)
	require.NotNil(t, howMet.ValueText)
	assert.Equal(t, how, *howMet.ValueText)
}

func TestContactKnowledgeCutover_UpdateSupersedesAndClears(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newKnowledgeCutoverHarness(t, ctx)

	ns := uuid.New().String()[:8]
	nyc := "NYC " + ns
	la := "LA " + ns
	h.trackPlace(nyc)
	h.trackPlace(la)

	contact, _, err := h.contactSvc.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Knowledge Update " + ns,
		Location: &nyc,
	}, nil)
	require.NoError(t, err)
	h.track(contact.ID)

	updated, err := h.contactSvc.UpdateContact(ctx, contact.ID, repository.UpdateContactRequest{
		FullName: contact.FullName,
		Location: &la,
	})
	require.NoError(t, err)
	require.NotNil(t, updated.Location)
	assert.Equal(t, la, *updated.Location, "cache location follows the supersession")

	now := accelerated.GetCurrentTime().UTC()
	current, err := h.assertionRepo.GetCurrentAccepted(ctx, contact.ID, repository.PredicateLivesIn, now)
	require.NoError(t, err)
	require.NotNil(t, current.ObjectNodeID)
	place, err := h.nodeRepo.GetNode(ctx, *current.ObjectNodeID)
	require.NoError(t, err)
	assert.Equal(t, la, place.CanonicalLabel)

	// Clear the location (closure): cache blanks to NULL.
	cleared, err := h.contactSvc.UpdateContact(ctx, contact.ID, repository.UpdateContactRequest{
		FullName: contact.FullName,
		Location: nil,
	})
	require.NoError(t, err)
	assert.Nil(t, cleared.Location, "clearing the field blanks the cache to NULL")

	_, err = h.assertionRepo.GetCurrentAccepted(ctx, contact.ID, repository.PredicateLivesIn, now)
	assert.ErrorIs(t, err, db.ErrNotFound, "no current lives_in after closure")
}

func TestContactKnowledgeCutover_AsyncWorkerRefreshesCache(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newKnowledgeCutoverHarness(t, ctx)

	ns := uuid.New().String()[:8]
	loc := "Seattle " + ns
	h.trackPlace(loc)

	contact, _, err := h.contactSvc.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Knowledge Async " + ns,
		Location: &loc,
	}, nil)
	require.NoError(t, err)
	h.track(contact.ID)

	// Corrupt the cache out-of-band, then drive the async consumer with a
	// synthetic accepted event for this contact's lives_in; the worker must
	// recompute the cache from the current-accepted assertion.
	err = pgx.BeginTxFunc(ctx, h.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		stale := "STALE"
		return h.contactRepo.UpdateContactLocationCacheTx(ctx, tx, contact.ID, &stale)
	})
	require.NoError(t, err)

	payload, err := events.Marshal(events.KindAssertionAccepted, events.AssertionEventPayload{
		Version:       1,
		AssertionID:   uuid.New(),
		SubjectNodeID: contact.ID,
		PredicateKey:  repository.PredicateLivesIn,
	})
	require.NoError(t, err)
	env := &events.Envelope{Source: "assertion", Kind: events.KindAssertionAccepted, Payload: payload}

	err = pgx.BeginTxFunc(ctx, h.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return h.cacheUpdater.HandleEvent(ctx, tx, env)
	})
	require.NoError(t, err)

	refetched, err := h.contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, refetched.Location)
	assert.Equal(t, loc, *refetched.Location, "async worker recomputed the cache from the assertion")
}

// TestContactKnowledgeCutover_AsyncWorkerBlanksCacheOnClosure proves the
// async stale-cache failure mode end-to-end: when a producer OTHER than the
// inline ContactService path closes the current-accepted assertion (so the cache
// column is left holding a stale value), driving the KnowledgeCacheUpdater worker
// on the assertion.superseded event recomputes from GetCurrentAccepted (which now
// returns nothing) and sets the cache column to NULL.
func TestContactKnowledgeCutover_AsyncWorkerBlanksCacheOnClosure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newKnowledgeCutoverHarness(t, ctx)

	ns := uuid.New().String()[:8]
	loc := "Miami " + ns
	h.trackPlace(loc)

	contact, _, err := h.contactSvc.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Async Blank " + ns,
		Location: &loc,
	}, nil)
	require.NoError(t, err)
	h.track(contact.ID)

	// Close the lives_in slot directly through AssertService (a "between jobs"
	// closure — the path SP3 extractors / agents use). AssertClosure publishes
	// assertion.superseded but does NOT touch the contact cache column, so the
	// cache is now STALE (still "Miami") while GetCurrentAccepted returns nothing.
	require.NoError(t, h.assertSvc.AssertClosure(ctx, service.ClosureRequest{
		SubjectNodeID: contact.ID,
		PredicateKey:  repository.PredicateLivesIn,
	}))
	stale, err := h.contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, stale.Location, "cache is intentionally stale until the async worker runs")
	assert.Equal(t, loc, *stale.Location)

	// Drive the async worker on the superseded event → recompute-from-scratch finds
	// no current lives_in → cache column set to NULL.
	payload, err := events.Marshal(events.KindAssertionSuperseded, events.AssertionEventPayload{
		Version:       1,
		AssertionID:   uuid.New(),
		SubjectNodeID: contact.ID,
		PredicateKey:  repository.PredicateLivesIn,
	})
	require.NoError(t, err)
	env := &events.Envelope{Source: "assertion", Kind: events.KindAssertionSuperseded, Payload: payload}
	err = pgx.BeginTxFunc(ctx, h.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return h.cacheUpdater.HandleEvent(ctx, tx, env)
	})
	require.NoError(t, err)

	blanked, err := h.contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.Nil(t, blanked.Location, "async worker blanks the cache to NULL when no current assertion remains")
}

func TestContactKnowledgeCutover_Backfill(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newKnowledgeCutoverHarness(t, ctx)

	ns := uuid.New().String()[:8]
	loc := "Boston " + ns
	bday := time.Date(1985, 7, 1, 0, 0, 0, 0, time.UTC)
	h.trackPlace(loc)

	contact := h.seedPreCutoverContact(t, ctx, "Knowledge Backfill "+ns, &loc, &bday)
	h.track(contact.ID)

	now := accelerated.GetCurrentTime().UTC()
	_, err := h.assertionRepo.GetCurrentAccepted(ctx, contact.ID, repository.PredicateLivesIn, now)
	require.ErrorIs(t, err, db.ErrNotFound, "no knowledge assertions before the backfill")

	res, err := h.migrationSvc.MigrateContactKnowledgeColumns(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, res.Contacts, 1)

	livesIn, err := h.assertionRepo.GetCurrentAccepted(ctx, contact.ID, repository.PredicateLivesIn, now)
	require.NoError(t, err)
	assert.WithinDuration(t, contact.CreatedAt, livesIn.KnowledgeFrom, time.Second, "knowledge_from preserved from contact.created_at")
	require.NotNil(t, livesIn.ObjectNodeID)
	place, err := h.nodeRepo.GetNode(ctx, *livesIn.ObjectNodeID)
	require.NoError(t, err)
	assert.Equal(t, loc, place.CanonicalLabel)
	assertHasUserProvenance(t, ctx, h.assertionRepo, livesIn.ID)

	birthday, err := h.assertionRepo.GetCurrentAccepted(ctx, contact.ID, repository.PredicateBirthday, now)
	require.NoError(t, err)
	require.NotNil(t, birthday.ValueDate)
	assert.Equal(t, bday, *birthday.ValueDate)

	// Re-run is idempotent: still exactly one current lives_in for this contact.
	_, err = h.migrationSvc.MigrateContactKnowledgeColumns(ctx)
	require.NoError(t, err)
	assertions, err := h.assertionRepo.ListAssertionsBySubject(ctx, contact.ID)
	require.NoError(t, err)
	liveLivesIn := 0
	for _, a := range assertions {
		if a.PredicateKey == repository.PredicateLivesIn && a.Status == repository.AssertionStatusAccepted && a.KnowledgeTo == nil {
			liveLivesIn++
		}
	}
	assert.Equal(t, 1, liveLivesIn, "re-run does not duplicate the live lives_in assertion")
}

func TestContactKnowledgeCutover_BackfillSkipsSoftDeleted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newKnowledgeCutoverHarness(t, ctx)

	ns := uuid.New().String()[:8]
	loc := "Chicago " + ns
	h.trackPlace(loc)

	contact := h.seedPreCutoverContact(t, ctx, "Knowledge Deleted "+ns, &loc, nil)
	h.track(contact.ID)
	require.NoError(t, h.contactRepo.SoftDeleteContact(ctx, contact.ID))
	require.NoError(t, h.nodeRepo.SoftDeleteNode(ctx, contact.ID))

	_, err := h.migrationSvc.MigrateContactKnowledgeColumns(ctx)
	require.NoError(t, err)

	assertions, err := h.assertionRepo.ListAssertionsBySubject(ctx, contact.ID)
	require.NoError(t, err)
	assert.Empty(t, assertions, "a soft-deleted contact's knowledge columns are not promoted")
}

func TestContactKnowledgeCutover_SortStillWorks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newKnowledgeCutoverHarness(t, ctx)

	ns := uuid.New().String()[:8]
	type seed struct{ name, loc string }
	seeds := []seed{
		{"Sort A " + ns, "Aville " + ns},
		{"Sort B " + ns, "Bville " + ns},
		{"Sort C " + ns, "Cville " + ns},
	}
	ids := make(map[uuid.UUID]string)
	for _, s := range seeds {
		loc := s.loc
		h.trackPlace(loc)
		c, _, err := h.contactSvc.CreateContact(ctx, repository.CreateContactRequest{
			FullName: s.name,
			Location: &loc,
		}, nil)
		require.NoError(t, err)
		h.track(c.ID)
		ids[c.ID] = loc
	}

	contacts, err := h.contactRepo.ListContacts(ctx, repository.ListContactsParams{
		Limit: 1000, Offset: 0, Sort: "location", Order: "asc",
	})
	require.NoError(t, err)

	var ordered []string
	for _, c := range contacts {
		if loc, ok := ids[c.ID]; ok {
			require.NotNil(t, c.Location, "cache location populated for sorted read")
			assert.Equal(t, loc, *c.Location)
			ordered = append(ordered, *c.Location)
		}
	}
	require.Len(t, ordered, len(seeds))
	assert.Equal(t, []string{"Aville " + ns, "Bville " + ns, "Cville " + ns}, ordered, "location sort order intact")
}

func TestContactKnowledgeCutover_ListAndSearchReadCache(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newKnowledgeCutoverHarness(t, ctx)

	ns := uuid.New().String()[:8]
	loc := "Denver " + ns
	bday := time.Date(1988, 11, 9, 0, 0, 0, 0, time.UTC)
	h.trackPlace(loc)

	c, _, err := h.contactSvc.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Readback Person " + ns,
		Location: &loc,
		Birthday: &bday,
	}, nil)
	require.NoError(t, err)
	h.track(c.ID)

	// Birthday-sorted list still returns the cache value (the sort query reads the
	// unchanged cache column).
	byBirthday, err := h.contactRepo.ListContacts(ctx, repository.ListContactsParams{
		Limit: 1000, Offset: 0, Sort: "birthday", Order: "asc",
	})
	require.NoError(t, err)
	foundInList := false
	for _, lc := range byBirthday {
		if lc.ID == c.ID {
			foundInList = true
			require.NotNil(t, lc.Birthday)
			assert.Equal(t, bday, *lc.Birthday, "birthday cache returned in list")
			require.NotNil(t, lc.Location)
			assert.Equal(t, loc, *lc.Location, "location cache returned in list")
		}
	}
	assert.True(t, foundInList, "contact present in birthday-sorted list")

	// Search by name still returns the contact with its cache columns populated.
	results, err := h.contactRepo.ListContacts(ctx, repository.ListContactsParams{
		Query: "Readback Person " + ns, Limit: 1000, Offset: 0,
	})
	require.NoError(t, err)
	foundInSearch := false
	for _, sc := range results {
		if sc.ID == c.ID {
			foundInSearch = true
			require.NotNil(t, sc.Location)
			assert.Equal(t, loc, *sc.Location, "location cache returned in search")
		}
	}
	assert.True(t, foundInSearch, "contact found by name search with cache populated")
}

func TestContactKnowledgeCutover_EnrichmentEmitsAssertionAndCache(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newKnowledgeCutoverHarness(t, ctx)

	ns := uuid.New().String()[:8]
	loc := "Austin " + ns
	bday := time.Date(1979, 2, 17, 0, 0, 0, 0, time.UTC)
	h.trackPlace(loc)

	// A contact with NO location/birthday (so enrichment fills both).
	contact, _, err := h.contactSvc.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Enrich Person " + ns,
	}, nil)
	require.NoError(t, err)
	h.track(contact.ID)

	external := &repository.ExternalContact{
		Birthday:  &bday,
		Addresses: []repository.AddressEntry{{Formatted: loc}},
	}
	_, err = h.enrichSvc.EnrichContactFromExternal(ctx, contact.ID, external)
	require.NoError(t, err)

	// Cache columns now reflect the inferred values (filled inline).
	refetched, err := h.contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, refetched.Location)
	assert.Equal(t, loc, *refetched.Location)
	require.NotNil(t, refetched.Birthday)
	assert.Equal(t, bday, *refetched.Birthday)

	// The lives_in assertion carries AGENT provenance (inferred, not a user edit).
	now := accelerated.GetCurrentTime().UTC()
	livesIn, err := h.assertionRepo.GetCurrentAccepted(ctx, contact.ID, repository.PredicateLivesIn, now)
	require.NoError(t, err)
	prov, err := h.assertionRepo.ListProvenance(ctx, livesIn.ID)
	require.NoError(t, err)
	require.NotEmpty(t, prov)
	foundAgent := false
	for _, p := range prov {
		if p.ProducerKind == repository.ProducerKindAgent && p.SourceKind == repository.SourceKindAgentSession {
			foundAgent = true
		}
	}
	assert.True(t, foundAgent, "enrichment-inferred assertion carries agent provenance")
}

func TestContactKnowledgeCutover_EmptyFieldNormalization(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newKnowledgeCutoverHarness(t, ctx)

	ns := uuid.New().String()[:8]
	blank := "   " // whitespace-only

	// Create with whitespace-only location/how_met → no assertion, no error, cache
	// stays empty (the pre-cutover leniency for blank fields).
	contact, _, err := h.contactSvc.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Empty Field " + ns,
		Location: &blank,
		HowMet:   &blank,
	}, nil)
	require.NoError(t, err, "whitespace-only fields must not error the create")
	h.track(contact.ID)
	assert.Nil(t, contact.Location, "blank location is not stored")
	assert.Nil(t, contact.HowMet, "blank how_met is not stored")

	now := accelerated.GetCurrentTime().UTC()
	_, err = h.assertionRepo.GetCurrentAccepted(ctx, contact.ID, repository.PredicateLivesIn, now)
	assert.ErrorIs(t, err, db.ErrNotFound, "no lives_in assertion minted for a blank location")

	// Now set a real location, then update with an empty string → treated as a
	// CLEAR (closure), cache blanks to NULL.
	realLoc := "Phoenix " + ns
	h.trackPlace(realLoc)
	_, err = h.contactSvc.UpdateContact(ctx, contact.ID, repository.UpdateContactRequest{
		FullName: contact.FullName,
		Location: &realLoc,
	})
	require.NoError(t, err)
	withLoc, err := h.contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, withLoc.Location)
	assert.Equal(t, realLoc, *withLoc.Location)

	empty := ""
	cleared, err := h.contactSvc.UpdateContact(ctx, contact.ID, repository.UpdateContactRequest{
		FullName: contact.FullName,
		Location: &empty,
	})
	require.NoError(t, err, "empty-string location must not error")
	assert.Nil(t, cleared.Location, "empty-string update clears the location cache")
	_, err = h.assertionRepo.GetCurrentAccepted(ctx, contact.ID, repository.PredicateLivesIn, now)
	assert.ErrorIs(t, err, db.ErrNotFound, "empty-string update closes the lives_in slot")
}

func TestContactKnowledgeCutover_HowMetPreservedWhenFormOmitsIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newKnowledgeCutoverHarness(t, ctx)

	ns := uuid.New().String()[:8]
	how := "met through a mutual friend " + ns
	loc := "Reno " + ns
	h.trackPlace(loc)

	// Create a contact WITH how_met.
	contact, _, err := h.contactSvc.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "HowMet Keep " + ns,
		HowMet:   &how,
	}, nil)
	require.NoError(t, err)
	h.track(contact.ID)

	now := accelerated.GetCurrentTime().UTC()
	before, err := h.assertionRepo.GetCurrentAccepted(ctx, contact.ID, repository.PredicateHowMet, now)
	require.NoError(t, err)

	// Edit the contact the way the form does — how_met ABSENT (nil), other fields
	// set. how_met must be UNTOUCHED: no closure, the SAME assertion stays current,
	// and the cache column still holds the value. (The form has no how_met input,
	// so a nil how_met is "not managed by this edit," not a user clear.)
	updated, err := h.contactSvc.UpdateContact(ctx, contact.ID, repository.UpdateContactRequest{
		FullName: "HowMet Keep Renamed " + ns,
		Location: &loc, // an unrelated field changes
		HowMet:   nil,  // the form omits how_met
	})
	require.NoError(t, err)
	require.NotNil(t, updated.HowMet, "how_met cache must survive an edit that omits it")
	assert.Equal(t, how, *updated.HowMet)

	after, err := h.assertionRepo.GetCurrentAccepted(ctx, contact.ID, repository.PredicateHowMet, now)
	require.NoError(t, err)
	assert.Equal(t, before.ID, after.ID, "the SAME how_met assertion stays current (no closure/supersession churn)")
	assert.Equal(t, repository.AssertionStatusAccepted, after.Status)
	assert.Nil(t, after.KnowledgeTo, "how_met assertion is not closed by an omitting edit")

	// Sanity: an EXPLICIT how_met value still asserts (changes the slot).
	newHow := "actually met at work " + ns
	_, err = h.contactSvc.UpdateContact(ctx, contact.ID, repository.UpdateContactRequest{
		FullName: updated.FullName,
		HowMet:   &newHow,
	})
	require.NoError(t, err)
	current, err := h.assertionRepo.GetCurrentAccepted(ctx, contact.ID, repository.PredicateHowMet, now)
	require.NoError(t, err)
	require.NotNil(t, current.ValueText)
	assert.Equal(t, newHow, *current.ValueText, "an explicit how_met value still updates the slot")
}

func TestContactKnowledgeCutover_EnrichmentErrorPropagates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newKnowledgeCutoverHarness(t, ctx)

	ns := uuid.New().String()[:8]
	loc := "Tucson " + ns
	h.trackPlace(loc)

	// Seed a contact whose PERSON NODE is soft-deleted: the assert path rejects a
	// soft-deleted subject node, so the inferred-knowledge write fails — and the
	// enrichment must surface that as an error (not log-and-continue) since the
	// inferred location would otherwise be silently dropped.
	contact := h.seedPreCutoverContact(t, ctx, "Enrich Fail "+ns, nil, nil)
	h.track(contact.ID)
	require.NoError(t, h.nodeRepo.SoftDeleteNode(ctx, contact.ID))

	external := &repository.ExternalContact{
		Addresses: []repository.AddressEntry{{Formatted: loc}},
	}
	_, err := h.enrichSvc.EnrichContactFromExternal(ctx, contact.ID, external)
	require.Error(t, err, "a dropped inferred location must surface as an error, not a silent success")
}

// assertHasUserProvenance asserts the assertion has at least one user-producer
// provenance locator.
func assertHasUserProvenance(t *testing.T, ctx context.Context, assertionRepo *repository.AssertionRepository, assertionID uuid.UUID) {
	t.Helper()
	prov, err := assertionRepo.ListProvenance(ctx, assertionID)
	require.NoError(t, err)
	require.NotEmpty(t, prov)
	foundUser := false
	for _, p := range prov {
		if p.ProducerKind == repository.ProducerKindUser && p.SourceKind == repository.SourceKindUser {
			foundUser = true
		}
	}
	assert.True(t, foundUser, "assertion carries a user-producer provenance locator")
}
