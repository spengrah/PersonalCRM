// Integration coverage for the in-sync gmail_correspondence DISCOVERY hook.
// Drives the REAL GmailSyncProvider.Sync with a real *events.Bus + database.Pool
// + a FAKE gmailFetcher (no OAuth/HTTP) and a real CorrespondenceDiscoverer
// wired via SetCorrespondenceDiscoverer, against the shared test DB. Proves:
//   - DISCOVERY RUNS BETWEEN FETCH AND STORAGE (the key regression): a fetched
//     multi-party message that does NOT pass the storage gate (so it is never
//     stored in comms_message) STILL yields a gmail_correspondence candidate,
//     with the suggested match recomputed from the Cc display name and the
//     co-occurring-contact evidence drawn from the KNOWN contact on the message
//     (never the suggested match);
//   - linking a produced candidate adds the email as a contact_method and
//     dispatches the KindContactMethodsAdded rematch (the inherited backfill
//     hand-off);
//   - a discovery error is NON-FATAL to the sync sweep: Sync returns no error,
//     the cursor advances, the storage-gate-passing message IS stored, and the
//     candidate is NOT upserted (discovery failed, logged not propagated).
//
// All seeding goes through repositories (sqlc-only); addresses/names are
// per-test-suffixed placeholders; times use accelerated.GetCurrentTime().
package tests

import (
	"context"
	"errors"
	"testing"
	"time"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/matching"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
	gmailapi "google.golang.org/api/gmail/v1"
)

// discoveryEnv bundles a real provider + bus + repos for the discovery tests.
type discoveryEnv struct {
	ctx            context.Context
	database       *db.Database
	gen            *factory.Generator
	provider       *google.GmailSyncProvider
	commsRepo      *repository.CommsMessageRepository
	contactRepo    *repository.ContactRepository
	methodRepo     *repository.ContactMethodRepository
	contactService *service.ContactService
	externalRepo   *repository.ExternalContactRepository
	eventRepo      *repository.EventRepository
	matchService   *service.ImportMatchService
}

func newDiscoveryEnv(t *testing.T) *discoveryEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	// Per-test isolated clone: the live consumer drains a private river_job.
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	commsRepo := repository.NewCommsMessageRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	methodRepo := repository.NewContactMethodRepository(database.Queries)
	externalRepo := repository.NewExternalContactRepository(database.Queries)
	eventRepo := repository.NewEventRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	contactService := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, contactTaskRepo, nil, nil)

	// Sync early-returns when bus == nil, so wire a live bus + pool (the shared
	// harness). email.* kinds are drained by the harness's no-op email worker.
	bus := setupTestEventBus(t, ctx, database, contactService)

	provider := google.NewGmailSyncProvider(nil, commsRepo, bus, database.Pool)

	gen, _ := migrationGenerator(t)

	return &discoveryEnv{
		ctx:            ctx,
		database:       database,
		gen:            gen,
		provider:       provider,
		commsRepo:      commsRepo,
		contactRepo:    contactRepo,
		methodRepo:     methodRepo,
		contactService: contactService,
		externalRepo:   externalRepo,
		eventRepo:      eventRepo,
		matchService:   service.NewImportMatchService(contactRepo),
	}
}

// newKnownContact seeds a namespaced contact carrying one email method (so it is
// a KNOWN contact in the sync's knownMap) and returns it plus that email.
func (e *discoveryEnv) newKnownContact(t *testing.T) (*repository.Contact, string) {
	t.Helper()
	spec := e.gen.Contact(factory.WithEmail())
	return e.seedSpec(t, spec), spec.Email
}

// seedContactNoMethod seeds a CRM contact with NO email method (so it is only
// trigram-matchable by the given name, never in the known-address set). The name
// is caller-supplied because the discovery tests cross-reference it against a
// message's Cc display name.
func (e *discoveryEnv) seedContactNoMethod(t *testing.T, name string) *repository.Contact {
	t.Helper()
	return e.seedSpec(t, factory.ContactSpec{FullName: name})
}

// seedSpec writes a factory ContactSpec through the env's nil-bus
// ContactService.CreateContact (single-tx multi-method write, no River client)
// and registers content-then-contact cleanup.
func (e *discoveryEnv) seedSpec(t *testing.T, spec factory.ContactSpec) *repository.Contact {
	t.Helper()
	methods := make([]service.ContactMethodInput, 0, len(spec.Methods))
	for _, m := range spec.Methods {
		methods = append(methods, service.ContactMethodInput{Type: m.Type, Value: m.Value, IsPrimary: m.IsPrimary})
	}
	contact, _, err := e.contactService.CreateContact(e.ctx, repository.CreateContactRequest{
		FullName: spec.FullName,
		Cadence:  spec.Cadence,
	}, methods)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = e.commsRepo.HardDeleteByContact(e.ctx, contact.ID)
		_ = e.contactRepo.HardDeleteContact(e.ctx, contact.ID)
	})
	return contact
}

// wireDiscoverer attaches a real CorrespondenceDiscoverer to the provider.
func (e *discoveryEnv) wireDiscoverer() {
	e.provider.SetCorrespondenceDiscoverer(google.NewCorrespondenceDiscoverer(e.contactRepo, e.externalRepo))
}

// cleanupExternal hard-deletes a produced candidate so the shared DB does not
// accumulate gmail_correspondence rows across runs.
func (e *discoveryEnv) cleanupExternal(t *testing.T, sourceID string) {
	t.Helper()
	t.Cleanup(func() {
		row, _ := e.externalRepo.GetBySource(e.ctx, google.CorrespondenceSource, sourceID, nil)
		if row != nil {
			_ = e.externalRepo.Delete(e.ctx, row.ID)
		}
	})
}

// cleanupEvents hard-deletes durable email.* event rows produced by a sweep.
func (e *discoveryEnv) cleanupEvents(t *testing.T, externalIDPrefix string) {
	t.Helper()
	t.Cleanup(func() {
		_ = e.eventRepo.HardDeleteEventsBySourceAndSourceIDPrefix(e.ctx, "email", externalIDPrefix+"%")
	})
}

// discoverySyncState builds a sync state at the legacy backfill floor so the
// windowed sync reaches a 2023-era seeded message in its first pass (the same
// floor + epoch the provider sweep tests use).
func discoverySyncState(accountID string) *repository.SyncState {
	return &repository.SyncState{
		Source:    "email",
		AccountID: &accountID,
		Metadata:  map[string]any{"backfill_since": "2023-11-01"},
	}
}

// 1. KEY REGRESSION: discovery runs between fetch and storage.
func TestDiscovery_RunsBetweenFetchAndStorage(t *testing.T) {
	t.Parallel()
	e := newDiscoveryEnv(t)
	e.wireDiscoverer()
	prefix := e.gen.Prefix()

	me := prefix + "me@synthetic.example"
	other := prefix + "peer-other@synthetic.example" // non-own, non-contact recipient
	patAddr := prefix + "peer-pat@synthetic.example" // unknown Cc → candidate
	patNorm := matching.NormalizeEmail(patAddr)
	patName := prefix + "Pat Carter"

	contactA, addrA := e.newKnownContact(t)       // KNOWN contact A (has method)
	contactB := e.seedContactNoMethod(t, patName) // trigram-matched by name only
	e.cleanupExternal(t, patNorm)
	e.cleanupEvents(t, prefix+"disc")

	// From=A (known), To=other (non-own non-contact), Cc=Pat (unknown, ≥2-token
	// name matching contact B). The storage gate drops this: for A, inbound needs
	// own-account ∈ recipients (false — only `other`+Pat are recipients);
	// outbound needs sender==own (false — sender is A). So zero qualifiedRows.
	msg := gmailMsg("g-disc-"+prefix, "thr-"+prefix, "Known Alpha <"+addrA+">",
		[]string{other}, []string{patName + " <" + patAddr + ">"}, nil,
		"Subj", "body", "<"+prefix+"disc@synthetic.example>", 1700000100000)
	e.provider.SetFetcherFactoryForTest(google.NewFakeGmailFetcherFactoryForTest(newFakeMessageStore([]*gmailapi.Message{msg}).fetcherFuncs()))
	e.provider.SetMeSetForTest(map[string]struct{}{me: {}})

	result, err := e.provider.Sync(e.ctx, discoverySyncState(me), nil)
	require.NoError(t, err)

	// (a) The storage gate dropped the message — zero comms_message rows for A.
	require.Equal(t, 0, result.ItemsMatched, "storage gate must store nothing for this message")
	aRows, err := e.commsRepo.ListByContact(e.ctx, contactA.ID)
	require.NoError(t, err)
	require.Empty(t, aRows, "no comms_message row stored (message did not pass the storage gate)")

	// (b) Discovery still produced a gmail_correspondence candidate for the Cc.
	row, err := e.externalRepo.GetBySource(e.ctx, google.CorrespondenceSource, patNorm, nil)
	require.NoError(t, err)
	require.NotNil(t, row, "discovery must surface a candidate from the fetched (unstored) message")
	require.Equal(t, repository.MatchStatusUnmatched, row.MatchStatus)
	require.NotNil(t, row.DisplayName)
	require.Equal(t, patName, *row.DisplayName)

	// (c) Suggested match recomputed from display_name → contact B.
	match, err := e.matchService.FindBestMatch(e.ctx, row)
	require.NoError(t, err)
	require.NotNil(t, match, "suggested match recomputed from display_name")
	require.Equal(t, contactB.ID.String(), match.ContactID, "suggested match is the name-matched contact B")

	// (d) Co-occurring-contact evidence is the KNOWN contact A (on From), NOT B.
	co, ok := row.Metadata["co_occurring_contact"].(map[string]any)
	require.True(t, ok, "co_occurring_contact evidence present")
	require.Equal(t, contactA.ID.String(), co["id"], "evidence is the known co-occurring contact A")
	require.NotEqual(t, contactB.ID.String(), co["id"], "evidence must NOT be the suggested match B")
}

// 2. Link a produced candidate → method added → rematch dispatched.
func TestDiscovery_LinkAddsMethodAndDispatchesRematch(t *testing.T) {
	t.Parallel()
	e := newDiscoveryEnv(t)
	e.wireDiscoverer()
	prefix := e.gen.Prefix()

	me := prefix + "me@synthetic.example"
	other := prefix + "peer-other@synthetic.example"
	patAddr := prefix + "peer-pat@synthetic.example"
	patNorm := matching.NormalizeEmail(patAddr)
	patName := prefix + "Pat Linker"

	_, addrA := e.newKnownContact(t)
	contactB := e.seedContactNoMethod(t, patName)
	e.cleanupExternal(t, patNorm)
	e.cleanupEvents(t, prefix+"disclink")

	msg := gmailMsg("g-disclink-"+prefix, "thr-"+prefix, "Known Beta <"+addrA+">",
		[]string{other}, []string{patName + " <" + patAddr + ">"}, nil,
		"Subj", "body", "<"+prefix+"disclink@synthetic.example>", 1700000100000)
	e.provider.SetFetcherFactoryForTest(google.NewFakeGmailFetcherFactoryForTest(newFakeMessageStore([]*gmailapi.Message{msg}).fetcherFuncs()))
	e.provider.SetMeSetForTest(map[string]struct{}{me: {}})

	_, err := e.provider.Sync(e.ctx, discoverySyncState(me), nil)
	require.NoError(t, err)
	row, err := e.externalRepo.GetBySource(e.ctx, google.CorrespondenceSource, patNorm, nil)
	require.NoError(t, err)
	require.NotNil(t, row, "discovery produced the candidate to link")

	// Live River client + event bus so the link publishes KindContactMethodsAdded
	// and enqueues a RematchDispatcher job.
	workers := river.NewWorkers()
	river.AddWorker(workers, &discoveryDispatcherNoopWorker{})
	riverClient, err := river.NewClient(riverpgxv5.New(e.database.Pool), &river.Config{
		Queues:   map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)
	require.NoError(t, riverClient.Start(e.ctx))
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = riverClient.Stop(stopCtx)
	})

	bus := events.NewBus(e.database.Pool, riverClient, e.eventRepo)
	rematchSvc := service.NewRematchService()
	rematchSvc.Register(discoveryEmailEligibleHandler{})
	enrichmentRepo := repository.NewEnrichmentRepository(e.database.Queries)
	enrichment := service.NewEnrichmentService(e.database, e.contactRepo, e.methodRepo, enrichmentRepo, bus, rematchSvc)

	jobID, err := enrichment.EnrichContactFromExternalWithSelections(
		e.ctx, contactB.ID, row,
		[]service.MethodSelection{{OriginalValue: patNorm, Type: "email"}},
		nil, nil, nil,
	)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, jobID, "email handler eligible → a rematch job id is returned")

	// (a) the email is now a contact_method on the linked contact B.
	methods, err := e.methodRepo.ListContactMethodsByContact(e.ctx, contactB.ID)
	require.NoError(t, err)
	found := false
	for _, m := range methods {
		if m.Value == patNorm && m.Type == "email" {
			found = true
		}
	}
	require.True(t, found, "linked email must be added as a contact_method")

	// (b) the rematch was dispatched: a contact_methods.added event + a
	// RematchDispatcher job for (contact, jobID).
	count, err := e.eventRepo.CountRematchDispatcherJobs(e.ctx, contactB.ID, jobID)
	require.NoError(t, err)
	require.Equal(t, int64(1), count, "exactly one rematch dispatcher job enqueued")
}

// 3. Discovery error is NON-FATAL to the sync sweep: a failing discovery upsert
// must not error Sync, rewind the cursor, or strand a storable comms_message
// row. Drive the failure via a fake external repo whose Upsert errors for the
// qualifying address.
func TestDiscovery_ErrorNonFatalToSync(t *testing.T) {
	t.Parallel()
	e := newDiscoveryEnv(t)
	prefix := e.gen.Prefix()

	me := prefix + "me@synthetic.example"
	other := prefix + "peer-other@synthetic.example"
	patAddr := prefix + "peer-pat@synthetic.example"
	patNorm := matching.NormalizeEmail(patAddr)
	patName := prefix + "Pat Failer"

	contactA, addrA := e.newKnownContact(t)
	_ = e.seedContactNoMethod(t, patName)
	e.cleanupExternal(t, patNorm)
	e.cleanupEvents(t, prefix+"discfail")
	e.cleanupEvents(t, prefix+"discstore")

	// Discoverer whose external Upsert errors for the qualifying Cc address.
	failExt := &failingUpsertExternal{
		inner:  e.externalRepo,
		failOn: map[string]struct{}{patNorm: {}},
	}
	e.provider.SetCorrespondenceDiscoverer(google.NewCorrespondenceDiscoverer(e.contactRepo, failExt))

	// Message 1: the discovery-triggering multi-party message (storage gate misses).
	discMsg := gmailMsg("g-discfail-"+prefix, "thr-f-"+prefix, "Known Gamma <"+addrA+">",
		[]string{other}, []string{patName + " <" + patAddr + ">"}, nil,
		"Subj", "body", "<"+prefix+"discfail@synthetic.example>", 1700000100000)
	// Message 2: a clean you↔contact message that DOES pass the storage gate
	// (inbound: A → me), so we can assert ingest is not stranded by the
	// discovery failure.
	storeMsg := gmailMsg("g-discstore-"+prefix, "thr-s-"+prefix, "Known Gamma <"+addrA+">",
		[]string{me}, nil, nil,
		"Stored", "stored body", "<"+prefix+"discstore@synthetic.example>", 1700000200000)

	e.provider.SetFetcherFactoryForTest(google.NewFakeGmailFetcherFactoryForTest(
		newFakeMessageStore([]*gmailapi.Message{discMsg, storeMsg}).fetcherFuncs()))
	e.provider.SetMeSetForTest(map[string]struct{}{me: {}})

	result, err := e.provider.Sync(e.ctx, discoverySyncState(me), nil)

	// (a) Sync returns no error despite the discovery upsert failure.
	require.NoError(t, err, "a discovery error must be logged, not returned from Sync")
	// (b) the cursor advanced (a fresh v2 cursor, not the blank prior).
	require.NotEmpty(t, result.NewCursor, "cursor must advance — discovery failure must not rewind it")
	// (c) the storage-gate-passing message IS persisted.
	require.GreaterOrEqual(t, result.ItemsMatched, 1, "the clean you↔contact message must still be stored")
	aRows, err := e.commsRepo.ListByContact(e.ctx, contactA.ID)
	require.NoError(t, err)
	require.NotEmpty(t, aRows, "the clean inbound message is stored despite the discovery failure")
	// (d) the candidate is NOT upserted (discovery failed for it).
	row, err := e.externalRepo.GetBySource(e.ctx, google.CorrespondenceSource, patNorm, nil)
	require.NoError(t, err)
	require.Nil(t, row, "the failing discovery upsert produced no candidate")
}

// failingUpsertExternal wraps the real external repo but errors on Upsert for a
// set of source_ids — the discovery failure seam (no DB error injection needed).
type failingUpsertExternal struct {
	inner  *repository.ExternalContactRepository
	failOn map[string]struct{}
}

func (f *failingUpsertExternal) GetBySource(ctx context.Context, source, sourceID string, accountID *string) (*repository.ExternalContact, error) {
	return f.inner.GetBySource(ctx, source, sourceID, accountID)
}

func (f *failingUpsertExternal) Upsert(ctx context.Context, req repository.UpsertExternalContactRequest) (*repository.ExternalContact, error) {
	if _, bad := f.failOn[req.SourceID]; bad {
		return nil, errors.New("forced discovery upsert failure")
	}
	return f.inner.Upsert(ctx, req)
}

// discoveryEmailEligibleHandler makes the "email" method type eligible so the
// enrichment link publishes KindContactMethodsAdded.
type discoveryEmailEligibleHandler struct{}

func (discoveryEmailEligibleHandler) IdentifierType() string { return "email" }
func (discoveryEmailEligibleHandler) Rematch(context.Context, uuid.UUID, string) (int, error) {
	return 0, nil
}

// discoveryDispatcherNoopWorker satisfies River's registered-kind rule so the
// live client accepts RematchDispatcher inserts; we assert row counts, not
// execution.
type discoveryDispatcherNoopWorker struct {
	river.WorkerDefaults[discoveryDispatcherNoopArgs]
}

func (*discoveryDispatcherNoopWorker) Work(context.Context, *river.Job[discoveryDispatcherNoopArgs]) error {
	return nil
}

type discoveryDispatcherNoopArgs struct{}

func (discoveryDispatcherNoopArgs) Kind() string { return "rematch_dispatcher" }
