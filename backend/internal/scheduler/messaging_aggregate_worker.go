package scheduler

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/logger"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
)

// workerLoopMaxIterations caps how many re-list passes the worker
// performs in a single run. The loop drains rows that landed between
// the engine's list-query and the worker's re-list (see spec §3 race-
// mechanics for the unhandled-row window discussion). Hitting the cap
// means staging is being filled faster than we drain; the next
// MessagingAggregateForContactArgs enqueue OR the periodic sweeper
// picks up the residual rows.
const workerLoopMaxIterations = 50

// chatAwareAggregator is the subset of the messaging aggregation
// engine surface the worker actually invokes. Concrete is
// *aggregation.Engine; the interface keeps the worker testable with
// a stub.
type chatAwareAggregator interface {
	AggregateForContact(ctx context.Context, contactID uuid.UUID, chatID string) error
}

// chatLister enumerates the distinct unprocessed chats for a contact
// scoped to a single source. The registry pattern mirrors
// repository.StagingProcessorRegistry — one entry per source so the
// worker can route by job.Args.Source.
type chatLister interface {
	ListUnprocessedChats(ctx context.Context, source string, contactID uuid.UUID) ([]string, error)
}

// PerSourceChatListerRegistry maps source name → chat-lister
// implementation. Construct in main.go with one entry per active
// source.
type PerSourceChatListerRegistry struct {
	entries map[string]func(ctx context.Context, contactID uuid.UUID) ([]string, error)
}

// NewPerSourceChatListerRegistry builds the registry over a
// source → function map.
func NewPerSourceChatListerRegistry(entries map[string]func(ctx context.Context, contactID uuid.UUID) ([]string, error)) *PerSourceChatListerRegistry {
	return &PerSourceChatListerRegistry{entries: entries}
}

// ListUnprocessedChats dispatches to the registered source entry.
// Unknown sources return (nil, nil) — the worker logs a warning and
// no-ops, matching the StagingProcessorRegistry pattern.
func (r *PerSourceChatListerRegistry) ListUnprocessedChats(ctx context.Context, source string, contactID uuid.UUID) ([]string, error) {
	fn, ok := r.entries[source]
	if !ok {
		return nil, nil
	}
	return fn(ctx, contactID)
}

// MessagingAggregateForContactWorker drives the chat-aware aggregation
// engine over all unprocessed chats for a (contactID, source) pair.
// Enqueued by the ingest service after a batch of raw_message.* events
// lands; UniqueOpts{ByArgs: true} dedups concurrent enqueues for the
// same pair into one in-flight job.
//
// The chat-aware path is what preserves the engine's extend/bridge/
// coalesce contract (spec §3 "Stage 2 — Aggregator"). The create-only
// batch path (Engine.AggregateForContactBatch) is intentionally NOT
// used here — it skips explicit-reply bridging and same-direction
// coalescing.
type MessagingAggregateForContactWorker struct {
	river.WorkerDefaults[consumerjobs.MessagingAggregateForContactArgs]
	engines    map[string]chatAwareAggregator
	chatLister chatLister
}

// NewMessagingAggregateForContactWorker constructs the worker.
// engines is keyed by source name; chatLister routes
// ListUnprocessedChats by source.
func NewMessagingAggregateForContactWorker(
	engines map[string]chatAwareAggregator,
	chatLister chatLister,
) *MessagingAggregateForContactWorker {
	return &MessagingAggregateForContactWorker{
		engines:    engines,
		chatLister: chatLister,
	}
}

// Work implements river.Worker for MessagingAggregateForContactArgs.
func (w *MessagingAggregateForContactWorker) Work(
	ctx context.Context,
	job *river.Job[consumerjobs.MessagingAggregateForContactArgs],
) error {
	engine, ok := w.engines[job.Args.Source]
	if !ok {
		// Unknown source: log and return nil (no work to do).
		// Mirrors the StagingProcessorRegistry's "unknown source =
		// noop" pattern. A misconfigured source string at enqueue
		// time shouldn't kill the worker — the operator sees the log
		// line and addresses.
		logger.Warn().
			Str("source", job.Args.Source).
			Str("contact_id", job.Args.ContactID.String()).
			Msg("messaging_aggregate: no engine registered for source; noop")
		return nil
	}
	if w.chatLister == nil {
		logger.Warn().
			Str("source", job.Args.Source).
			Msg("messaging_aggregate: no chat lister wired; noop")
		return nil
	}

	for iter := 0; iter < workerLoopMaxIterations; iter++ {
		chats, err := w.chatLister.ListUnprocessedChats(ctx, job.Args.Source, job.Args.ContactID)
		if err != nil {
			return fmt.Errorf("list unprocessed chats: %w", err)
		}
		if len(chats) == 0 {
			return nil // drained
		}
		for _, chatID := range chats {
			if err := engine.AggregateForContact(ctx, job.Args.ContactID, chatID); err != nil {
				return fmt.Errorf("aggregate contact=%s chat=%s: %w",
					job.Args.ContactID, chatID, err)
			}
		}
	}
	// Hit the loop bound. The next ingest's enqueue OR the periodic
	// sweeper will pick up remaining rows. The bound prevents a
	// runaway worker from monopolizing a River executor slot.
	logger.Warn().
		Str("contact_id", job.Args.ContactID.String()).
		Str("source", job.Args.Source).
		Int("max_iter", workerLoopMaxIterations).
		Msg("messaging_aggregate: hit loop bound; subsequent enqueues will continue draining")
	return nil
}

// Timeout caps each invocation. 60s matches existing worker timeouts;
// PR7's chat.db reader load testing may surface a need to bump this.
func (*MessagingAggregateForContactWorker) Timeout(_ *river.Job[consumerjobs.MessagingAggregateForContactArgs]) time.Duration {
	return 60 * time.Second
}
