package main

import (
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/scheduler"

	"github.com/riverqueue/river"
)

// syncLogTrimInterval is how often the sync_log_trim periodic job runs. The
// retention WINDOW is config (SYNC_LOG_RETENTION_DAYS); the sweep interval is a
// fixed daily cadence, matching job_sample_trim.
const syncLogTrimInterval = 24 * time.Hour

// registerSyncLogTrim registers the sync_log_trim periodic job (and its
// worker). Unconditional: external_sync_log rows outlive the feature flag that
// wrote them, so the trim runs whether or not external sync is enabled. Called
// from run() AND from buildWireChainForGolden (golden-list pinned).
func registerSyncLogTrim(reg *riverRegistrar, repo *repository.SyncRepository, cfg *config.Config) {
	addWorker(reg, scheduler.NewSyncLogTrimWorker(repo, cfg.Sync.LogRetentionDays))
	reg.addPeriodic(scheduler.SyncLogTrimArgs{}.Kind(), river.NewPeriodicJob(
		river.PeriodicInterval(syncLogTrimInterval),
		func() (river.JobArgs, *river.InsertOpts) { return scheduler.SyncLogTrimArgs{}, nil },
		&river.PeriodicJobOpts{RunOnStart: true},
	))
}
