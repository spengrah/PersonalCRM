package scheduler

import (
	"context"
	"time"

	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/logger"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
)

// UnprocessedContactLister returns the distinct contact IDs with at
// least one unprocessed staging row across the source's staging
// table. Used by MessagingAggregateSweeperWorker; concrete in
// production is *repository.MessagesMessageRepository.
type UnprocessedContactLister interface {
	ListUnprocessedContactIDs(ctx context.Context) ([]uuid.UUID, error)
}

// RiverInserter is the narrow surface the sweeper uses to enqueue
// MessagingAggregateForContactArgs jobs. Concrete is
// *river.Client[pgx.Tx].
type RiverInserter interface {
	Insert(ctx context.Context, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// MessagingAggregateSweeperWorker is the periodic safety net for
// stranded-row recovery. The worker:
//  1. Lists all contacts with at least one eligible (unprocessed AND
//     unclaimed-or-stale) staging row for a given source.
//  2. Enqueues a MessagingAggregateForContactArgs per contact (with
//     UniqueOpts so in-flight jobs dedup).
//
// Bounds worst-case stranded-row latency at the sweep interval (5
// min in production). Defense-in-depth against the narrow race where
// a row lands while another aggregator worker has just exited its
// re-list loop AND UniqueOpts suppressed a follow-up enqueue.
type MessagingAggregateSweeperWorker struct {
	river.WorkerDefaults[consumerjobs.MessagingAggregateSweeperArgs]
	listers     map[string]UnprocessedContactLister
	riverClient RiverInserter
}

// NewMessagingAggregateSweeperWorker constructs the worker.
// listers maps source name → UnprocessedContactLister. Pass nil
// riverClient to disable enqueue (useful only for tests).
func NewMessagingAggregateSweeperWorker(
	listers map[string]UnprocessedContactLister,
	riverClient RiverInserter,
) *MessagingAggregateSweeperWorker {
	return &MessagingAggregateSweeperWorker{
		listers:     listers,
		riverClient: riverClient,
	}
}

// Work implements river.Worker.
func (w *MessagingAggregateSweeperWorker) Work(
	ctx context.Context,
	_ *river.Job[consumerjobs.MessagingAggregateSweeperArgs],
) error {
	totalInserted := 0
	totalDuplicateSkipped := 0
	for source, lister := range w.listers {
		contactIDs, err := lister.ListUnprocessedContactIDs(ctx)
		if err != nil {
			// Don't fail the whole sweep on a single source's error
			// — other sources may still be drainable. Log and
			// continue. River retries the periodic job on the next
			// tick.
			logger.Warn().
				Err(err).
				Str("source", source).
				Msg("messaging_aggregate_sweeper: list contacts failed")
			continue
		}
		for _, cid := range contactIDs {
			if w.riverClient == nil {
				continue
			}
			args := consumerjobs.MessagingAggregateForContactArgs{
				ContactID: cid,
				Source:    source,
			}
			res, err := w.riverClient.Insert(ctx, args, &river.InsertOpts{
				UniqueOpts: consumerjobs.MessagingAggregateUniqueOpts(),
			})
			if err != nil {
				logger.Warn().
					Err(err).
					Str("source", source).
					Str("contact_id", cid.String()).
					Msg("messaging_aggregate_sweeper: enqueue failed")
				continue
			}
			if res != nil && res.UniqueSkippedAsDuplicate {
				totalDuplicateSkipped++
				continue
			}
			totalInserted++
		}
	}
	if totalInserted > 0 || totalDuplicateSkipped > 0 {
		logger.Info().
			Int("inserted", totalInserted).
			Int("duplicate_skipped", totalDuplicateSkipped).
			Msg("messaging_aggregate_sweeper: tick processed aggregator jobs")
	}
	return nil
}

// Timeout caps each invocation. The list query is bounded by
// unprocessed-contact count (typically small); 30s is generous.
func (*MessagingAggregateSweeperWorker) Timeout(_ *river.Job[consumerjobs.MessagingAggregateSweeperArgs]) time.Duration {
	return 30 * time.Second
}
