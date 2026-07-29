//go:build integration_testdb

package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic/declare"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
)

// Shared support for the declared-seeding integration suite.
//
// ISOLATION: every test here runs on its OWN clone (newIsolatedRiverTestDB).
// declare.Run starts a live River client, and namespace scoping does not
// isolate river_job CONSUMPTION — a client draining on a shared database steals
// sibling tests' jobs. None of these call t.Parallel() for the same reason;
// this is the case the testing rules sanction the clone for.
//
// The suite deliberately does NOT set CRM_ENV. Under the ambient (production)
// cadence table a week is a week, so "overdue by three days" is three days
// rather than fifty seconds, and the assertions have no timing race in them.

// declareNS mints a fresh namespace token for one test. It stays inside the
// toolkit charset and never ends in the reserved -sN salt suffix.
func declareNS(t *testing.T) string {
	t.Helper()
	return "d" + uuid.NewString()[:8]
}

// declareTestDB opens a per-test clone.
func declareTestDB(t *testing.T) (*db.Database, context.Context) {
	t.Helper()
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)
	return database, ctx
}

// --- API read path ----------------------------------------------------------

// declareKnowledgeCacheNoopWorker registers the knowledge_cache_updater kind so
// the assert bus accepts the enqueues a contact-create authority flip produces.
// This client is constructed but never STARTED — the read router publishes
// nothing; it exists only so the bus can be built.
type declareKnowledgeCacheNoopWorker struct {
	river.WorkerDefaults[consumerjobs.KnowledgeCacheUpdaterJobArgs]
}

func (*declareKnowledgeCacheNoopWorker) Work(_ context.Context, _ *river.Job[consumerjobs.KnowledgeCacheUpdaterJobArgs]) error {
	return nil
}

// newDeclareReadRouter builds a router carrying the PRODUCTION contact read
// surface. Postconditions are asserted through it rather than by querying
// tables directly: a fixture that exists in the database but does not reach the
// API is not a usable fixture.
func newDeclareReadRouter(t *testing.T, database *db.Database) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	methodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	claimRepo := repository.NewEventConsumerClaimRepository(database.Queries)

	workers := river.NewWorkers()
	river.AddWorker(workers, &declareKnowledgeCacheNoopWorker{})
	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues:   map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)
	bus := events.NewBus(database.Pool, client, repository.NewEventRepository(database.Queries))

	assertSvc := service.NewAssertService(
		database.Pool,
		repository.NewNodeRepository(database.Queries),
		repository.NewEntityRepository(database.Queries),
		repository.NewPredicateRepository(database.Queries),
		repository.NewAssertionRepository(database.Queries),
		bus,
	)
	cache := consumer.NewKnowledgeCacheUpdater(
		repository.NewAssertionRepository(database.Queries),
		repository.NewNodeRepository(database.Queries),
		contactRepo,
	)
	cadenceUpdater := consumer.NewCadenceUpdater(claimRepo, contactRepo, database.Queries, consumer.CadenceModeCutover, false)

	contactService := service.NewContactService(
		database, contactRepo, methodRepo, interactionRepo,
		repository.NewContactTaskRepository(database.Queries), nil, nil,
		cadenceUpdater, assertSvc, cache, nil,
	)

	router := gin.New()
	v1 := router.Group("/api/v1")
	handlers.RegisterContactRoutes(v1, handlers.ContactRouteDeps{
		Contact:     handlers.NewContactHandler(contactService),
		Interaction: handlers.NewInteractionHandler(interactionRepo, nil),
		Note: handlers.NewNoteHandler(service.NewNoteService(
			repository.NewNoteRepository(database.Queries), contactRepo)),
	})
	return router
}

// getJSON drives a GET through the production router and decodes `data` into out.
func getJSON(t *testing.T, router *gin.Engine, url string, out any) {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, url, nil))
	require.Equal(t, http.StatusOK, w.Code, "GET %s → %d: %s", url, w.Code, w.Body.String())

	var envelope struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &envelope))
	require.True(t, envelope.Success)
	require.NoError(t, json.Unmarshal(envelope.Data, out))
}

func listContacts(t *testing.T, router *gin.Engine, search string) []handlers.ContactResponse {
	t.Helper()
	var out []handlers.ContactResponse
	getJSON(t, router, "/api/v1/contacts?limit=100&search="+search, &out)
	return out
}

func listOverdue(t *testing.T, router *gin.Engine) []handlers.OverdueContactResponse {
	t.Helper()
	var out []handlers.OverdueContactResponse
	getJSON(t, router, "/api/v1/contacts/overdue", &out)
	return out
}

func getContact(t *testing.T, router *gin.Engine, id string) handlers.ContactResponse {
	t.Helper()
	var out handlers.ContactResponse
	getJSON(t, router, "/api/v1/contacts/"+id, &out)
	return out
}

func countInteractions(t *testing.T, router *gin.Engine, id string) int {
	t.Helper()
	var out []map[string]any
	getJSON(t, router, "/api/v1/contacts/"+id+"/interactions", &out)
	return len(out)
}

// --- residue -----------------------------------------------------------------

// namespaceResidue counts every row class the cleanup ladder is responsible
// for, scoped to one namespace. Every count must be zero after a clean sweep —
// TOMBSTONES INCLUDED, which is why the contact count comes from the
// soft-delete-blind id query rather than a live-only read.
type namespaceResidue struct {
	Contacts          int
	Interactions      int
	Events            int
	ExternalIdentity  int64
	ExternalContacts  int64
	Nodes             int64
	Hosts             int
	CommsMessages     int64
	TelegramPeerBand  int64
	PhoneMethodsInNS  int64
	VenueNodesTracked int64
}

func (r namespaceResidue) total() int64 {
	return int64(r.Contacts) + int64(r.Interactions) + int64(r.Events) +
		r.ExternalIdentity + r.ExternalContacts + r.Nodes + int64(r.Hosts) +
		r.CommsMessages + r.TelegramPeerBand + r.PhoneMethodsInNS + r.VenueNodesTracked
}

// measureResidue reads the namespace's surviving rows through the support
// repository. Contact-scoped children are counted via the contacts the prefix
// query still finds; once the contacts are gone those counts are zero by
// construction, so the contact count is the load-bearing one and the rest guard
// the intermediate states.
func measureResidue(t *testing.T, ctx context.Context, database *db.Database, namespace string, seed uint64) namespaceResidue {
	t.Helper()
	support := repository.NewSyntheticSupportRepository(database.Queries)
	gen := factory.NewGenerator(seed, namespace)
	prefix := gen.Prefix()

	contactIDs, err := support.SelectContactIDsByFullNamePrefix(ctx, prefix)
	require.NoError(t, err)
	eventIDs, err := support.ListEventIdsForContacts(ctx, contactIDs)
	require.NoError(t, err)
	rootEventIDs, err := support.ListEventIdsBySourceAndSourceIDPrefix(ctx, repository.InteractionSourceEmail, prefix)
	require.NoError(t, err)
	venues, err := support.SelectVenueNodeIDsForContacts(ctx, contactIDs)
	require.NoError(t, err)
	identities, err := support.CountExternalIdentitiesByIdentifierPrefix(ctx, prefix)
	require.NoError(t, err)
	nodes, err := support.CountNodesByLabelPrefix(ctx, prefix)
	require.NoError(t, err)
	liveNodes, err := support.CountLiveNodesByIds(ctx, contactIDs)
	require.NoError(t, err)
	peers, err := support.CountTelegramMessagesInPeerBand(ctx, gen.PeerBandStart(), gen.PeerBandEnd())
	require.NoError(t, err)
	phones, err := support.CountContactMethodsByValueNormalizedPrefix(ctx, gen.SyntheticPhonePrefix())
	require.NoError(t, err)
	commsLinked, err := support.CountUnmatchedExternalContactByEmailPrefix(ctx, prefix)
	require.NoError(t, err)

	hosts := 0
	if _, exists, err := support.SelectMacHostIDByHostname(ctx, prefix+"host"); err == nil && exists {
		hosts = 1
	} else {
		require.NoError(t, err)
	}

	return namespaceResidue{
		Contacts:          len(contactIDs),
		Events:            len(eventIDs) + len(rootEventIDs),
		ExternalIdentity:  identities,
		ExternalContacts:  commsLinked,
		Nodes:             nodes + liveNodes,
		Hosts:             hosts,
		TelegramPeerBand:  peers,
		PhoneMethodsInNS:  phones,
		VenueNodesTracked: int64(len(venues)),
	}
}

// requireCleanedTo runs cleanup for the requested tokens and asserts every
// effective namespace reports "cleaned".
func requireCleaned(t *testing.T, ctx context.Context, database *db.Database, tokens []string, seed uint64) declare.CleanupResult {
	t.Helper()
	res, err := declare.CleanupNamespaces(ctx, database, tokens, seed)
	require.NoError(t, err)
	for ns, outcome := range res.Results {
		require.Equal(t, declare.StatusCleaned, outcome.Status,
			"namespace %s: %s (%s)", ns, outcome.Status, outcome.Err)
	}
	return res
}

// hostnameFor is the discovery marker every harness seeds for a namespace.
func hostnameFor(namespace string) string {
	return factory.SyntheticSourcePrefix + namespace + "-host"
}

// waitFor polls predicate until it holds or the budget expires. The budget is a
// timer context rather than a wall-clock deadline comparison: the repo bans
// direct time.Now() reads, and a context deadline is the audited way to bound a
// poll loop.
func waitFor(t *testing.T, budget time.Duration, what string, predicate func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if predicate() {
			return
		}
		select {
		case <-ctx.Done():
			// One last look: the predicate may have become true inside the tick
			// that the deadline interrupted.
			require.True(t, predicate(), "timed out after %s waiting for %s", budget, what)
			return
		case <-ticker.C:
		}
	}
}

// pollJobDisposition watches a planted river job until a worker touches it
// (snoozes or finalizes it) or the budget expires, returning the last
// disposition it managed to read.
//
// It deliberately makes no assertions: it runs inside a test hook, on the run's
// own goroutine, where a failed require would abandon declare.Run mid-flight —
// leaking its River client and leaving the hook armed for the next test. The
// caller asserts on the returned value after the run completes.
func pollJobDisposition(
	ctx context.Context,
	support *repository.SyntheticSupportRepository,
	jobID int64,
	budget time.Duration,
) repository.RiverJobDisposition {
	deadline, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	var last repository.RiverJobDisposition
	for {
		if d, err := support.GetRiverJobDisposition(deadline, jobID); err == nil {
			last = d
			if d.Snoozes > 0 || d.Finalized {
				return last
			}
		}
		select {
		case <-deadline.Done():
			return last
		case <-ticker.C:
		}
	}
}

func mustRun(t *testing.T, ctx context.Context, database *db.Database, behaviorID, namespace string) declare.Result {
	t.Helper()
	res, err := declare.Run(ctx, database, behaviorID, namespace, factory.DefaultSeed)
	require.NoError(t, err, "declare.Run(%s, %s)", behaviorID, namespace)
	return res
}

func fmtNS(base string, n int) string { return fmt.Sprintf("%s-c%d", base, n) }
