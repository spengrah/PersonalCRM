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

// baseWorkerKinds is the 17 unconditionally-registered workers (shapes 1 & 4).
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
})

// syncWorkerKinds adds the two external-sync-gated workers (shapes 3 & 6).
var syncWorkerKinds = sortedCopy(append([]string{
	"scheduler_tick",
	"sync_provider_account",
}, baseWorkerKinds...))

// basePeriodicKinds is the 4 unconditionally-registered periodic jobs.
var basePeriodicKinds = sortedCopy([]string{
	"messaging_aggregate_sweeper",
	"pairing_token_janitor",
	"sync_staleness_watchdog",
	"assertion_rollover",
})

// syncPeriodicKinds adds the external-sync-gated scheduler tick.
var syncPeriodicKinds = sortedCopy(append([]string{"scheduler_tick"}, basePeriodicKinds...))

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

			reg, syncStk := buildWireChainForGolden(t, cfg)

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

// buildWireChainForGolden drives the wire chain in run()'s exact order, minus
// buildTelegram (telegram-skip rule) and minus riverClient.Start / the HTTP
// server. It returns the registrar (recording worker/periodic kinds) and the
// external-sync stack (for provider enumeration).
func buildWireChainForGolden(t *testing.T, cfg *config.Config) (*riverRegistrar, syncStack) {
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

	// Telegram is intentionally SKIPPED (Start must not run); a nil
	// telegramManager is exactly a telegram-disabled boot for aggregation.
	agg := buildAggregationEngines(database, core, contactService, graph, ingest, messaging, consumers, eventBus, riverClient, nil, syncStk.GChatProvider, syncStk.GChatSyncStates)
	registerMessagingWorkers(reg, ingest, messaging, agg, riverClient)

	machost := buildMacHost(reg, database, core, ingest)
	buildStaleness(reg, cfg, database, machost)
	registerAssertionRollover(reg, graph)
	registerSyncScheduler(reg, cfg, syncStk, riverClient)

	return reg, syncStk
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
