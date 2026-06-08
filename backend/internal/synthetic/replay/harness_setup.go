package replay

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
	"personal-crm/backend/internal/google"
	"personal-crm/backend/internal/messages"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/scheduler"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/telegram"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
)

// NewHarness builds a replay harness for a test. ctx is MANDATORY and is the
// exact context passed to client.Start (NOT a timeout-derived ctx — River
// silently stops fetching if its Start ctx cancels). It registers a t.Cleanup
// closure that stops the client, bounded-waits Gate B, and gates the ENTIRE
// cleanup on Gate B == 0 (leaving the namespaced dataset intact when unsettled).
func NewHarness(t *testing.T, ctx context.Context, database *db.Database) *Harness {
	t.Helper()
	h, teardown, err := NewHarnessWithDB(ctx, database)
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := teardown(context.Background()); err != nil {
			t.Logf("synthetic harness teardown error (non-fatal): %v", err)
		}
	})
	return h
}

// NewHarnessWithDB builds a replay harness without a *testing.T (for non-test
// callers — future entrypoints/staging). It returns an error because building/
// starting the River client, wiring repos, and seeding the synthetic Mac host
// can all fail. The returned closure is the quiesce + conditional-cleanup
// teardown (stops the client, bounded-waits Gate B, gates the whole cleanup on
// Gate B == 0). The namespace defaults to a stable token derived from the
// current time so concurrent harnesses do not collide.
func NewHarnessWithDB(ctx context.Context, database *db.Database) (*Harness, func(context.Context) error, error) {
	return newHarness(ctx, database, defaultNamespace(), factory.DefaultSeed)
}

// NewHarnessForNamespace builds a harness with an explicit namespace + seed.
// Tests use this to give each sub-test a unique namespace so shared-test-DB
// reuse cannot collide.
func NewHarnessForNamespace(t *testing.T, ctx context.Context, database *db.Database, namespace string, seed uint64) *Harness {
	t.Helper()
	h, teardown, err := newHarness(ctx, database, namespace, seed)
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := teardown(context.Background()); err != nil {
			t.Logf("synthetic harness teardown error (non-fatal): %v", err)
		}
	})
	return h
}

func defaultNamespace() string {
	return fmt.Sprintf("h%d", accelerated.GetCurrentTime().UnixNano())
}

func newHarness(ctx context.Context, database *db.Database, namespace string, seed uint64) (*Harness, func(context.Context) error, error) {
	cfg := config.TestConfig()
	if cfg.River.WorkerConcurrency <= 0 {
		cfg.River.WorkerConcurrency = 4
	}

	// Repositories.
	eventRepo := repository.NewEventRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	methodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	commsRepo := repository.NewCommsMessageRepository(database.Queries)
	externalRepo := repository.NewExternalContactRepository(database.Queries)
	telegramRepo := repository.NewTelegramMessageRepository(database.Queries)
	messagesRepo := repository.NewMessagesMessageRepository(database.Queries)
	identityRepo := repository.NewIdentityRepository(database.Queries)
	claimRepo := repository.NewEventConsumerClaimRepository(database.Queries)
	hostRepo := repository.NewMacHostRepository(database.Queries)
	calendarEventRepo := repository.NewCalendarEventRepository(database.Queries)
	support := repository.NewSyntheticSupportRepository(database.Queries)

	identityService := service.NewIdentityService(identityRepo)
	// Contact service built with nil bus first; the real bus is injected after
	// the client/bus exist (chicken-and-egg, mirroring the canonical harness).
	contactService := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, contactTaskRepo, nil, nil)

	// River workers: the deferred-shim construction order (bus needs client; the
	// real workers need bus). Real workers: interaction_recorder, cadence_updater,
	// email_interaction_consumer, messaging_aggregate_for_contact (engines for
	// source=messages + gchat), followup_manager (off mode). rematch_dispatcher
	// is a no-op (rematch is not part of the inbound replay graph for Element 1).
	workers := river.NewWorkers()
	recorderShim := &deferredRecorderWorker{}
	cadenceShim := &deferredCadenceWorker{}
	emailShim := &deferredEmailWorker{}
	followUpShim := &deferredFollowUpWorker{}
	river.AddWorker(workers, recorderShim)
	river.AddWorker(workers, cadenceShim)
	river.AddWorker(workers, emailShim)
	river.AddWorker(workers, followUpShim)
	river.AddWorker(workers, &rematchNoopWorker{})

	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers:  workers,
		TestOnly: true, // immediate processing, no staggered maintenance
	})
	if err != nil {
		return nil, nil, fmt.Errorf("build river client: %w", err)
	}

	bus := events.NewBus(database.Pool, client, eventRepo)
	contactService.InjectBusForTest(bus)

	// Cadence updater (cutover) wired into the contact service.
	cadenceUpdater := consumer.NewCadenceUpdater(claimRepo, contactRepo, database.Queries, consumer.CadenceModeCutover, false)
	contactService.SetCadenceUpdater(cadenceUpdater)

	// Staging registry covers telegram + messages + gchat sources. The gchat
	// session processor is REQUIRED: without it the InteractionRecorder cannot
	// mark comms_message(source='gchat') rows processed, the zero-rows rollback
	// fires, and the aggregation engine reprocesses forever (Gate B never clears).
	stagingRegistry := repository.NewStagingProcessorRegistry(map[string]repository.StagingProcessor{
		repository.InteractionSourceTelegram: repository.NewTelegramStagingProcessor(telegramRepo),
		repository.InteractionSourceMessages: repository.NewMessagesStagingProcessor(messagesRepo),
		repository.InteractionSourceGChat:    repository.NewCommsSessionStagingProcessor(commsRepo),
	})

	recorder := consumer.NewInteractionRecorder(contactService, stagingRegistry, bus, cadenceUpdater, nil, calendarEventRepo)
	recorderShim.real = consumer.NewInteractionRecorderWorker(bus, database.Pool, recorder, nil)
	cadenceShim.real = consumer.NewCadenceUpdaterWorker(bus, database.Pool, cadenceUpdater)

	// Off-mode FollowUpManager: cutover-only Todoist deps are nil.
	followUpManager := consumer.NewFollowUpManager(
		consumer.FollowUpModeOff,
		claimRepo, contactRepo, nil, nil, interactionRepo, nil,
		database.Pool, nil, nil, "", cfg.Watchdog,
	)
	followUpShim.real = consumer.NewFollowUpManagerWorker(bus, database.Pool, followUpManager)

	// Email interaction consumer (the REAL worker so Gmail settles to interactions).
	emailConsumer := consumer.NewEmailInteractionConsumer(
		contactService, commsRepo, interactionRepo, contactService,
		bus, cadenceUpdater, followUpManager,
	)
	emailShim.real = consumer.NewEmailInteractionConsumerWorker(bus, database.Pool, emailConsumer)

	// Messaging aggregate worker: engines for source=messages (iMessage) +
	// source=gchat, with the chat-lister registry. Mirrors main.go wiring.
	messagesEnqueuer := consumer.NewRiverInteractionRecorderEnqueuer(client)
	messagesEngine := messages.NewAggregationEngine(
		4, 48, messagesRepo, interactionRepo, contactService, contactService, bus, database.Pool, messagesEnqueuer,
	)
	gchatEnqueuer := consumer.NewRiverInteractionRecorderEnqueuer(client)
	gchatEngine := google.NewGChatAggregationEngine(
		2, 48, commsRepo, interactionRepo, contactService, contactService, bus, database.Pool, gchatEnqueuer,
	)
	chatListerRegistry := scheduler.NewPerSourceChatListerRegistry(
		map[string]func(ctx context.Context, contactID uuid.UUID) ([]string, error){
			repository.InteractionSourceMessages: messagesRepo.ListUnprocessedChatsByContact,
			repository.InteractionSourceGChat: func(ctx context.Context, contactID uuid.UUID) ([]string, error) {
				return commsRepo.ListUnprocessedChatsByContactForSource(ctx, repository.InteractionSourceGChat, contactID)
			},
		},
	)
	river.AddWorker(workers, scheduler.NewMessagingAggregateForContactWorker(
		map[string]scheduler.ChatAwareAggregator{
			repository.InteractionSourceMessages: messagesEngine,
			repository.InteractionSourceGChat:    gchatEngine,
		},
		chatListerRegistry,
	))

	// IngestService: REVOKED synthetic host + hostLiveness=nil + the harness
	// riverClient (so the iMessage messaging-aggregate enqueue succeeds).
	macHostID, err := support.SeedRevokedMacHost(ctx, factory.SyntheticSourcePrefix+namespace+"-host")
	if err != nil {
		return nil, nil, fmt.Errorf("seed revoked mac host: %w", err)
	}
	ingestService := service.NewIngestService(
		database, bus, identityService, messagesRepo, client, externalRepo,
		nil, // hostLiveness = nil: skips the active-host re-check + dodges the singleton
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)

	// Telegram peer matcher + aggregation engine for the telegram adapter.
	peerMatcher := telegram.NewPeerMatcher(identityService, telegramRepo, externalRepo, nil, 3)
	tgAggEngine := telegram.NewAggregationEngine(
		2, 48, telegramRepo, interactionRepo, contactService, contactService, bus, database.Pool, nil,
	)

	// IMPORTANT: pass the OUTER ctx (not a timeout-derived one) to Start.
	if err := client.Start(ctx); err != nil {
		return nil, nil, fmt.Errorf("start river client: %w", err)
	}

	gen := factory.NewGenerator(seed, namespace)

	h := &Harness{
		ctx:               ctx,
		database:          database,
		bus:               bus,
		client:            client,
		namespace:         namespace,
		gen:               gen,
		contactRepo:       contactRepo,
		methodRepo:        methodRepo,
		interactionRepo:   interactionRepo,
		commsRepo:         commsRepo,
		externalRepo:      externalRepo,
		telegramRepo:      telegramRepo,
		messagesRepo:      messagesRepo,
		identityService:   identityService,
		contactService:    contactService,
		cadenceUpdater:    cadenceUpdater,
		support:           support,
		ingestService:     ingestService,
		macHostID:         macHostID,
		peerMatcher:       &telegramPeerMatcherDeps{matcher: peerMatcher, engine: tgAggEngine},
		telegramAggEngine: tgAggEngine,
		created:           newCreated(),
	}
	_ = hostRepo // reserved for future liveness wiring; intentionally nil here

	teardown := func(stopCtx context.Context) error {
		return h.teardown(stopCtx)
	}
	return h, teardown, nil
}

// rematchNoopWorker drains the rematch_dispatcher kind (rematch is out of the
// Element-1 inbound replay graph; pending states come from the unknown-sender
// path, not a rematch pass).
type rematchNoopWorker struct {
	river.WorkerDefaults[consumerjobs.RematchDispatcherJobArgs]
}

func (*rematchNoopWorker) Work(_ context.Context, _ *river.Job[consumerjobs.RematchDispatcherJobArgs]) error {
	return nil
}

func (*rematchNoopWorker) Timeout(_ *river.Job[consumerjobs.RematchDispatcherJobArgs]) time.Duration {
	return 30 * time.Second
}
