package tests

import (
	"context"
	"fmt"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
)

// setupTestEventBus wires a real river client + InteractionRecorder +
// ContactService against the provided database. The client runs live
// (NOT TestOnly) so published events are picked up by the consumer
// worker and written asynchronously — the same flow production runs.
// Cleanup stops the river client on t.Cleanup.
//
// Use the returned *events.Bus in place of nil when constructing
// CalendarSyncProvider / CalendarRematchHandler / AggregationEngine in
// tests. After the publisher runs, call waitForInteractionBySourceRef /
// waitForInteractionCount / waitForTelegramMessagesProcessed to wait for
// the async consumer write.
//
// This replaces the PR 5 "direct path wrote first" synchronous behavior
// that pre-cutover tests relied on. Post-PR-6 there is no direct path —
// the async consumer is the sole writer (spec §3.4.1; plan Decision 4a).
func setupTestEventBus(
	t *testing.T,
	ctx context.Context,
	database *db.Database,
	contactService *service.ContactService,
) *events.Bus {
	t.Helper()

	eventRepo := repository.NewEventRepository(database.Queries)
	telegramMessageRepo := repository.NewTelegramMessageRepository(database.Queries)

	cfg := config.TestConfig()
	if cfg.River.WorkerConcurrency <= 0 {
		cfg.River.WorkerConcurrency = 4
	}

	// River client + worker registration has a chicken-and-egg shape:
	//  - river.NewClient needs the workers bundle.
	//  - The real InteractionRecorderWorker needs bus + recorder, which
	//    need the client to construct.
	//
	// Resolve by deferring the recorder inside the worker shim. A shared
	// pointer is filled after both bus and recorder exist — before the
	// client Start runs any jobs.
	workers := river.NewWorkers()
	shim := &deferredRecorderWorker{}
	river.AddWorker(workers, shim)
	// InteractionRecorder publishes interaction.recorded which enqueues
	// both cadence_updater and followup_manager jobs. Tests that don't
	// exercise those consumers still need to register workers for the
	// kinds — river rejects unknown kinds at dequeue time. Placeholder
	// no-op workers drain the queue without doing any work.
	river.AddWorker(workers, &cadenceUpdaterNoopWorker{})
	river.AddWorker(workers, &followUpManagerNoopWorker{})
	// email.received / email.sent now route to the email_interaction_consumer
	// kind (Gmail phase 3). Register a no-op worker so suites that publish
	// email.* through this harness (e.g. the Gmail provider sweep tests, which
	// assert on the durable event + content rows, not the derived interaction)
	// can enqueue legally. Suites that want the REAL email consumer use
	// setupTestEventBusForEmail.
	river.AddWorker(workers, &emailInteractionNoopWorker{})

	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers:  workers,
		TestOnly: true, // disable staggered maintenance startup so test-scoped clients start processing jobs immediately
	})
	require.NoError(t, err)

	bus := events.NewBus(database.Pool, client, eventRepo)
	// Build a real CadenceUpdater so the recorder's inline apply fires
	// in these end-to-end integration tests.
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	claimRepo := repository.NewEventConsumerClaimRepository(database.Queries)
	cadenceUpdater := consumer.NewCadenceUpdater(
		claimRepo, contactRepo, database.Queries,
		consumer.CadenceModeCutover,
		false,
	)
	// Wire cadenceUpdater into the contact service so direct-invoke
	// paths (Merge / Extend / Promote / RecordInteraction non-bus
	// wrapper / UpdateContact cadence-edit) work in these async
	// integration tests.
	contactService.SetCadenceUpdater(cadenceUpdater)
	stagingRegistry := repository.NewStagingProcessorRegistry(map[string]repository.StagingProcessor{
		repository.InteractionSourceTelegram: repository.NewTelegramStagingProcessor(telegramMessageRepo),
	})
	recorder := consumer.NewInteractionRecorder(contactService, stagingRegistry, bus, cadenceUpdater, nil, repository.NewCalendarEventRepository(database.Queries))
	// Fill the shim's real worker now that bus + recorder exist.
	shim.real = consumer.NewInteractionRecorderWorker(bus, database.Pool, recorder, nil)

	// IMPORTANT: pass the OUTER ctx (not a timeout-derived one) to Start.
	// River derives its fetch/work context from whatever Start() receives;
	// if we pass a context that gets cancelled on this helper's return,
	// the river client silently stops fetching jobs and tests hang.
	require.NoError(t, client.Start(ctx))

	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = client.Stop(stopCtx)
	})

	return bus
}

// setupTestEventBusWithRematch is the PR-10 counterpart to
// setupTestEventBus that also wires the RematchDispatcherWorker against
// the given *service.RematchService. Matches the harness pattern
// (shim + real-worker fill-in) so the rematch consumer is live after
// Start.
//
// Chicken-and-egg: the bus needs InteractionRecorder (which needs
// contactService), and contactService needs the bus to publish
// contact_methods.added. Callers construct contactSvc with nil bus
// FIRST, then call this helper to obtain the live bus, then call
// contactSvc.InjectBusForTest(bus) to swap the nil bus for the real
// one before any test runs. Same shape for EnrichmentService if tests
// exercise enrichment publishes.
func setupTestEventBusWithRematch(
	t *testing.T,
	ctx context.Context,
	database *db.Database,
	contactService *service.ContactService,
	rematchSvc *service.RematchService,
) *events.Bus {
	t.Helper()

	eventRepo := repository.NewEventRepository(database.Queries)
	telegramMessageRepo := repository.NewTelegramMessageRepository(database.Queries)

	cfg := config.TestConfig()
	if cfg.River.WorkerConcurrency <= 0 {
		cfg.River.WorkerConcurrency = 4
	}

	workers := river.NewWorkers()
	interactionShim := &deferredRecorderWorker{}
	river.AddWorker(workers, interactionShim)
	river.AddWorker(workers, &cadenceUpdaterNoopWorker{})
	river.AddWorker(workers, &followUpManagerNoopWorker{})
	river.AddWorker(workers, &emailInteractionNoopWorker{})
	rematchShim := &deferredRematchWorker{}
	river.AddWorker(workers, rematchShim)

	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)

	bus := events.NewBus(database.Pool, client, eventRepo)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	claimRepo := repository.NewEventConsumerClaimRepository(database.Queries)
	cadenceUpdater := consumer.NewCadenceUpdater(
		claimRepo, contactRepo, database.Queries,
		consumer.CadenceModeCutover,
		false,
	)
	contactService.SetCadenceUpdater(cadenceUpdater)
	stagingRegistry2 := repository.NewStagingProcessorRegistry(map[string]repository.StagingProcessor{
		repository.InteractionSourceTelegram: repository.NewTelegramStagingProcessor(telegramMessageRepo),
	})
	recorder := consumer.NewInteractionRecorder(contactService, stagingRegistry2, bus, cadenceUpdater, nil, repository.NewCalendarEventRepository(database.Queries))
	interactionShim.real = consumer.NewInteractionRecorderWorker(bus, database.Pool, recorder, nil)

	rematchDispatcher := consumer.NewRematchDispatcher(rematchSvc)
	rematchShim.real = consumer.NewRematchDispatcherWorker(bus, database.Pool, rematchDispatcher)

	// IMPORTANT: pass the OUTER ctx (not a timeout-derived one) to Start.
	require.NoError(t, client.Start(ctx))

	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = client.Stop(stopCtx)
	})

	return bus
}

// setupTestEventBusForEmail wires a live river client for the email
// integration suite with THREE real workers (vs. the noop cadence/follow-up
// of setupTestEventBus):
//
//  1. EmailInteractionConsumerWorker (subject under test) — registered for
//     the email_interaction_consumer kind.
//  2. The REAL CadenceUpdaterWorker, wired to the SAME CadenceUpdater
//     instance the email consumer's create branch uses inline, so the async
//     cadence_updater job enqueued by interaction.recorded actually writes
//     cadence columns. (In practice the create branch's inline cadence apply
//     wins the durable claim, so the async worker no-ops — but registering
//     the real worker exercises the claim/no-op semantics exactly as prod,
//     and catches a regression where the inline path stops firing.)
//  3. A REAL FollowUpManagerWorker constructed in mode=off so the
//     followup_manager job enqueued by interaction.recorded drains cleanly
//     without Todoist wiring. The email consumer's inline followUp dep is
//     wired to the same off-mode manager. Cadence — not follow-up task
//     creation — is the load-bearing phase-3 assertion (§9), so off-mode
//     keeps the harness lean while draining the queue legally.
//
// Chicken-and-egg: the bus needs the email consumer (which needs the
// contactService as writer/aggregator), and the consumer needs the bus to
// publish interaction.recorded. Resolved with the same shim pattern as
// setupTestEventBus. Returns the live bus + the InteractionRepository
// (callers assert via FindBySourceRef) + the CommsMessageRepository (callers
// seed content rows + assert interaction_id/processed_at linkage).
func setupTestEventBusForEmail(
	t *testing.T,
	ctx context.Context,
	database *db.Database,
	contactService *service.ContactService,
) (*events.Bus, *consumer.EmailInteractionConsumer) {
	t.Helper()

	eventRepo := repository.NewEventRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	commsMessageRepo := repository.NewCommsMessageRepository(database.Queries)
	claimRepo := repository.NewEventConsumerClaimRepository(database.Queries)

	cfg := config.TestConfig()
	if cfg.River.WorkerConcurrency <= 0 {
		cfg.River.WorkerConcurrency = 4
	}

	cadenceUpdater := consumer.NewCadenceUpdater(
		claimRepo, contactRepo, database.Queries,
		consumer.CadenceModeCutover,
		false,
	)
	contactService.SetCadenceUpdater(cadenceUpdater)

	// Off-mode FollowUpManager: cutover-only Todoist deps are nil (gated on
	// mode == cutover per NewFollowUpManager's doc comment).
	followUpManager := consumer.NewFollowUpManager(
		consumer.FollowUpModeOff,
		claimRepo, contactRepo, nil, nil, interactionRepo, nil,
		database.Pool, nil, nil, "", cfg.Watchdog,
	)

	workers := river.NewWorkers()
	emailShim := &deferredEmailWorker{}
	river.AddWorker(workers, emailShim)
	cadenceShim := &deferredCadenceWorker{}
	river.AddWorker(workers, cadenceShim)
	followUpShim := &deferredFollowUpWorker{}
	river.AddWorker(workers, followUpShim)

	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)

	bus := events.NewBus(database.Pool, client, eventRepo)

	emailConsumer := consumer.NewEmailInteractionConsumer(
		contactService, commsMessageRepo, interactionRepo, contactService,
		bus, cadenceUpdater, followUpManager,
	)
	emailShim.real = consumer.NewEmailInteractionConsumerWorker(bus, database.Pool, emailConsumer)
	cadenceShim.real = consumer.NewCadenceUpdaterWorker(bus, database.Pool, cadenceUpdater)
	followUpShim.real = consumer.NewFollowUpManagerWorker(bus, database.Pool, followUpManager)

	require.NoError(t, client.Start(ctx))
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = client.Stop(stopCtx)
	})

	return bus, emailConsumer
}

// deferredEmailWorker / deferredCadenceWorker / deferredFollowUpWorker mirror
// deferredRecorderWorker: register a River worker on the bundle before the
// real worker (which needs the bus, which needs the client) exists.
type deferredEmailWorker struct {
	river.WorkerDefaults[consumerjobs.EmailInteractionConsumerJobArgs]
	real *consumer.EmailInteractionConsumerWorker
}

func (w *deferredEmailWorker) Work(ctx context.Context, j *river.Job[consumerjobs.EmailInteractionConsumerJobArgs]) error {
	if w.real == nil {
		return fmt.Errorf("deferredEmailWorker invoked before real worker assignment")
	}
	return w.real.Work(ctx, j)
}

func (w *deferredEmailWorker) Timeout(j *river.Job[consumerjobs.EmailInteractionConsumerJobArgs]) time.Duration {
	if w.real == nil {
		return 30 * time.Second
	}
	return w.real.Timeout(j)
}

type deferredCadenceWorker struct {
	river.WorkerDefaults[consumerjobs.CadenceUpdaterJobArgs]
	real *consumer.CadenceUpdaterWorker
}

func (w *deferredCadenceWorker) Work(ctx context.Context, j *river.Job[consumerjobs.CadenceUpdaterJobArgs]) error {
	if w.real == nil {
		return fmt.Errorf("deferredCadenceWorker invoked before real worker assignment")
	}
	return w.real.Work(ctx, j)
}

func (w *deferredCadenceWorker) Timeout(j *river.Job[consumerjobs.CadenceUpdaterJobArgs]) time.Duration {
	if w.real == nil {
		return 30 * time.Second
	}
	return w.real.Timeout(j)
}

type deferredFollowUpWorker struct {
	river.WorkerDefaults[consumerjobs.FollowUpManagerJobArgs]
	real *consumer.FollowUpManagerWorker
}

func (w *deferredFollowUpWorker) Work(ctx context.Context, j *river.Job[consumerjobs.FollowUpManagerJobArgs]) error {
	if w.real == nil {
		return fmt.Errorf("deferredFollowUpWorker invoked before real worker assignment")
	}
	return w.real.Work(ctx, j)
}

func (w *deferredFollowUpWorker) Timeout(j *river.Job[consumerjobs.FollowUpManagerJobArgs]) time.Duration {
	if w.real == nil {
		return 30 * time.Second
	}
	return w.real.Timeout(j)
}

// deferredRematchWorker is the rematch counterpart to
// deferredRecorderWorker: lets us register a River worker on a workers
// bundle before the real worker exists (river.NewClient consumes the
// workers bundle at construction time but the real worker needs a bus,
// and the bus needs the client).
type deferredRematchWorker struct {
	river.WorkerDefaults[consumerjobs.RematchDispatcherJobArgs]
	real *consumer.RematchDispatcherWorker
}

func (w *deferredRematchWorker) Work(ctx context.Context, j *river.Job[consumerjobs.RematchDispatcherJobArgs]) error {
	if w.real == nil {
		return fmt.Errorf("deferredRematchWorker invoked before real worker assignment")
	}
	return w.real.Work(ctx, j)
}

func (w *deferredRematchWorker) Timeout(j *river.Job[consumerjobs.RematchDispatcherJobArgs]) time.Duration {
	if w.real == nil {
		return 5 * time.Minute
	}
	return w.real.Timeout(j)
}

// defaultInteractionWaitTimeout is the budget we give the async consumer
// to pick up an enqueued job and commit the interaction row. Generous to
// avoid flakes under CI load; integration tests that time out at this
// duration indicate a real wiring regression.
const defaultInteractionWaitTimeout = 30 * time.Second

// deferredRecorderWorker is a thin shim that lets us register a
// River worker on a workers bundle BEFORE the real worker exists —
// necessary because river.NewClient consumes the workers bundle at
// construction time but the real InteractionRecorderWorker needs a bus,
// and the bus needs the client. Work() delegates to whichever real
// worker has been assigned by the time jobs run.
type deferredRecorderWorker struct {
	river.WorkerDefaults[consumerjobs.InteractionRecorderJobArgs]
	real *consumer.InteractionRecorderWorker
}

func (w *deferredRecorderWorker) Work(ctx context.Context, j *river.Job[consumerjobs.InteractionRecorderJobArgs]) error {
	if w.real == nil {
		return fmt.Errorf("deferredRecorderWorker invoked before real worker assignment")
	}
	return w.real.Work(ctx, j)
}

func (w *deferredRecorderWorker) Timeout(j *river.Job[consumerjobs.InteractionRecorderJobArgs]) time.Duration {
	if w.real == nil {
		return 30 * time.Second
	}
	return w.real.Timeout(j)
}

// wireCadenceUpdaterForTest constructs a real CadenceUpdater against
// the given database and injects it into contactService so cadence
// entry points (RecordInteraction direct path, MergeContacts,
// ExtendInteraction, PromoteInteractionToMutual, UpdateContact cadence
// edits) work in tests without needing the full event-bus harness.
// Uses cutover mode so cadence columns get written.
func wireCadenceUpdaterForTest(t *testing.T, database *db.Database, contactService *service.ContactService) *consumer.CadenceUpdater {
	t.Helper()
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

// cadenceUpdaterNoopWorker satisfies the cadence_updater kind for test
// harnesses that don't exercise the real CadenceUpdater. Runs as a no-
// op so the job completes without DB side-effects. Tests that do want
// real cadence processing construct their own setup around the
// concrete CadenceUpdaterWorker.
type cadenceUpdaterNoopWorker struct {
	river.WorkerDefaults[consumerjobs.CadenceUpdaterJobArgs]
}

func (*cadenceUpdaterNoopWorker) Work(_ context.Context, _ *river.Job[consumerjobs.CadenceUpdaterJobArgs]) error {
	return nil
}

func (*cadenceUpdaterNoopWorker) Timeout(_ *river.Job[consumerjobs.CadenceUpdaterJobArgs]) time.Duration {
	return 30 * time.Second
}

// followUpManagerNoopWorker satisfies the followup_manager kind for
// test harnesses that don't exercise the real FollowUpManager. Same
// rationale as cadenceUpdaterNoopWorker.
type followUpManagerNoopWorker struct {
	river.WorkerDefaults[consumerjobs.FollowUpManagerJobArgs]
}

func (*followUpManagerNoopWorker) Work(_ context.Context, _ *river.Job[consumerjobs.FollowUpManagerJobArgs]) error {
	return nil
}

func (*followUpManagerNoopWorker) Timeout(_ *river.Job[consumerjobs.FollowUpManagerJobArgs]) time.Duration {
	return 30 * time.Second
}

// emailInteractionNoopWorker satisfies the email_interaction_consumer kind
// for harnesses that publish email.* but don't exercise the real email
// consumer (e.g. the Gmail provider sweep tests). Drains the job without DB
// side effects.
type emailInteractionNoopWorker struct {
	river.WorkerDefaults[consumerjobs.EmailInteractionConsumerJobArgs]
}

func (*emailInteractionNoopWorker) Work(_ context.Context, _ *river.Job[consumerjobs.EmailInteractionConsumerJobArgs]) error {
	return nil
}

func (*emailInteractionNoopWorker) Timeout(_ *river.Job[consumerjobs.EmailInteractionConsumerJobArgs]) time.Duration {
	return 30 * time.Second
}

// waitForInteractionBySourceRef polls InteractionRepository.FindBySourceRef
// until the row appears or the timeout elapses. Used after the publisher
// runs so the async InteractionRecorder consumer has time to commit.
func waitForInteractionBySourceRef(
	t *testing.T,
	ctx context.Context,
	repo *repository.InteractionRepository,
	contactID uuid.UUID,
	source string,
	sourceRef string,
	timeout time.Duration,
) *repository.Interaction {
	t.Helper()
	deadline := accelerated.GetCurrentTime().Add(timeout)
	var lastErr error
	for accelerated.GetCurrentTime().Before(deadline) {
		got, err := repo.FindBySourceRef(ctx, contactID, source, sourceRef)
		if err == nil {
			return got
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for interaction (contact=%s source=%s source_ref=%s): %v",
		contactID, source, sourceRef, lastErr)
	return nil
}

// waitForTelegramInteractionCount polls the DB for `want` telegram
// interactions attached to the contact (or timeout). Used by aggregation
// tests after publishing events.
func waitForTelegramInteractionCount(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	contactID uuid.UUID,
	want int,
	timeout time.Duration,
) int {
	t.Helper()
	deadline := accelerated.GetCurrentTime().Add(timeout)
	var last int
	for accelerated.GetCurrentTime().Before(deadline) {
		err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM interaction
			 WHERE contact_id = $1 AND source = 'telegram' AND deleted_at IS NULL`,
			contactID,
		).Scan(&last)
		require.NoError(t, err)
		if last >= want {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d telegram interactions (got %d) for contact=%s",
		want, last, contactID)
	return last
}

// waitForInteractionCountExact polls ListContactInteractions until
// exactly `want` rows are visible (or timeout). Returns the row slice
// so callers can assert on direction/source/etc. post-wait.
func waitForInteractionCountExact(
	t *testing.T,
	ctx context.Context,
	repo *repository.InteractionRepository,
	contactID uuid.UUID,
	want int,
	timeout time.Duration,
) []repository.Interaction {
	t.Helper()
	deadline := accelerated.GetCurrentTime().Add(timeout)
	var last []repository.Interaction
	for accelerated.GetCurrentTime().Before(deadline) {
		rows, err := repo.ListContactInteractions(ctx, contactID, 100, 0)
		require.NoError(t, err)
		last = rows
		if len(rows) == want {
			return rows
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for exactly %d interactions for contact=%s (got %d)",
		want, contactID, len(last))
	return last
}

// waitForInteractionDirection polls until the contact has an interaction
// with the given direction (useful for reply-bridge tests where
// outbound → mutual promotion is what we're waiting on).
func waitForInteractionDirection(
	t *testing.T,
	ctx context.Context,
	repo *repository.InteractionRepository,
	contactID uuid.UUID,
	direction string,
	timeout time.Duration,
) []repository.Interaction {
	t.Helper()
	deadline := accelerated.GetCurrentTime().Add(timeout)
	var last []repository.Interaction
	for accelerated.GetCurrentTime().Before(deadline) {
		rows, err := repo.ListContactInteractions(ctx, contactID, 100, 0)
		require.NoError(t, err)
		last = rows
		for _, r := range rows {
			if r.Direction == direction {
				return rows
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for direction=%s interaction for contact=%s (got %d rows)",
		direction, contactID, len(last))
	return last
}

// waitForTelegramMessagesProcessed polls until `want` telegram messages
// for the given peer are marked processed_at != NULL. In cutover the
// consumer marks these in the interaction-insert tx (plan Decision 10).
func waitForTelegramMessagesProcessed(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	peerUserID int64,
	contactID uuid.UUID,
	want int,
	timeout time.Duration,
) {
	t.Helper()
	deadline := accelerated.GetCurrentTime().Add(timeout)
	var last int
	for accelerated.GetCurrentTime().Before(deadline) {
		err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM telegram_message
			 WHERE peer_user_id = $1 AND matched_contact_id = $2
			   AND processed_at IS NOT NULL AND deleted_at IS NULL`,
			peerUserID, contactID,
		).Scan(&last)
		require.NoError(t, err)
		if last >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d processed messages (got %d) for peer=%d contact=%s",
		want, last, peerUserID, contactID)
}
