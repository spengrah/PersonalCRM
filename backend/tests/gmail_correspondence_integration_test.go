// Integration coverage for the gmail_correspondence enrichment source.
// Drives the REAL producer, repos, ImportMatchService, EnrichmentService (with
// a live event bus + River client), and the re-derivation runner (with a fake
// Gmail fetcher via the provider's exported test seam) against the shared test
// DB. Proves:
//   - the producer turns name-bearing comms_message participants into
//     gmail_correspondence external_contact rows with the expected evidence +
//     suggested_match;
//   - linking a candidate adds the email as a contact_method and dispatches the
//     KindContactMethodsAdded rematch (the inherited backfill hand-off — async
//     scan completion is covered by gmail_rematch_integration_test.go, not here);
//   - sticky-ignore is preserved across producer re-runs (no clobber);
//   - the import endpoint returns 403 for this link-only source (the shared
//     import-suggestions surface's server-side link-only enforcement);
//   - the one-time re-derivation re-fetches name-less rows, ADDS the name keys
//     while preserving all existing content + provenance (the additive-merge
//     invariant), and the producer then surfaces the previously-hidden backlog;
//   - the re-derivation is idempotent.
//
// All seeding goes through repositories (sqlc-only); addresses/names are
// placeholders; times use accelerated.GetCurrentTime().
package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/matching"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
	gmailapi "google.golang.org/api/gmail/v1"
)

// correspondenceEnv bundles a real DB + the repos/services these tests drive.
type correspondenceEnv struct {
	ctx          context.Context
	database     *db.Database
	commsRepo    *repository.CommsMessageRepository
	contactRepo  *repository.ContactRepository
	methodRepo   *repository.ContactMethodRepository
	externalRepo *repository.ExternalContactRepository
	eventRepo    *repository.EventRepository
	matchService *service.ImportMatchService
	suggester    *google.GmailCorrespondenceSuggester
}

func newCorrespondenceEnv(t *testing.T, ownAddr string) *correspondenceEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx := context.Background()
	require.NoError(t, db.RunMigrations(ctx, databaseURL, getMigrationsPath()))

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	commsRepo := repository.NewCommsMessageRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	methodRepo := repository.NewContactMethodRepository(database.Queries)
	externalRepo := repository.NewExternalContactRepository(database.Queries)
	eventRepo := repository.NewEventRepository(database.Queries)

	suggester := google.NewGmailCorrespondenceSuggester(
		commsRepo, contactRepo, externalRepo,
		func(context.Context) (map[string]struct{}, error) {
			return map[string]struct{}{matching.NormalizeEmail(ownAddr): {}}, nil
		},
	)

	return &correspondenceEnv{
		ctx:          ctx,
		database:     database,
		commsRepo:    commsRepo,
		contactRepo:  contactRepo,
		methodRepo:   methodRepo,
		externalRepo: externalRepo,
		eventRepo:    eventRepo,
		matchService: service.NewImportMatchService(contactRepo),
		suggester:    suggester,
	}
}

// uniqueAddr returns a per-test unique placeholder address so parallel/shared-DB
// runs do not collide on the producer's source_id dedup.
func uniqueAddr(prefix string) string {
	return prefix + "-" + uuid.New().String()[:8] + "@example.test"
}

// seedCorrespondenceContact creates a CRM contact and registers cleanup of its
// comms_message rows, methods, and the contact itself.
func (e *correspondenceEnv) seedContact(t *testing.T, fullName string) *repository.Contact {
	t.Helper()
	c, err := e.contactRepo.CreateContact(e.ctx, repository.CreateContactRequest{FullName: fullName})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = e.commsRepo.HardDeleteByContact(e.ctx, c.ID)
		_ = e.contactRepo.SoftDeleteContact(e.ctx, c.ID)
	})
	return c
}

// nameMetadata builds a source_metadata blob with a single From participant
// carrying both the bare address and the display name (the post-capture shape).
func nameMetadata(t *testing.T, fromAddr, fromName, ownAddr string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"from":      fromAddr,
		"from_name": fromName,
		"to":        []string{ownAddr},
		"to_names":  []string{"Me"},
		"subject":   "Hello",
		"html":      "<p>body</p>",
	})
	require.NoError(t, err)
	return b
}

// seedMessage upserts one email comms_message row for the contact.
func (e *correspondenceEnv) seedMessage(t *testing.T, contactID uuid.UUID, externalID, account, gmailID string, metadata []byte) {
	t.Helper()
	acct := account
	gid := gmailID
	_, err := e.commsRepo.UpsertMessage(e.ctx, repository.UpsertCommsMessageParams{
		Source:           repository.InteractionSourceEmail,
		ExternalID:       externalID,
		Direction:        repository.InteractionDirectionInbound,
		SentAt:           accelerated.GetCurrentTime().Add(-24 * time.Hour),
		AccountID:        &acct,
		SourceMetadata:   metadata,
		MatchedContactID: contactID,
		GmailMessageID:   &gid,
	})
	require.NoError(t, err)
}

// cleanupExternal registers a hard-delete of a produced candidate so the
// shared DB does not accumulate gmail_correspondence rows across runs.
func (e *correspondenceEnv) cleanupExternal(t *testing.T, sourceID string) {
	t.Helper()
	t.Cleanup(func() {
		row, _ := e.externalRepo.GetBySource(e.ctx, google.CorrespondenceSource, sourceID, nil)
		if row != nil {
			_ = e.externalRepo.Delete(e.ctx, row.ID)
		}
	})
}

func TestCorrespondence_ProducerEmitsCandidateWithSuggestedMatch(t *testing.T) {
	ownAddr := uniqueAddr("me")
	e := newCorrespondenceEnv(t, ownAddr)

	fullName := "Correspondence Alpha " + uuid.New().String()[:6]
	contact := e.seedContact(t, fullName)
	unknownAddr := uniqueAddr("alpha")
	e.cleanupExternal(t, matching.NormalizeEmail(unknownAddr))

	e.seedMessage(t, contact.ID, "ext-"+uuid.New().String(), ownAddr, "gmail-1",
		nameMetadata(t, unknownAddr, fullName, ownAddr))

	n, err := e.suggester.Run(e.ctx, accelerated.GetCurrentTime().Add(-google.CorrespondenceWindow))
	require.NoError(t, err)
	require.GreaterOrEqual(t, n, 1)

	row, err := e.externalRepo.GetBySource(e.ctx, google.CorrespondenceSource, matching.NormalizeEmail(unknownAddr), nil)
	require.NoError(t, err)
	require.NotNil(t, row, "producer must emit a gmail_correspondence candidate")
	require.Equal(t, repository.MatchStatusUnmatched, row.MatchStatus)
	require.NotNil(t, row.DisplayName)
	require.Equal(t, fullName, *row.DisplayName)
	require.Equal(t, float64(1), row.Metadata["message_count"])
	co, ok := row.Metadata["co_occurring_contact"].(map[string]any)
	require.True(t, ok, "co_occurring_contact evidence present")
	require.Equal(t, contact.ID.String(), co["id"])

	// The surface recomputes suggested_match from display_name → the seeded
	// same-named contact.
	match, err := e.matchService.FindBestMatch(e.ctx, row)
	require.NoError(t, err)
	require.NotNil(t, match, "suggested match recomputed from display_name")
	require.Equal(t, contact.ID.String(), match.ContactID)
}

func TestCorrespondence_SkipsSoftDeletedContactRows(t *testing.T) {
	// A contact's soft-delete (UPDATE deleted_at) does NOT cascade to its
	// comms_message rows — the FK cascade only fires on a hard DELETE. The
	// producer's scan query INNER-JOINs a live contact, so once the matched
	// contact is soft-deleted its correspondence must stop surfacing candidates.
	ownAddr := uniqueAddr("me")
	e := newCorrespondenceEnv(t, ownAddr)

	fullName := "Correspondence Deleted " + uuid.New().String()[:6]
	contact := e.seedContact(t, fullName)
	unknownAddr := uniqueAddr("deleted")
	normAddr := matching.NormalizeEmail(unknownAddr)
	e.cleanupExternal(t, normAddr)

	e.seedMessage(t, contact.ID, "ext-"+uuid.New().String(), ownAddr, "gmail-1",
		nameMetadata(t, unknownAddr, fullName, ownAddr))

	// Soft-delete the matched contact; its comms_message row stays live.
	require.NoError(t, e.contactRepo.SoftDeleteContact(e.ctx, contact.ID))

	_, err := e.suggester.Run(e.ctx, accelerated.GetCurrentTime().Add(-google.CorrespondenceWindow))
	require.NoError(t, err)

	row, err := e.externalRepo.GetBySource(e.ctx, google.CorrespondenceSource, normAddr, nil)
	require.NoError(t, err)
	require.Nil(t, row, "a soft-deleted contact's correspondence must not surface a candidate")
}

func TestCorrespondence_LinkAddsMethodAndDispatchesRematch(t *testing.T) {
	ownAddr := uniqueAddr("me")
	e := newCorrespondenceEnv(t, ownAddr)

	// Live River client + event bus so the link publishes KindContactMethodsAdded
	// and enqueues a RematchDispatcher job (the inherited backfill hand-off).
	workers := river.NewWorkers()
	river.AddWorker(workers, &correspondenceDispatcherNoopWorker{})
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
	rematchSvc.Register(correspondenceEmailEligibleHandler{})
	enrichmentRepo := repository.NewEnrichmentRepository(e.database.Queries)
	enrichment := service.NewEnrichmentService(e.database, e.contactRepo, e.methodRepo, enrichmentRepo, bus, rematchSvc)

	fullName := "Correspondence Beta " + uuid.New().String()[:6]
	contact := e.seedContact(t, fullName)
	unknownAddr := uniqueAddr("beta")
	normAddr := matching.NormalizeEmail(unknownAddr)
	e.cleanupExternal(t, normAddr)

	e.seedMessage(t, contact.ID, "ext-"+uuid.New().String(), ownAddr, "gmail-1",
		nameMetadata(t, unknownAddr, fullName, ownAddr))

	_, err = e.suggester.Run(e.ctx, accelerated.GetCurrentTime().Add(-google.CorrespondenceWindow))
	require.NoError(t, err)
	row, err := e.externalRepo.GetBySource(e.ctx, google.CorrespondenceSource, normAddr, nil)
	require.NoError(t, err)
	require.NotNil(t, row)

	// Link: enrich the contact with the candidate's email (the modal's link path).
	jobID, err := enrichment.EnrichContactFromExternalWithSelections(
		e.ctx, contact.ID, row,
		[]service.MethodSelection{{OriginalValue: normAddr, Type: "email"}},
		nil, nil, nil,
	)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, jobID, "email handler eligible → a rematch job id is returned")

	// (a) the email is now a contact_method on the linked contact.
	methods, err := e.methodRepo.ListContactMethodsByContact(e.ctx, contact.ID)
	require.NoError(t, err)
	found := false
	for _, m := range methods {
		if m.Value == normAddr && m.Type == "email" {
			found = true
		}
	}
	require.True(t, found, "linked email must be added as a contact_method")

	// (b) the rematch was dispatched: a contact_methods.added event published +
	// a RematchDispatcher job enqueued for (contact, jobID).
	count, err := e.eventRepo.CountRematchDispatcherJobs(e.ctx, contact.ID, jobID)
	require.NoError(t, err)
	require.Equal(t, int64(1), count, "exactly one rematch dispatcher job enqueued")
}

func TestCorrespondence_StickyIgnoreNoClobber(t *testing.T) {
	ownAddr := uniqueAddr("me")
	e := newCorrespondenceEnv(t, ownAddr)

	fullName := "Correspondence Gamma " + uuid.New().String()[:6]
	contact := e.seedContact(t, fullName)
	unknownAddr := uniqueAddr("gamma")
	normAddr := matching.NormalizeEmail(unknownAddr)
	e.cleanupExternal(t, normAddr)

	e.seedMessage(t, contact.ID, "ext-"+uuid.New().String(), ownAddr, "gmail-1",
		nameMetadata(t, unknownAddr, fullName, ownAddr))

	// First run produces the candidate; ignore it.
	_, err := e.suggester.Run(e.ctx, accelerated.GetCurrentTime().Add(-google.CorrespondenceWindow))
	require.NoError(t, err)
	row, err := e.externalRepo.GetBySource(e.ctx, google.CorrespondenceSource, normAddr, nil)
	require.NoError(t, err)
	require.NotNil(t, row)
	require.NoError(t, e.externalRepo.Ignore(e.ctx, row.ID))

	// Re-run the producer: the ignored row must STAY ignored (the upsert's
	// DO UPDATE SET never touches match_status) and must NOT reappear unmatched.
	_, err = e.suggester.Run(e.ctx, accelerated.GetCurrentTime().Add(-google.CorrespondenceWindow))
	require.NoError(t, err)

	after, err := e.externalRepo.GetBySource(e.ctx, google.CorrespondenceSource, normAddr, nil)
	require.NoError(t, err)
	require.NotNil(t, after)
	require.Equal(t, repository.MatchStatusIgnored, after.MatchStatus, "ignored row not clobbered to unmatched")

	unmatched, err := e.externalRepo.ListAllUnmatched(e.ctx, 1000, 0)
	require.NoError(t, err)
	for _, u := range unmatched {
		require.NotEqual(t, normAddr, u.SourceID, "ignored candidate must not reappear in the unmatched list")
	}
}

func TestCorrespondence_ImportEndpointForbidden(t *testing.T) {
	ownAddr := uniqueAddr("me")
	e := newCorrespondenceEnv(t, ownAddr)

	// Seed a gmail_correspondence candidate directly.
	normAddr := matching.NormalizeEmail(uniqueAddr("delta"))
	e.cleanupExternal(t, normAddr)
	name := "Correspondence Delta"
	row, err := e.externalRepo.Upsert(e.ctx, repository.UpsertExternalContactRequest{
		Source:      google.CorrespondenceSource,
		SourceID:    normAddr,
		DisplayName: &name,
		Emails:      []repository.EmailEntry{{Value: normAddr}},
	})
	require.NoError(t, err)
	require.NotNil(t, row)

	// Wire the import handler + POST the import endpoint.
	contactService := service.NewContactService(e.database, e.contactRepo, e.methodRepo,
		repository.NewInteractionRepository(e.database.Queries), repository.NewContactTaskRepository(e.database.Queries), nil, nil)
	enrichmentRepo := repository.NewEnrichmentRepository(e.database.Queries)
	enrichment := service.NewEnrichmentService(e.database, e.contactRepo, e.methodRepo, enrichmentRepo, nil, nil)
	suggestionService := service.NewSuggestionService(e.externalRepo, e.contactRepo, e.methodRepo, enrichment, e.matchService, e.database)
	importHandler := handlers.NewImportHandler(e.externalRepo, nil, contactService, e.matchService, enrichment, suggestionService)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	v1 := router.Group("/api/v1")
	v1.Group("/imports").POST("/:id/import", importHandler.ImportContact)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/imports/"+row.ID.String()+"/import", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code, "link-only source must reject import; body: %s", rec.Body.String())
}

func TestCorrespondence_RederivationEndToEndAndIdempotent(t *testing.T) {
	ownAddr := uniqueAddr("me")
	e := newCorrespondenceEnv(t, ownAddr)

	fullName := "Correspondence Epsilon " + uuid.New().String()[:6]
	contact := e.seedContact(t, fullName)
	unknownAddr := uniqueAddr("epsilon")
	normAddr := matching.NormalizeEmail(unknownAddr)
	e.cleanupExternal(t, normAddr)

	account := ownAddr
	gmailID := "gmail-rederive-" + uuid.New().String()[:8]
	externalID := "ext-" + uuid.New().String()

	// Seed a NAME-LESS row (pre-capture shape): bare from/to + full content +
	// account_gmail_ids provenance, but no *_name keys.
	namelessMeta, err := json.Marshal(map[string]any{
		"from":    unknownAddr,
		"to":      []string{ownAddr},
		"subject": "Original Subject",
		"html":    "<p>original body</p>",
	})
	require.NoError(t, err)
	e.seedMessage(t, contact.ID, externalID, account, gmailID, namelessMeta)

	// Producer over the name-less row yields nothing (no display name).
	n0, err := e.suggester.Run(e.ctx, accelerated.GetCurrentTime().Add(-google.CorrespondenceWindow))
	require.NoError(t, err)
	pre, err := e.externalRepo.GetBySource(e.ctx, google.CorrespondenceSource, normAddr, nil)
	require.NoError(t, err)
	require.Nil(t, pre, "name-less row must not surface (n0=%d)", n0)

	// Provider with a fake fetcher that supplies display-name headers for the
	// re-fetch (no OAuth).
	provider := google.NewGmailSyncProvider(nil, e.commsRepo, nil, e.database.Pool)
	provider.SetFetcherFactoryForTest(google.NewFakeGmailFetcherFactoryForTest(google.FakeGmailFetcherFuncs{
		GetMessage: func(_ context.Context, id string) (*gmailapi.Message, error) {
			require.Equal(t, gmailID, id)
			return &gmailapi.Message{
				Id: id,
				Payload: &gmailapi.MessagePart{
					Headers: []*gmailapi.MessagePartHeader{
						{Name: "From", Value: "\"" + fullName + "\" <" + unknownAddr + ">"},
						{Name: "To", Value: ownAddr},
					},
				},
			}, nil
		},
	}))

	runner := google.NewCorrespondenceNameRederiveService(e.commsRepo, provider)
	since := accelerated.GetCurrentTime().Add(-google.CorrespondenceWindow)
	res, err := runner.RederiveNames(e.ctx, since)
	require.NoError(t, err)
	require.GreaterOrEqual(t, res.Rederived, 1)
	require.Equal(t, 0, res.Failed)

	// Additive-merge invariant: the row now has the name keys AND still has its
	// original content + provenance keys.
	msg, err := e.commsRepo.GetMessage(e.ctx, repository.InteractionSourceEmail, externalID, contact.ID)
	require.NoError(t, err)
	var merged map[string]any
	require.NoError(t, json.Unmarshal(msg.SourceMetadata, &merged))
	require.Equal(t, fullName, merged["from_name"], "name key added")
	require.Equal(t, unknownAddr, merged["from"], "original from preserved")
	require.Equal(t, "Original Subject", merged["subject"], "original subject preserved")
	require.Equal(t, "<p>original body</p>", merged["html"], "original html preserved")
	require.Contains(t, merged, "account_gmail_ids", "provenance preserved")
	require.Contains(t, merged, "observed_accounts", "provenance preserved")

	// Producer over the full range now surfaces the previously-hidden backlog.
	_, err = e.suggester.Run(e.ctx, since)
	require.NoError(t, err)
	row, err := e.externalRepo.GetBySource(e.ctx, google.CorrespondenceSource, normAddr, nil)
	require.NoError(t, err)
	require.NotNil(t, row, "re-derived backlog now surfaces as a candidate")

	// Idempotency: a second re-derivation re-derives nothing (NOT (? 'from_name')).
	res2, err := runner.RederiveNames(e.ctx, since)
	require.NoError(t, err)
	require.Equal(t, 0, res2.Rederived, "second run re-derives nothing")
}

// correspondenceEmailEligibleHandler makes the "email" method type eligible so
// the enrichment link publishes KindContactMethodsAdded. The admin/test process
// never runs the handler body — only the crm-api dispatcher does.
type correspondenceEmailEligibleHandler struct{}

func (correspondenceEmailEligibleHandler) IdentifierType() string { return "email" }
func (correspondenceEmailEligibleHandler) Rematch(context.Context, uuid.UUID, string) (int, error) {
	return 0, nil
}

// correspondenceDispatcherNoopWorker satisfies River's registered-kind rule so
// the live client accepts RematchDispatcher inserts; we assert row counts, not
// execution.
type correspondenceDispatcherNoopWorker struct {
	river.WorkerDefaults[correspondenceDispatcherNoopArgs]
}

func (*correspondenceDispatcherNoopWorker) Work(context.Context, *river.Job[correspondenceDispatcherNoopArgs]) error {
	return nil
}

type correspondenceDispatcherNoopArgs struct{}

func (correspondenceDispatcherNoopArgs) Kind() string { return "rematch_dispatcher" }
