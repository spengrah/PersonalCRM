package main

import (
	"context"
	"time"

	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/scheduler"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// registerMessagingWorkers registers the chat-aware messaging aggregate
// worker + the 5-min sweeper worker and its periodic job. The chat-lister
// and sweeper-lister registries map source → repository lister; future
// messaging sources extend the maps without touching the workers.
func registerMessagingWorkers(
	reg *riverRegistrar,
	ingest ingestRepos,
	messaging messagingFoundation,
	agg aggregationStack,
	riverClient *river.Client[pgx.Tx],
) {
	messagesMessageRepo := ingest.MessagesMessage
	commsMessageRepo := messaging.CommsMessageRepo
	messagesEngine := agg.MessagesEngine
	gchatEngine := agg.GChatEngine

	// Register the messaging aggregate workers. The chat-lister
	// registry maps source → repository's ListUnprocessedChatsByContact;
	// future messaging sources (whatsapp etc) extend the map without
	// touching the worker.
	chatListerRegistry := scheduler.NewPerSourceChatListerRegistry(
		map[string]func(ctx context.Context, contactID uuid.UUID) ([]string, error){
			repository.InteractionSourceMessages: messagesMessageRepo.ListUnprocessedChatsByContact,
			// Source-bound closure: the comms repo method is multi-source
			// (ListUnprocessedChatsByContactForSource), so bind 'gchat'.
			repository.InteractionSourceGChat: func(ctx context.Context, contactID uuid.UUID) ([]string, error) {
				return commsMessageRepo.ListUnprocessedChatsByContactForSource(ctx, repository.InteractionSourceGChat, contactID)
			},
		},
	)
	addWorker(reg, scheduler.NewMessagingAggregateForContactWorker(
		map[string]scheduler.ChatAwareAggregator{
			repository.InteractionSourceMessages: messagesEngine,
			repository.InteractionSourceGChat:    gchatEngine,
		},
		chatListerRegistry,
	))

	// Periodic 5-min sweeper — drains never-claimed stranded rows that
	// the in-line worker re-list loop AND the post-Stage-3 reenqueue
	// both missed. Run once on startup so restart-recovery does not wait
	// a full interval before the safety net engages.
	sweeperListers := map[string]scheduler.UnprocessedContactLister{
		repository.InteractionSourceMessages: messagesMessageRepo,
		// Source-bound adapter: comms_message is multi-source, so wrap the
		// repo with a 'gchat'-pinned lister.
		repository.InteractionSourceGChat: newCommsSourceContactLister(commsMessageRepo, repository.InteractionSourceGChat),
	}
	addWorker(reg, scheduler.NewMessagingAggregateSweeperWorker(sweeperListers, riverClient))
	reg.addPeriodic(consumerjobs.MessagingAggregateSweeperArgs{}.Kind(), river.NewPeriodicJob(
		river.PeriodicInterval(5*time.Minute),
		func() (river.JobArgs, *river.InsertOpts) {
			return consumerjobs.MessagingAggregateSweeperArgs{}, nil
		},
		&river.PeriodicJobOpts{RunOnStart: true},
	))
}

// stalenessStack holds the staleness service (consumed by the health
// checker) + its handler (consumed by route registration).
type stalenessStack struct {
	Service *service.StalenessService
	Handler *handlers.StalenessHandler
}

// buildStaleness wires the sync-staleness watchdog. Registered
// unconditionally (like the janitor): heartbeat/push breaches must be
// detected even with external sync off, and the watchdog reads existing
// freshness state rather than driving any provider sync. The endpoint
// reader uses the same service instance.
func buildStaleness(reg *riverRegistrar, cfg *config.Config, database *db.Database, machost macHostStack) stalenessStack {
	macSyncRepo := machost.SyncRepo
	macHostRepo := machost.Repo

	stalenessBreachRepo := repository.NewStalenessRepository(database.Queries)
	stalenessService := service.NewStalenessService(
		cfg.Staleness,
		cfg.Features.EnableExternalSync,
		macSyncRepo,
		macHostRepo,
		stalenessBreachRepo,
	)
	stalenessHandler := handlers.NewStalenessHandler(stalenessService)
	addWorker(reg, scheduler.NewStalenessWatchdogWorker(stalenessService))
	reg.addPeriodic(scheduler.StalenessWatchdogArgs{}.Kind(), river.NewPeriodicJob(
		river.PeriodicInterval(5*time.Minute),
		func() (river.JobArgs, *river.InsertOpts) {
			return scheduler.StalenessWatchdogArgs{}, nil
		},
		&river.PeriodicJobOpts{RunOnStart: true},
	))

	return stalenessStack{
		Service: stalenessService,
		Handler: stalenessHandler,
	}
}

// registerAssertionRollover registers the daily assertion valid-time
// rollover worker + its periodic job. Stateless; RunOnStart catches up any
// overdue rollovers on boot.
func registerAssertionRollover(reg *riverRegistrar, graph graphCore) {
	assertService := graph.AssertService

	// Assertion valid-time rollover — a daily catch-up sweep that terminalizes the
	// bounded-with-pending-successor assertions whose valid_to has passed (no event
	// fires at that future date otherwise). Stateless; RunOnStart catches up any
	// overdue rollovers on boot.
	addWorker(reg, scheduler.NewAssertionRolloverWorker(assertService))
	reg.addPeriodic(scheduler.AssertionRolloverArgs{}.Kind(), river.NewPeriodicJob(
		river.PeriodicInterval(24*time.Hour),
		func() (river.JobArgs, *river.InsertOpts) {
			return scheduler.AssertionRolloverArgs{}, nil
		},
		&river.PeriodicJobOpts{RunOnStart: true},
	))
}

// registerSyncScheduler registers the scheduler-tick + sync-provider-account
// workers (+ periodic tick) and wires the River enqueuer onto the sync
// service — all gated on external sync being enabled with a real syncService.
// The SetRiverEnqueuer wiring happens BEFORE riverClient.Start so any tick
// fire or TriggerSync that races with bring-up goes through river instead of
// falling back to inline sync.
func registerSyncScheduler(reg *riverRegistrar, cfg *config.Config, syncStk syncStack, riverClient *river.Client[pgx.Tx]) {
	syncService := syncStk.SyncService

	// The scheduler-tick + sync-provider-account workers are only registered
	// when external sync is enabled and we have a real syncService —
	// otherwise there is nothing for them to do.
	if cfg.Features.EnableExternalSync && syncService != nil {
		addWorker(reg, scheduler.NewSchedulerTickWorker(syncService))
		addWorker(reg, scheduler.NewSyncProviderAccountWorker(syncService))
		reg.addPeriodic(scheduler.SchedulerTickArgs{}.Kind(), river.NewPeriodicJob(
			river.PeriodicInterval(5*time.Minute),
			func() (river.JobArgs, *river.InsertOpts) {
				return scheduler.SchedulerTickArgs{}, nil
			},
			&river.PeriodicJobOpts{RunOnStart: true},
		))
	}

	// Wire the enqueuer onto the service BEFORE starting the client so
	// any tick fire or TriggerSync that races with bring-up goes through
	// river instead of falling back to inline sync.
	if syncService != nil && cfg.Features.EnableExternalSync {
		syncService.SetRiverEnqueuer(riverClient)
	}
}
