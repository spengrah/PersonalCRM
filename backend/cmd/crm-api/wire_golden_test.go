//go:build integration_testdb

// Golden-list regression pin for the composition-root wire split (contract 7,
// PR4). River's public API does not enumerate registered workers or periodic
// jobs, so the wire functions register through riverRegistrar, which records
// the kind of every worker + periodic job. This test drives the wire chain in
// run()'s exact order (MINUS buildTelegram — see the telegram-skip rule) per
// config shape and asserts the recorded worker/periodic kinds + the
// provider-registry contents against golden lists.
//
// Telegram-skip rule: buildTelegram calls telegramManager.Start(ctx), which
// must never run in a test. Skipping it is behavior-faithful for this pin —
// telegram registers NO workers, NO periodic jobs, and NO providers, and
// buildAggregationEngines takes telegramManager as a nil-safe param (nil ==
// telegram-disabled boot). So shape 4 (telegram) asserts lists IDENTICAL to
// shape 1 (base), which is exactly what the source inventory proves.
package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/testdb"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	// Hoisted out of buildWireChainForGolden (called from parallel subtests) so
	// the global logger is initialized once, avoiding a data race on logger.Get.
	logger.Init(config.TestConfig().Logger)
	os.Exit(testdb.SetupPackage(m, testdb.WithMigrationsPath(migrationsPathForTest())))
}

// migrationsPathForTest resolves the migrations dir relative to this test file
// (cmd/crm-api → ../../migrations), honoring an absolute MIGRATIONS_PATH override.
func migrationsPathForTest() string {
	if path := os.Getenv("MIGRATIONS_PATH"); path != "" && filepath.IsAbs(path) {
		return path
	}
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..", "migrations")
}

// --- golden lists (sorted) -------------------------------------------------

// baseWorkerKinds is the 18 unconditionally-registered workers (shapes 1 & 4).
var baseWorkerKinds = sortedCopy([]string{
	"noop",
	"interaction_recorder",
	"calendar_decline_handler",
	"email_interaction_consumer",
	"cadence_updater",
	"knowledge_cache_updater",
	"followup_manager",
	"todoist_task_op",
	"todoist_followup_create",
	"todoist_followup_close",
	"todoist_followup_refresh",
	"rematch_dispatcher",
	"messaging_aggregate_for_contact",
	"messaging_aggregate_sweeper",
	"pairing_token_janitor",
	"sync_staleness_watchdog",
	"assertion_rollover",
	"job_sample_trim",
})

// syncWorkerKinds adds the two external-sync-gated workers (shapes 3 & 6).
var syncWorkerKinds = sortedCopy(append([]string{
	"scheduler_tick",
	"sync_provider_account",
}, baseWorkerKinds...))

// basePeriodicKinds is the 5 unconditionally-registered periodic jobs.
var basePeriodicKinds = sortedCopy([]string{
	"messaging_aggregate_sweeper",
	"pairing_token_janitor",
	"sync_staleness_watchdog",
	"assertion_rollover",
	"job_sample_trim",
})

// syncPeriodicKinds adds the external-sync-gated scheduler tick.
var syncPeriodicKinds = sortedCopy(append([]string{"scheduler_tick"}, basePeriodicKinds...))

// whatsappWorkerKinds / whatsappPeriodicKinds add the WhatsApp-gated history
// drain (shape 7). WhatsApp requires external sync — config.Validate refuses
// the pair as inconsistent — so the shape builds on the sync lists, not the
// base ones.
var whatsappWorkerKinds = sortedCopy(append([]string{"whatsapp_history_drain"}, syncWorkerKinds...))

var whatsappPeriodicKinds = sortedCopy(append([]string{"whatsapp_history_drain"}, syncPeriodicKinds...))

// shape3Providers is the fully-configured external-sync provider set.
var shape3Providers = sortedCopy([]string{
	"gcontacts", "gcal", "email", "gchat", "todoist",
	"messages", "icloud_contacts", "phone_calls",
})

// shape6Providers drops gmail ("email") — interaction-mode off ⇒ pubBus nil ⇒
// the Gmail provider is NOT registered (INV-3 registry pin).
var shape6Providers = sortedCopy([]string{
	"gcontacts", "gcal", "gchat", "todoist",
	"messages", "icloud_contacts", "phone_calls",
})

func TestWireGoldenLists(t *testing.T) {
	t.Parallel()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	tests := []struct {
		name          string
		mutate        func(*config.Config)
		wantWorkers   []string
		wantPeriodic  []string
		wantProviders []string // nil ⇒ external sync disabled, no provider assertion
	}{
		{
			name:         "shape1_base",
			mutate:       func(c *config.Config) {},
			wantWorkers:  baseWorkerKinds,
			wantPeriodic: basePeriodicKinds,
		},
		{
			name: "shape3_external_sync",
			mutate: func(c *config.Config) {
				c.Features.EnableExternalSync = true
			},
			wantWorkers:   syncWorkerKinds,
			wantPeriodic:  syncPeriodicKinds,
			wantProviders: shape3Providers,
		},
		{
			name: "shape4_telegram",
			mutate: func(c *config.Config) {
				// Telegram is skipped in the wire chain (Start must not run),
				// so its worker/periodic lists equal shape 1's.
				c.Features.EnableTelegramSync = true
			},
			wantWorkers:  baseWorkerKinds,
			wantPeriodic: basePeriodicKinds,
		},
		{
			name: "shape6_external_sync_interaction_off",
			mutate: func(c *config.Config) {
				c.Features.EnableExternalSync = true
				c.EventBus.InteractionMode = config.EventBusInteractionModeOff
			},
			wantWorkers:   syncWorkerKinds,
			wantPeriodic:  syncPeriodicKinds,
			wantProviders: shape6Providers,
		},
		{
			// The WhatsApp shape sets BOTH flags because config.Validate
			// refuses WhatsApp-on / external-sync-off as inconsistent, and a
			// golden shape that could not boot pins nothing real.
			name: "shape7_whatsapp_sync",
			mutate: func(c *config.Config) {
				c.Features.EnableExternalSync = true
				c.Features.EnableWhatsAppSync = true
			},
			wantWorkers:   whatsappWorkerKinds,
			wantPeriodic:  whatsappPeriodicKinds,
			wantProviders: shape3Providers,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cloneURL, drop := testdb.NewEphemeralClone(t)
			t.Cleanup(drop)

			cfg := config.TestConfig()
			cfg.Database.URL = cloneURL
			cfg.Database.MigrationsPath = migrationsPathForTest()
			tc.mutate(cfg)

			chain := buildWireChainForGolden(t, cfg)
			reg, syncStk := chain.reg, chain.sync

			require.Equal(t, tc.wantWorkers, sortedCopy(reg.workerKinds), "worker kinds")
			require.Equal(t, tc.wantPeriodic, sortedCopy(reg.periodicKinds), "periodic kinds")

			if tc.wantProviders != nil {
				require.NotNil(t, syncStk.SyncService, "external sync should build a sync service")
				var names []string
				for _, c := range syncStk.SyncService.GetAvailableProviders() {
					names = append(names, c.Name)
				}
				require.Equal(t, tc.wantProviders, sortedCopy(names), "provider registry contents")
			}
		})
	}
}

// wireChain is everything buildWireChainForGolden produces. The golden lists
// read reg + sync; the WhatsApp composition-root parity test
// (whatsapp_wiring_test.go) invokes the source-keyed registries in messaging,
// agg and wiring, which is why they are returned rather than discarded.
type wireChain struct {
	reg       *riverRegistrar
	sync      syncStack
	database  *db.Database
	core      coreRepos
	graph     graphCore
	messaging messagingFoundation
	agg       aggregationStack
	wiring    messagingWorkerWiring
	river     *river.Client[pgx.Tx]
	bus       *events.Bus
	eventRepo *repository.EventRepository
}

// buildWireChainForGolden drives the wire chain in run()'s exact order, minus
// buildTelegram (telegram-skip rule) and minus riverClient.Start / the HTTP
// server.
func buildWireChainForGolden(t *testing.T, cfg *config.Config) wireChain {
	t.Helper()
	ctx := context.Background()

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	core := buildCoreRepos(database.Queries)

	riverWorkers := river.NewWorkers()
	reg := newRiverRegistrar(riverWorkers)
	addWorker(reg, &noopWorker{})

	riverClient, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		JobTimeout: cfg.River.JobTimeout,
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers:                     riverWorkers,
		ErrorHandler:                events.NewRiverErrorHandler(logger.Get()),
		Logger:                      logger.NewSlogLogger(logger.Get()),
		DiscardedJobRetentionPeriod: riverDiscardedJobRetention,
	})
	require.NoError(t, err)
	reg.periodic = riverClient.PeriodicJobs()

	eventRepo := repository.NewEventRepository(database.Queries)
	eventBus := events.NewBus(database.Pool, riverClient, eventRepo)

	ingest := buildIngestRepos(database.Queries)
	graph := buildGraphCore(database, eventBus)
	messaging := buildMessagingFoundation(database.Queries, ingest.MessagesMessage, ingest.CalendarEvent)
	// Part of run()'s order (main.go): AttachUnmatchedByPeer owns its own
	// transaction and requires a pool, so the WhatsApp rematch handlers this
	// chain registers would fail at runtime without it. Registers no worker,
	// periodic or provider, so the golden lists are unaffected.
	messaging.CommsMessageRepo.SetPool(database.Pool)
	consumers := buildDomainConsumers(cfg, database, core, graph, eventBus, riverClient)
	contactService := buildContactService(cfg, database, core, graph, consumers, eventBus, riverClient)
	interactionRecorder := buildInteractionRecorder(contactService, messaging, ingest, consumers, eventBus)
	ingestStk := buildIngestStack(database, core, contactService, ingest, messaging, consumers, eventBus, riverClient)
	registerCoreConsumerWorkers(reg, database, core, contactService, interactionRecorder, messaging, consumers, eventBus)
	pubBus, _ := resolveInteractionMode(cfg, database, interactionRecorder, eventBus)
	registerModeWorkers(reg, cfg, database, core, consumers, eventBus, riverClient)
	domain := buildDomainServices(database, core, graph, ingest, consumers, ingestStk, eventBus)
	registerRematchDispatcher(reg, graph, database, eventBus)

	var syncStk syncStack
	if cfg.Features.EnableExternalSync {
		syncStk = buildExternalSync(ctx, cfg, database, core, contactService, graph, ingest, messaging, consumers, domain, eventBus, riverClient, pubBus)
	}

	// WhatsApp: only the drain REGISTRATION is driven, at the position
	// wireWhatsApp calls it. The rest of wireWhatsApp opens the device store
	// and calls Manager.Start, which must never run in this chain — the same
	// reason buildTelegram is skipped. The registration deliberately takes no
	// manager/ingestor/gate, so a config shape's kind list does not depend on
	// device-store availability, and the drainer normalizes the nil source.
	registerWhatsAppHistoryDrain(reg, cfg, database, nil, nil, nil)

	// Telegram is intentionally SKIPPED (Start must not run); a nil
	// telegramManager is exactly a telegram-disabled boot for aggregation.
	whatsappEngine := buildWhatsAppAggregationEngine(cfg, database, core.Interaction, messaging.CommsMessageRepo, contactService, eventBus, riverClient)
	agg := buildAggregationEngines(cfg, database, core, contactService, graph, ingest, messaging, consumers, eventBus, riverClient, nil, syncStk.GChatProvider, syncStk.GChatSyncStates, whatsappEngine)
	wiring := registerMessagingWorkers(reg, ingest, messaging, agg, riverClient)

	machost := buildMacHost(reg, database, core, ingest)
	buildStaleness(reg, cfg, database, machost)
	registerAssertionRollover(reg, graph)
	registerSyncScheduler(reg, cfg, syncStk, riverClient)
	// The recorder is intentionally NOT started here (it starts a goroutine and
	// is neither a worker nor a periodic job); only the trim worker/periodic is
	// pinned by the golden lists.
	registerJobSampleWorkers(reg, repository.NewJobSampleRepository(database.Queries), cfg)

	return wireChain{
		reg:       reg,
		sync:      syncStk,
		database:  database,
		core:      core,
		graph:     graph,
		messaging: messaging,
		agg:       agg,
		wiring:    wiring,
		river:     riverClient,
		bus:       eventBus,
		eventRepo: eventRepo,
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
