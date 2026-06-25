package replay

import (
	"context"
	"fmt"
	"regexp"
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

// NewHarnessWithDBForNamespace builds a harness without a *testing.T for an
// EXPLICIT namespace + seed (non-test callers — the crm-admin profile
// entrypoints). It lets the seed entrypoints pin a STABLE per-profile namespace
// (e.g. "prodshaped") + DefaultSeed so a reset reproduces a byte-identical world,
// rather than the time-derived namespace NewHarnessWithDB uses. The
// collision-checked re-salt still applies; on a freshly-reset DB the band is free
// so the stable namespace is used verbatim. Returns the harness + the quiesce/
// conditional-cleanup teardown closure + an error.
func NewHarnessWithDBForNamespace(ctx context.Context, database *db.Database, namespace string, seed uint64) (*Harness, func(context.Context) error, error) {
	return newHarness(ctx, database, namespace, seed)
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

// namespacePattern is the safe charset for a namespace token: lowercase
// alphanumerics and hyphens only. It deliberately excludes the SQL LIKE
// metacharacters `_` and `%` (and everything else), so a namespace can never
// over-match in the prefix-based cleanup deletes.
var namespacePattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// ValidateNamespace rejects a namespace token that contains characters outside
// the safe charset (notably the LIKE metacharacters `_` and `%`). Exported so the
// charset contract is directly unit-testable; NewHarness* call it at construction.
func ValidateNamespace(namespace string) error {
	if !namespacePattern.MatchString(namespace) {
		return fmt.Errorf("synthetic: invalid namespace %q — must match %s (no SQL LIKE metacharacters)", namespace, namespacePattern.String())
	}
	return nil
}

func newHarness(ctx context.Context, database *db.Database, namespace string, seed uint64) (*Harness, func(context.Context) error, error) {
	// Reject namespaces with characters outside the safe charset. Cleanup deletes
	// by `LIKE 'synth-<ns>-%'`, so a namespace containing a LIKE metacharacter
	// (`_` or `%`) would over-match and could wipe another namespace's rows;
	// restricting the token to [a-z0-9-] makes that impossible by construction.
	if err := ValidateNamespace(namespace); err != nil {
		return nil, nil, err
	}

	cfg := config.TestConfig()
	if cfg.River.WorkerConcurrency <= 0 {
		cfg.River.WorkerConcurrency = 4
	}
	// The MessageHandler size threshold tracks the test config's
	// TELEGRAM_GROUP_MAX_MEMBERS rather than a hard-coded copy of the production
	// default, so a config change does not silently leave the harness on a stale
	// boundary. Defend against a zero/unset value.
	groupMaxMembers := cfg.Telegram.GroupMaxMembers
	if groupMaxMembers <= 0 {
		groupMaxMembers = 10
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

	// Setup-time peer-band collision detection (D5): derive the generator and
	// re-salt the namespace if its telegram peer sub-block is already occupied by
	// a different namespace's live rows. Probabilistically disjoint, with this
	// detection it is "disjoint + detected at setup," not a hard guarantee.
	gen, namespace, err := resolveNamespace(ctx, support, namespace, seed)
	if err != nil {
		return nil, nil, err
	}

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

	// Venue resolver: populates interaction.venue_id so replay adapters can
	// assert a venue node was created for each venue-bearing source. Mirrors the
	// main.go wiring.
	venueRepo := repository.NewVenueRepository(database.Queries)
	venueResolver := repository.NewVenueResolverRegistry(
		venueRepo,
		map[string]repository.VenueContainerReader{
			repository.InteractionSourceTelegram: repository.NewTelegramVenueContainerReader(),
			repository.InteractionSourceMessages: repository.NewMessagesVenueContainerReader(),
			repository.InteractionSourceGChat:    repository.NewGChatVenueContainerReader(),
		},
		calendarEventRepo,
	)

	recorder := consumer.NewInteractionRecorder(contactService, stagingRegistry, bus, cadenceUpdater, nil, calendarEventRepo)
	recorder.SetVenueResolver(venueResolver)
	recorderShim.real = consumer.NewInteractionRecorderWorker(bus, database.Pool, recorder, nil)
	cadenceShim.real = consumer.NewCadenceUpdaterWorker(bus, database.Pool, cadenceUpdater)

	// Calendar decline handler: calendar.declined routes to a distinct River job
	// (CalendarDeclineHandlerJobArgs) that soft-deletes the derived gcal
	// interaction. Without a registered worker the enqueued job never finalizes,
	// so a decline replay's Gate B (and the harness teardown) would stall. Mirrors
	// the cmd/crm-api wiring; inert for replays that never decline an event.
	calendarDeclineHandler := consumer.NewCalendarDeclineHandler(interactionRepo, contactRepo)
	river.AddWorker(workers, consumer.NewCalendarDeclineHandlerWorker(bus, database.Pool, calendarDeclineHandler))

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
	emailConsumer.SetVenueResolver(venueRepo)
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

	h := &Harness{
		ctx:             ctx,
		database:        database,
		bus:             bus,
		client:          client,
		namespace:       namespace,
		gen:             gen,
		contactRepo:     contactRepo,
		methodRepo:      methodRepo,
		interactionRepo: interactionRepo,
		commsRepo:       commsRepo,
		externalRepo:    externalRepo,
		telegramRepo:    telegramRepo,
		messagesRepo:    messagesRepo,
		venueRepo:       venueRepo,
		identityService: identityService,
		contactService:  contactService,
		cadenceUpdater:  cadenceUpdater,
		support:         support,
		ingestService:   ingestService,
		macHostID:       macHostID,
		peerMatcher:     &telegramPeerMatcherDeps{matcher: peerMatcher, engine: tgAggEngine},
		groupMaxMembers: groupMaxMembers,
		created:         newCreated(),
	}
	_ = hostRepo // reserved for future liveness wiring; intentionally nil here

	teardown := func(stopCtx context.Context) error {
		return h.teardown(stopCtx)
	}
	return h, teardown, nil
}

// bandResaltAttempts bounds how many times resolveNamespace re-salts a colliding
// namespace before failing loudly.
const bandResaltAttempts = 8

// resolveNamespace builds the generator and, if this namespace's telegram peer
// sub-block OR synthetic-phone area code is already occupied by another
// namespace's live rows, re-salts the namespace (appending an incrementing
// suffix) until it finds free bands or exhausts the attempt budget. Both numeric
// bands are checked because both are matched DB-wide with no namespace scoping.
// The phone check queries BOTH contact_method (where a seeded synthetic phone
// lives — the primary cross-match origin) AND external_identity (where a replay
// later mints the identity). The practical guarantee is "probabilistically
// disjoint + detected at setup," not a hard mathematical guarantee.
func resolveNamespace(ctx context.Context, support *repository.SyntheticSupportRepository, namespace string, seed uint64) (*factory.Generator, string, error) {
	candidate := namespace
	for attempt := 0; attempt < bandResaltAttempts; attempt++ {
		gen := factory.NewGenerator(seed, candidate)
		peerOccupied, err := support.CountTelegramMessagesInPeerBand(ctx, gen.PeerBandStart(), gen.PeerBandEnd())
		if err != nil {
			return nil, "", fmt.Errorf("peer-band collision check: %w", err)
		}
		// Group chat ids are drawn from the SAME peer band; telegram_chat_config
		// keys on telegram_chat_id with no namespace column, so a leftover config
		// row in this band (e.g. from a crashed prior run whose cleanup never ran)
		// must also trigger a re-salt.
		chatConfigOccupied, err := support.CountTelegramChatConfigInChatIdBand(ctx, gen.PeerBandStart(), gen.PeerBandEnd())
		if err != nil {
			return nil, "", fmt.Errorf("chat-config band collision check: %w", err)
		}
		// Telegram discovery/stranded replays create external_contact +
		// external_identity rows keyed by a bare peer-id source_id in this band. A
		// crashed prior run can leave them with no telegram_message row, so the
		// telegram_message check above would miss them — check them too.
		barePeerOccupied, err := support.CountTelegramBarePeerRowsInBand(ctx, gen.PeerBandStart(), gen.PeerBandEnd())
		if err != nil {
			return nil, "", fmt.Errorf("bare-peer band collision check: %w", err)
		}
		phonePrefix := gen.SyntheticPhonePrefix()
		methodPhones, err := support.CountContactMethodsByValueNormalizedPrefix(ctx, phonePrefix)
		if err != nil {
			return nil, "", fmt.Errorf("phone-band collision check (contact_method): %w", err)
		}
		identityPhones, err := support.CountExternalIdentitiesByIdentifierPrefix(ctx, phonePrefix)
		if err != nil {
			return nil, "", fmt.Errorf("phone-band collision check (external_identity): %w", err)
		}
		if peerOccupied == 0 && chatConfigOccupied == 0 && barePeerOccupied == 0 && methodPhones == 0 && identityPhones == 0 {
			return gen, candidate, nil
		}
		// Collision in either band: re-salt and retry.
		candidate = fmt.Sprintf("%s-s%d", namespace, attempt+1)
	}
	return nil, "", fmt.Errorf("synthetic: could not find free numeric bands for namespace %q after %d re-salts", namespace, bandResaltAttempts)
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
