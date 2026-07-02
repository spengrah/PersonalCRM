package main

import (
	"context"
	"time"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/scheduler"

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
