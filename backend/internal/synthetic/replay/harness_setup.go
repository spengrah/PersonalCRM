package replay

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
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
	"personal-crm/backend/internal/whatsapp"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

// ErrConstructorResidue marks a harness-construction failure that LEFT the
// namespace's synthetic host row behind. newHarness removes that row
// best-effort on its own post-host failure paths; when the removal itself
// fails, the returned error wraps this sentinel so a caller can report the
// residue truthfully (errors.Is) rather than guessing. The residue is still
// reachable — the declared-seed cleanup path finds a host-only world from the
// requested namespace token.
var ErrConstructorResidue = errors.New("synthetic: harness construction left namespaced rows behind")

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
	return newHarness(ctx, database, defaultNamespace(), factory.DefaultSeed, accelerated.GetCurrentTime())
}

// NewHarnessWithDBForNamespace builds a harness without a *testing.T for an
// EXPLICIT namespace + seed (non-test callers — the crm-admin profile
// entrypoints). It lets the seed entrypoints pin a STABLE per-profile namespace
// (e.g. "standard") + DefaultSeed so a reset reproduces a byte-identical world,
// rather than the time-derived namespace NewHarnessWithDB uses. The
// collision-checked re-salt still applies; on a freshly-reset DB the band is free
// so the stable namespace is used verbatim. Returns the harness + the quiesce/
// conditional-cleanup teardown closure + an error.
func NewHarnessWithDBForNamespace(ctx context.Context, database *db.Database, namespace string, seed uint64) (*Harness, func(context.Context) error, error) {
	return newHarness(ctx, database, namespace, seed, accelerated.GetCurrentTime())
}

// NewHarnessForNamespace builds a harness with an explicit namespace + seed.
// Tests use this to give each sub-test a unique namespace so shared-test-DB
// reuse cannot collide.
func NewHarnessForNamespace(t *testing.T, ctx context.Context, database *db.Database, namespace string, seed uint64) *Harness {
	t.Helper()
	h, teardown, err := newHarness(ctx, database, namespace, seed, accelerated.GetCurrentTime())
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

func newHarness(ctx context.Context, database *db.Database, namespace string, seed uint64, anchor time.Time) (*Harness, func(context.Context) error, error) {
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
	gen, namespace, err := resolveNamespace(ctx, support, namespace, seed, anchor)
	if err != nil {
		return nil, nil, err
	}

	identityService := service.NewIdentityService(identityRepo)

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
	// assertion.accepted/superseded route a knowledge_cache_updater job (the
	// location/birthday/how_met authority flip); register the kind so a seeded
	// contact-with-knowledge publish enqueues legally. The cache is filled inline
	// via ContactService's RefreshTx, so a no-op async worker is sufficient here.
	river.AddWorker(workers, &knowledgeCacheNoopWorker{})

	// QUEUE ISOLATION (see SyntheticQueueName): this client fetches ONLY the
	// namespace's private queue, and an insert hook rewrites every job it
	// enqueues onto that same queue. Both halves are required and neither is
	// optional — see the doc comment on syntheticQueueRouter.
	queueName := SyntheticQueueName(namespace)
	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			queueName: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers:  workers,
		Hooks:    []rivertype.Hook{&syntheticQueueRouter{queue: queueName}},
		TestOnly: true, // immediate processing, no staggered maintenance
	})
	if err != nil {
		return nil, nil, fmt.Errorf("build river client: %w", err)
	}

	bus := events.NewBus(database.Pool, client, eventRepo)

	// Cadence updater (cutover) — passed to NewContactService as a ctor arg.
	cadenceUpdater := consumer.NewCadenceUpdater(claimRepo, contactRepo, database.Queries, consumer.CadenceModeCutover, false)

	// Knowledge writer (location/birthday/how_met authority flip): the contact
	// service emits lives_in/birthday/how_met assertions through AssertService and
	// refreshes the derived cache columns inline via KnowledgeCacheUpdater. Both
	// are passed to NewContactService as the knowledge-writer ctor pair.
	graphNodeRepo := repository.NewNodeRepository(database.Queries)
	graphEntityRepo := repository.NewEntityRepository(database.Queries)
	graphPredicateRepo := repository.NewPredicateRepository(database.Queries)
	graphAssertionRepo := repository.NewAssertionRepository(database.Queries)
	assertService := service.NewAssertService(database.Pool, graphNodeRepo, graphEntityRepo, graphPredicateRepo, graphAssertionRepo, bus)
	knowledgeCache := consumer.NewKnowledgeCacheUpdater(graphAssertionRepo, graphNodeRepo, contactRepo)

	// Contact service — built AFTER cadence + assertService + knowledgeCache so
	// they pass as ctor args (INV-5 order). followUp is deliberately nil (this
	// harness never wires non-bus follow-up work). The real bus is injected after
	// construction because it needs the client (chicken-and-egg), mirroring the
	// canonical harness.
	contactService := service.NewContactService(
		database, contactRepo, methodRepo, interactionRepo, contactTaskRepo, nil, nil,
		cadenceUpdater, assertService, knowledgeCache, nil,
	)
	contactService.InjectBusForTest(bus)

	// Merge-time task-close enqueuer. Replay merge scenarios seed STANDALONE
	// contacts (no cadence tasks), so no enqueue-eligible refs exist today;
	// wiring with remoteCloseEnabled=false (mirroring this harness's
	// follow-up mode: off) keeps a future profile that merges a task-bearing
	// contact on the safe WARN-and-skip path instead of erroring.
	contactService.SetTaskCloseEnqueuer(client, false)

	// Staging registry covers telegram + messages + gchat sources. The gchat
	// session processor is REQUIRED: without it the InteractionRecorder cannot
	// mark comms_message(source='gchat') rows processed, the zero-rows rollback
	// fires, and the aggregation engine reprocesses forever (Gate B never clears).
	stagingRegistry := repository.NewStagingProcessorRegistry(map[string]repository.StagingProcessor{
		repository.InteractionSourceTelegram: repository.NewTelegramStagingProcessor(telegramRepo),
		repository.InteractionSourceMessages: repository.NewMessagesStagingProcessor(messagesRepo),
		repository.InteractionSourceGChat:    repository.NewCommsSessionStagingProcessor(commsRepo),
		repository.InteractionSourceWhatsApp: repository.NewCommsSessionStagingProcessor(commsRepo),
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
			repository.InteractionSourceWhatsApp: repository.NewWhatsAppVenueContainerReader(),
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
		nil, "", cfg.Watchdog,
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
		google.GChatBurstWindowHours, google.GChatReplyBridgeHours,
		commsRepo, interactionRepo, contactService, contactService, bus, database.Pool, gchatEnqueuer,
	)
	// WhatsApp: the REAL ingest trio (repo → gate → matcher → ingestor),
	// reproducing buildWhatsAppIngestor, plus its aggregation engine. The
	// adapter drives the ingestor; the worker drives the engine.
	whatsappRepo := repository.NewWhatsAppRepository(database.Queries)
	// One reading of the threshold feeds BOTH the matcher and the accessor a
	// fixture sizes itself by, so the two can never disagree about how many
	// unmatched messages mint a candidate.
	whatsappDiscoveryMinMsgs := cfg.WhatsApp.DiscoveryMinMessages
	whatsappIngestor := whatsapp.NewIngestor(
		commsRepo,
		whatsapp.NewChatGate(whatsappRepo, cfg.WhatsApp.GroupMaxMembers),
		// enricher nil: EnrichmentService is not constructed in this package and
		// PeerMatcher tolerates a nil one.
		whatsapp.NewPeerMatcher(identityService, commsRepo, externalRepo, nil, whatsappDiscoveryMinMsgs),
	)
	whatsappEnqueuer := consumer.NewRiverInteractionRecorderEnqueuer(client)
	whatsappEngine := whatsapp.NewAggregationEngine(
		cfg.WhatsApp.BurstWindowHours, cfg.WhatsApp.ReplyBridgeHours,
		commsRepo, interactionRepo, contactService, contactService, bus, database.Pool, whatsappEnqueuer,
	)

	chatListerRegistry := scheduler.NewPerSourceChatListerRegistry(
		map[string]func(ctx context.Context, contactID uuid.UUID) ([]string, error){
			repository.InteractionSourceMessages: messagesRepo.ListUnprocessedChatsByContact,
			repository.InteractionSourceGChat: func(ctx context.Context, contactID uuid.UUID) ([]string, error) {
				return commsRepo.ListUnprocessedChatsByContactForSource(ctx, repository.InteractionSourceGChat, contactID)
			},
			repository.InteractionSourceWhatsApp: func(ctx context.Context, contactID uuid.UUID) ([]string, error) {
				return commsRepo.ListUnprocessedChatsByContactForSource(ctx, repository.InteractionSourceWhatsApp, contactID)
			},
		},
	)
	river.AddWorker(workers, scheduler.NewMessagingAggregateForContactWorker(
		map[string]scheduler.ChatAwareAggregator{
			repository.InteractionSourceMessages: messagesEngine,
			repository.InteractionSourceGChat:    gchatEngine,
			repository.InteractionSourceWhatsApp: whatsappEngine,
		},
		chatListerRegistry,
	))

	// IngestService: REVOKED synthetic host + hostLiveness=nil + the harness
	// riverClient (so the iMessage messaging-aggregate enqueue succeeds).
	macHostID, err := support.SeedRevokedMacHost(ctx, factory.SyntheticSourcePrefix+namespace+"-host")
	if err != nil {
		return nil, nil, fmt.Errorf("seed revoked mac host: %w", err)
	}

	// From here on the namespace is OCCUPIED: the host row is the marker that
	// callers use to detect an in-use namespace. A constructor failure past this
	// point returns no teardown closure (there is no harness yet), so without a
	// best-effort removal here the marker would claim the namespace forever with
	// nothing behind it. When the removal itself fails the error is wrapped with
	// ErrConstructorResidue so the caller can report the residue HONESTLY
	// instead of assuming either outcome.
	postHostFailure := func(cause error) (*Harness, func(context.Context) error, error) {
		if _, delErr := support.DeleteMacHostByID(ctx, macHostID); delErr != nil {
			return nil, nil, fmt.Errorf("%w: removing the namespaced synthetic host after a constructor failure failed (%v); original failure: %w",
				ErrConstructorResidue, delErr, cause)
		}
		return nil, nil, cause
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
		return postHostFailure(fmt.Errorf("start river client: %w", err))
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
		whatsapp:        &whatsappDeps{ingestor: whatsappIngestor, discoveryMinMessages: whatsappDiscoveryMinMsgs},
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
func resolveNamespace(ctx context.Context, support *repository.SyntheticSupportRepository, namespace string, seed uint64, anchor time.Time) (*factory.Generator, string, error) {
	candidate := namespace
	for attempt := 0; attempt < bandResaltAttempts; attempt++ {
		gen := factory.NewGeneratorAt(seed, candidate, anchor)
		free, err := BandsFree(ctx, support, gen)
		if err != nil {
			return nil, "", err
		}
		if free {
			return gen, candidate, nil
		}
		// Collision in either band: re-salt and retry.
		candidate = fmt.Sprintf("%s-s%d", namespace, attempt+1)
	}
	return nil, "", fmt.Errorf("synthetic: could not find free numeric bands for namespace %q after %d re-salts", namespace, bandResaltAttempts)
}

// BandsFree reports whether every numeric band this generator would draw from is
// unoccupied. It is THE definition of band occupancy for the whole toolkit:
// setup-time re-salting asks it, and so does the declared runner's revalidation
// after the re-salt lock swap. Those two once kept hand-maintained copies of the
// same list and drifted apart by one predicate, so they share this function now —
// a band that only one of them can see is a collision nothing detects.
//
// Every band here is matched DB-WIDE with no namespace scoping, which is why
// occupancy has to be checked at all rather than assumed from the namespace.
func BandsFree(ctx context.Context, support *repository.SyntheticSupportRepository, gen *factory.Generator) (bool, error) {
	peerOccupied, err := support.CountTelegramMessagesInPeerBand(ctx, gen.PeerBandStart(), gen.PeerBandEnd())
	if err != nil {
		return false, fmt.Errorf("peer-band collision check: %w", err)
	}
	// Group chat ids are drawn from the SAME peer band; telegram_chat_config
	// keys on telegram_chat_id with no namespace column, so a leftover config
	// row in this band (e.g. from a crashed prior run whose cleanup never ran)
	// must also trigger a re-salt.
	chatConfigOccupied, err := support.CountTelegramChatConfigInChatIdBand(ctx, gen.PeerBandStart(), gen.PeerBandEnd())
	if err != nil {
		return false, fmt.Errorf("chat-config band collision check: %w", err)
	}
	// Telegram discovery/stranded replays create external_contact +
	// external_identity rows keyed by a bare peer-id source_id in this band. A
	// crashed prior run can leave them with no telegram_message row, so the
	// telegram_message check above would miss them — check them too.
	barePeerOccupied, err := support.CountTelegramBarePeerRowsInBand(ctx, gen.PeerBandStart(), gen.PeerBandEnd())
	if err != nil {
		return false, fmt.Errorf("bare-peer band collision check: %w", err)
	}
	phonePrefix := gen.SyntheticPhonePrefix()
	methodPhones, err := support.CountContactMethodsByValueNormalizedPrefix(ctx, phonePrefix)
	if err != nil {
		return false, fmt.Errorf("phone-band collision check (contact_method): %w", err)
	}
	identityPhones, err := support.CountExternalIdentitiesByIdentifierPrefix(ctx, phonePrefix)
	if err != nil {
		return false, fmt.Errorf("phone-band collision check (external_identity): %w", err)
	}
	// A declared import candidate is not a contact and its direct-upsert
	// writer creates no identity, so its phones live ONLY in the
	// external_contact JSON — invisible to both checks above. The import
	// matcher still scores them against contact_method rows DB-wide, so a
	// later namespace that reused this area code would suggest a foreign
	// contact for this namespace's candidate.
	candidatePhones, err := support.CountExternalContactPhonesInBand(ctx, phonePrefix)
	if err != nil {
		return false, fmt.Errorf("phone-band collision check (external_contact phones): %w", err)
	}
	return peerOccupied == 0 && chatConfigOccupied == 0 && barePeerOccupied == 0 &&
		methodPhones == 0 && identityPhones == 0 && candidatePhones == 0, nil
}

// syntheticQueuePrefix names every queue a replay harness owns. Cleanup and the
// isolation tests key off it, so it is a constant rather than a literal.
const syntheticQueuePrefix = "synthetic-"

// SyntheticQueueName is the PRIVATE River queue a harness works for a namespace.
//
// Why a private queue rather than a per-worker ownership check. A harness starts
// a real River client, and River's fetch selects ANY available job in the queues
// it is configured for — there is no kind or namespace filter. Sharing `default`
// with the live application therefore has three distinct failure modes, and no
// per-worker guard covers all of them:
//   - a no-op worker (rematch_dispatcher, knowledge_cache_updater) FINALIZES a
//     foreign job without doing its work — silently lost work;
//   - a real worker wired for replay semantics (followup_manager, which this
//     harness deliberately runs in OFF mode) finalizes a foreign job having
//     skipped the work it was enqueued for;
//   - a production kind the harness does not register at all is fetched and
//     failed as an unknown kind, burning the job's attempts until it discards.
//
// Fetching only this queue closes all three at once, and the insert hook that
// routes the harness's own jobs here closes the mirror image: the application's
// client, which fetches only `default`, can never take a seed's job either.
//
// The name is DERIVED, not random, so a later cleanup — which has no handle on
// the client that made it — can find the queue's leftovers. FNV-1a of the
// namespace keeps it inside River's queue-name grammar (lowercase alphanumerics
// with single separators, ≤64 chars) for any namespace, which a sanitized
// namespace would not be: the toolkit charset admits leading, trailing and
// doubled hyphens.
func SyntheticQueueName(namespace string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(namespace))
	return fmt.Sprintf("%s%016x", syntheticQueuePrefix, h.Sum64())
}

// syntheticQueueRouter forces every job this harness inserts onto its private
// queue, whatever queue the job args or insert opts asked for.
//
// It is the other half of the isolation: a client that only FETCHES its private
// queue would still leave its own jobs on `default`, where the live application
// would work them with production wiring the seed did not ask for (the harness
// runs follow-up in off mode precisely so a seed does not create real
// follow-ups). River runs insert hooks on every insert path — Insert, InsertTx,
// InsertMany, InsertManyTx, and the fast variants all funnel through the same
// shared insert — and builds the final insert params AFTER the hooks, so
// rewriting Queue here is sufficient and complete.
type syntheticQueueRouter struct {
	river.HookDefaults
	queue string
}

func (r *syntheticQueueRouter) InsertBegin(_ context.Context, params *rivertype.JobInsertParams) error {
	params.Queue = r.queue
	return nil
}

// rematchNoopWorker drains the rematch_dispatcher kind: rematch is out of the
// Element-1 inbound replay graph (pending states come from the unknown-sender
// path, not a rematch pass), so the job only has to finalize. It can no-op
// unconditionally because queue isolation guarantees the job is this harness's
// own — see SyntheticQueueName.
type rematchNoopWorker struct {
	river.WorkerDefaults[consumerjobs.RematchDispatcherJobArgs]
}

func (*rematchNoopWorker) Work(_ context.Context, _ *river.Job[consumerjobs.RematchDispatcherJobArgs]) error {
	return nil
}

func (*rematchNoopWorker) Timeout(_ *river.Job[consumerjobs.RematchDispatcherJobArgs]) time.Duration {
	return 30 * time.Second
}

// knowledgeCacheNoopWorker registers the knowledge_cache_updater kind so the
// harness's River client accepts the assertion.accepted/superseded enqueues that
// a seeded contact's location/birthday/how_met assertions produce. The cache is
// filled inline by ContactService, so the async worker is a no-op — and, by the
// same queue-isolation argument, only ever sees this harness's own jobs.
type knowledgeCacheNoopWorker struct {
	river.WorkerDefaults[consumerjobs.KnowledgeCacheUpdaterJobArgs]
}

func (*knowledgeCacheNoopWorker) Work(_ context.Context, _ *river.Job[consumerjobs.KnowledgeCacheUpdaterJobArgs]) error {
	return nil
}

func (*knowledgeCacheNoopWorker) Timeout(_ *river.Job[consumerjobs.KnowledgeCacheUpdaterJobArgs]) time.Duration {
	return 30 * time.Second
}
