package api

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
)

// buildManualHandlerForTest wires the PR 6 cutover manual-interaction
// pipeline (bus → consumer → manual handler) against a real DB + live
// river client. The manual handler invokes HandleEvent INLINE inside its
// own tx, so the HTTP response returns immediately with the interaction.
// The river client is running live (not TestOnly) so any async jobs the
// bus enqueues (for asserts-via-river tests) complete too.
//
// Passes the outer ctx (not a timeout-derived one) to client.Start so
// the river client's fetch loop doesn't die when this helper returns.
//
// Returns the manual handler, the contact service (for callers that still
// exercise the service directly), and a cleanup func that stops the client.
func buildManualHandlerForTest(ctx context.Context, database *db.Database, cfg *config.Config) (*service.ManualInteractionHandler, *service.ContactService, func(), error) {
	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	telegramMessageRepo := repository.NewTelegramMessageRepository(database.Queries)
	eventRepo := repository.NewEventRepository(database.Queries)

	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, contactTaskRepo, nil, nil)

	// Chicken-and-egg: the worker needs the bus, the bus needs the client,
	// the client needs the workers bundle pre-registered. Register a shim
	// pointer-wrapper worker, construct client + bus + real recorder, then
	// assign the shim's `real` field.
	workers := river.NewWorkers()
	shim := &apiTestRecorderShim{}
	river.AddWorker(workers, shim)
	// interaction.recorded events enqueue cadence_updater and
	// followup_manager jobs. Register no-op placeholders so river
	// accepts those kinds; API tests don't exercise the real workers.
	river.AddWorker(workers, &apiTestCadenceShim{})
	river.AddWorker(workers, &apiTestFollowUpShim{})

	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers:  workers,
		TestOnly: true, // disable staggered maintenance startup
	})
	if err != nil {
		return nil, nil, nil, err
	}

	bus := events.NewBus(database.Pool, client, eventRepo)
	// Wire a real CadenceUpdater so API manual-handler tests exercise
	// the inline apply-on-publish path against a live DB. contactRepo's
	// pool is already set upstream by the router wiring; if not,
	// cadence_updater direct-invoke APIs fall back to the caller's tx.
	contactRepo.SetPool(database.Pool)
	claimRepo := repository.NewEventConsumerClaimRepository(database.Queries)
	cadenceUpdater := consumer.NewCadenceUpdater(
		claimRepo, contactRepo, database.Queries,
		consumer.CadenceModeCutover, // API tests exercise the cutover writer
		false,
	)
	// Wire cadenceUpdater into the contact service so direct-invoke
	// paths (Merge / Extend / Promote / RecordInteraction non-bus
	// wrapper / UpdateContact cadence-edit) reach the sole writer in
	// these tests.
	contactService.SetCadenceUpdater(cadenceUpdater)
	stagingRegistry := repository.NewStagingProcessorRegistry(map[string]repository.StagingProcessor{
		repository.InteractionSourceTelegram: repository.NewTelegramStagingProcessor(telegramMessageRepo),
	})
	recorder := consumer.NewInteractionRecorder(contactService, stagingRegistry, bus, cadenceUpdater, nil, repository.NewCalendarEventRepository(database.Queries))
	shim.real = consumer.NewInteractionRecorderWorker(bus, database.Pool, recorder, nil)

	manualHandler := service.NewManualInteractionHandler(database.Pool, bus, recorder)

	// Pass the outer ctx — river derives the fetch-loop ctx from whatever
	// we pass to Start, and a cancelled ctx silently kills job fetching.
	if err := client.Start(ctx); err != nil {
		return nil, nil, nil, err
	}

	cleanup := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = client.Stop(stopCtx)
	}

	return manualHandler, contactService, cleanup, nil
}

// mustBuildManualHandlerForTest is the *testing.T wrapper used by
// setup* routers. Uses t.Fatalf on error and registers the cleanup via
// t.Cleanup so callers don't have to track the cleanup func.
func mustBuildManualHandlerForTest(t *testing.T, ctx context.Context, database *db.Database, cfg *config.Config) (*service.ManualInteractionHandler, *service.ContactService) {
	t.Helper()
	mh, cs, cleanup, err := buildManualHandlerForTest(ctx, database, cfg)
	if err != nil {
		t.Fatalf("failed to build manual handler test wiring: %v", err)
	}
	t.Cleanup(cleanup)
	return mh, cs
}

// wireCadenceUpdaterForAPITest constructs a real CadenceUpdater
// against the given database and injects it into contactService so
// cadence entry points (RecordInteraction direct path, MergeContacts,
// cadence edits via UpdateContact, link/import cadence overrides)
// exercise the sole writer in API-layer tests that don't need the full
// event-bus wiring of buildManualHandlerForTest. Returns the
// constructed CadenceUpdater so callers can also wire it into
// EnrichmentService. Takes *testing.T so callers that have one can
// still use t.Helper; pass nil from non-test helpers like
// setupImportTestRouter.
func wireCadenceUpdaterForAPITest(t *testing.T, database *db.Database, contactService *service.ContactService) *consumer.CadenceUpdater {
	if t != nil {
		t.Helper()
	}
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	claimRepo := repository.NewEventConsumerClaimRepository(database.Queries)
	cadenceUpdater := consumer.NewCadenceUpdater(
		claimRepo, contactRepo, database.Queries,
		consumer.CadenceModeCutover,
		false,
	)
	contactService.SetCadenceUpdater(cadenceUpdater)
	return cadenceUpdater
}

// apiTestRecorderShim defers InteractionRecorderWorker assignment until
// after bus + recorder are constructed. See buildManualHandlerForTest.
type apiTestRecorderShim struct {
	river.WorkerDefaults[consumerjobs.InteractionRecorderJobArgs]
	real *consumer.InteractionRecorderWorker
}

func (w *apiTestRecorderShim) Work(ctx context.Context, j *river.Job[consumerjobs.InteractionRecorderJobArgs]) error {
	if w.real == nil {
		return nil // shim invoked pre-assignment; no-op
	}
	return w.real.Work(ctx, j)
}

func (w *apiTestRecorderShim) Timeout(j *river.Job[consumerjobs.InteractionRecorderJobArgs]) time.Duration {
	if w.real == nil {
		return 30 * time.Second
	}
	return w.real.Timeout(j)
}

// apiTestCadenceShim is a no-op worker for cadence_updater jobs in API
// tests. Consumes the job without side-effects so river doesn't fail
// the enclosing InsertTx with "unhandled job kind".
type apiTestCadenceShim struct {
	river.WorkerDefaults[consumerjobs.CadenceUpdaterJobArgs]
}

func (*apiTestCadenceShim) Work(_ context.Context, _ *river.Job[consumerjobs.CadenceUpdaterJobArgs]) error {
	return nil
}

func (*apiTestCadenceShim) Timeout(_ *river.Job[consumerjobs.CadenceUpdaterJobArgs]) time.Duration {
	return 30 * time.Second
}

// apiTestFollowUpShim is a no-op worker for followup_manager jobs in
// API tests. Same rationale as apiTestCadenceShim.
type apiTestFollowUpShim struct {
	river.WorkerDefaults[consumerjobs.FollowUpManagerJobArgs]
}

func (*apiTestFollowUpShim) Work(_ context.Context, _ *river.Job[consumerjobs.FollowUpManagerJobArgs]) error {
	return nil
}

func (*apiTestFollowUpShim) Timeout(_ *river.Job[consumerjobs.FollowUpManagerJobArgs]) time.Duration {
	return 30 * time.Second
}
