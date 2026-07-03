package main

import (
	"context"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/jobsample"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
)

// jobSampleTrimInterval is how often the job_sample_trim periodic job runs. The
// retention WINDOW is config (RIVER_JOB_SAMPLE_RETENTION_DAYS); the sweep
// interval is a fixed daily cadence.
const jobSampleTrimInterval = 24 * time.Hour

// jobSampleRecorderWaitTimeout bounds how long shutdown waits for the recorder
// goroutine to drain after riverClient.Stop closes the subscription channel. It
// guards against a Stop that times out without closing the channel wedging
// shutdown.
const jobSampleRecorderWaitTimeout = 5 * time.Second

// registerJobSampleWorkers registers the job_sample_trim periodic job (and its
// worker). Called from run() AND from buildWireChainForGolden (golden-list
// pinned).
func registerJobSampleWorkers(reg *riverRegistrar, repo *repository.JobSampleRepository, cfg *config.Config) {
	addWorker(reg, jobsample.NewTrimWorker(repo, cfg.River.JobSampleRetentionDays))
	reg.addPeriodic(jobsample.TrimArgs{}.Kind(), river.NewPeriodicJob(
		river.PeriodicInterval(jobSampleTrimInterval),
		func() (river.JobArgs, *river.InsertOpts) { return jobsample.TrimArgs{}, nil },
		&river.PeriodicJobOpts{RunOnStart: true},
	))
}

// startJobSampleRecorder subscribes to completed/failed events and starts the
// consumer goroutine. Call BEFORE riverClient.Start so no early events are
// missed. Returns a wait func to join the goroutine on shutdown (bounded so a
// Stop timeout can't wedge shutdown).
//
// It subscribes to all four per-execution event kinds (completed, failed,
// snoozed, cancelled) — each is a worker execution that occupied a slot.
//
// The subscription's cancel func is deliberately ignored: River closes every
// active subscription channel in its Stop shutdown defer, so the `for range`
// consumer self-terminates on Stop with no manual unsubscribe.
func startJobSampleRecorder(ctx context.Context, riverClient *river.Client[pgx.Tx], repo *repository.JobSampleRepository) func() {
	ch, _ := riverClient.SubscribeConfig(&river.SubscribeConfig{
		ChanSize: 10_000,
		// All four per-execution kinds: each represents a worker execution that
		// occupied a slot. Snoozed + cancelled are NOT rare for todoist_task_op
		// (mode-off / pending-update waits re-snooze the same job), so omitting
		// them would undercount slot occupancy for exactly the external-API
		// consumer this instrumentation is about.
		Kinds: []river.EventKind{
			river.EventKindJobCompleted,
			river.EventKindJobFailed,
			river.EventKindJobSnoozed,
			river.EventKindJobCancelled,
		},
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		jobsample.NewRecorder(repo).Run(ctx, ch)
	}()

	return func() {
		select {
		case <-done:
		case <-time.After(jobSampleRecorderWaitTimeout):
			logger.Warn().Msg("job_exec_sample recorder did not drain within timeout on shutdown")
		}
	}
}
