package scheduler

import (
	"context"
	"errors"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/logger"

	"github.com/riverqueue/river"
)

// SyncServiceForAccountSync is the narrow interface
// SyncProviderAccountWorker needs. service.SyncService satisfies it via
// RunAccountSync.
type SyncServiceForAccountSync interface {
	RunAccountSync(ctx context.Context, source string, accountID *string) error
}

// SyncProviderAccountWorker is the river.Worker for per-account sync
// jobs. It is a thin dispatcher to SyncService.RunAccountSync so that
// the business logic stays in the service layer.
type SyncProviderAccountWorker struct {
	river.WorkerDefaults[SyncProviderAccountArgs]
	svc SyncServiceForAccountSync
}

// NewSyncProviderAccountWorker constructs a SyncProviderAccountWorker
// over the given service.
func NewSyncProviderAccountWorker(svc SyncServiceForAccountSync) *SyncProviderAccountWorker {
	return &SyncProviderAccountWorker{svc: svc}
}

// Work implements river.Worker for SyncProviderAccountArgs. Errors from
// RunAccountSync are wrapped and returned so river retries per the
// configured retry policy. db.ErrNotFound is treated as a terminal
// non-error (the sync_state was deleted; retrying won't help).
func (w *SyncProviderAccountWorker) Work(ctx context.Context, job *river.Job[SyncProviderAccountArgs]) error {
	source := job.Args.Source
	accountID := job.Args.AccountID

	if err := w.svc.RunAccountSync(ctx, source, accountID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			logger.Warn().
				Str("source", source).
				Msg("sync_provider_account: state not found; treating as terminal")
			return nil
		}
		return err
	}
	return nil
}
