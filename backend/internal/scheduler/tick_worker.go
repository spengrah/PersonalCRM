package scheduler

import (
	"context"

	"personal-crm/backend/internal/logger"

	"github.com/riverqueue/river"
)

// DueAccount identifies a (source, account_id) pair whose sync is due.
// Mirrors repository.DueAccount; duplicated here so the scheduler package
// does not have to import repository (which would create a cycle since
// repository imports scheduler for SyncProviderAccountArgs).
// service.SyncService returns []repository.DueAccount; callers adapt by
// constructing a thin translator (see main.go wiring).
type DueAccount struct {
	Source    string
	AccountID *string
}

// SyncServiceForTick is the narrow interface SchedulerTickWorker needs.
// Keeping it narrow makes the worker trivial to unit-test without
// standing up a real service + repository stack.
type SyncServiceForTick interface {
	ListDueAccounts(ctx context.Context) ([]DueAccount, error)
	EnqueueAccountSyncIfNotInFlight(ctx context.Context, source string, accountID *string) (bool, error)
}

// SchedulerTickWorker is the river.Worker for the 5-minute periodic
// SchedulerTickJob. On each tick it enumerates due sync states and calls
// EnqueueAccountSyncIfNotInFlight for each one. Per-account errors are
// logged but do not fail the tick — a partial failure should not block
// the other accounts from enqueueing.
type SchedulerTickWorker struct {
	river.WorkerDefaults[SchedulerTickArgs]
	svc SyncServiceForTick
}

// NewSchedulerTickWorker constructs a SchedulerTickWorker over the given
// service. In production this is *service.SyncService, which satisfies
// SyncServiceForTick.
func NewSchedulerTickWorker(svc SyncServiceForTick) *SchedulerTickWorker {
	return &SchedulerTickWorker{svc: svc}
}

// Work implements river.Worker for SchedulerTickArgs.
func (w *SchedulerTickWorker) Work(ctx context.Context, _ *river.Job[SchedulerTickArgs]) error {
	accounts, err := w.svc.ListDueAccounts(ctx)
	if err != nil {
		// Transient error listing due states; return wrapped so river
		// retries the tick per default retry policy.
		return err
	}

	if len(accounts) == 0 {
		logger.Debug().Msg("scheduler tick: no due sync accounts")
		return nil
	}

	logger.Info().
		Int("count", len(accounts)).
		Msg("scheduler tick: enumerating due sync accounts")

	for _, acct := range accounts {
		enqueued, err := w.svc.EnqueueAccountSyncIfNotInFlight(ctx, acct.Source, acct.AccountID)
		if err != nil {
			// Per-account error is not fatal to the tick; the next tick
			// will retry. Log with context for operator visibility.
			logger.Error().
				Err(err).
				Str("source", acct.Source).
				Msg("scheduler tick: failed to enqueue sync job; continuing")
			continue
		}
		if !enqueued {
			logger.Debug().
				Str("source", acct.Source).
				Msg("scheduler tick: sync already in-flight; skipped")
			continue
		}
		logger.Debug().
			Str("source", acct.Source).
			Msg("scheduler tick: enqueued sync job")
	}

	return nil
}
