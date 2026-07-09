// Package jobsample records one row per finished River job attempt (via a
// Client.Subscribe consumer) into job_exec_sample, and trims that table on a
// periodic schedule. The persisted samples let the queue-split decision be
// computed over a multi-week window even though completed rows prune from
// river_job after River's ~24h retention.
package jobsample

import (
	"context"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"

	"github.com/riverqueue/river"
)

// warnThrottle is the minimum gap between near-buffer-cap WARN logs. The warning
// is the only residual lossy-capture signal (River silently drops events on
// buffer overflow), so it must be visible but not spammy.
const warnThrottle = time.Minute

// nearFullFraction is the fill fraction of the subscription channel above which
// the recorder logs the rate-limited near-cap warning.
const nearFullFraction = 0.8

// SampleWriter is the narrow write interface the Recorder needs. In production
// this is *repository.JobSampleRepository.
type SampleWriter interface {
	InsertJobExecSample(ctx context.Context, s repository.JobExecSampleRow) error
}

// Recorder drains a River event subscription and writes one job_exec_sample row
// per finished job attempt.
type Recorder struct {
	writer SampleWriter
}

// NewRecorder constructs a Recorder over the given writer.
func NewRecorder(w SampleWriter) *Recorder {
	return &Recorder{writer: w}
}

// eventToSample maps a River event to insert-row fields. Pure + unit-testable
// (no DB, no clock): it does NOT set CreatedAt. Kind-agnostic — it works for all
// four subscribed per-execution kinds (completed, failed, snoozed, cancelled)
// off the JobRow + JobStats fields. Returns ok=false when the event should be
// skipped (nil Job/JobStats, or nil AttemptedAt — the latter is a job cancelled
// BEFORE it ever ran, which occupied no slot; completed/failed always have it).
//
// finalized_at = ev.Job.FinalizedAt if non-nil, else the attempt end is
// synthesized as *ev.Job.AttemptedAt + ev.JobStats.RunDuration. FinalizedAt is
// nil for a retryable failure AND for a snooze (state "scheduled"/"available")
// — both hit the synthesized branch; a completion, final discard, or cancel has
// it set. state = string(ev.Job.State) (e.g. "completed"/"retryable"/
// "discarded"/"scheduled"/"cancelled"). queue_wait_ms comes from
// ev.JobStats.QueueWaitDuration — NOT attempted_at - scheduled_at — because
// River mutates scheduled_at (to the retry/snooze time) before emitting the
// event, which would produce a negative wait.
func eventToSample(ev *river.Event) (repository.JobExecSampleRow, bool) {
	job := ev.Job
	// JobStats carries the precomputed durations we depend on; a nil Job or
	// JobStats (or a never-attempted job) is skipped rather than dereferenced —
	// best-effort capture must never panic the recorder goroutine.
	if job == nil || ev.JobStats == nil || job.AttemptedAt == nil {
		return repository.JobExecSampleRow{}, false
	}

	finalizedAt := job.AttemptedAt.Add(ev.JobStats.RunDuration)
	if job.FinalizedAt != nil {
		finalizedAt = *job.FinalizedAt
	}
	// AttemptedAt is stamped by River's fetch query (DB clock) while
	// job.FinalizedAt is stamped by the River client (Go clock). When the
	// client clock lags the DB clock by more than the job's run time,
	// FinalizedAt lands before AttemptedAt and the insert would violate
	// job_exec_sample_interval_chk, silently dropping the sample. Clamp so
	// best-effort capture survives bounded clock skew.
	if finalizedAt.Before(*job.AttemptedAt) {
		finalizedAt = *job.AttemptedAt
	}

	return repository.JobExecSampleRow{
		RiverJobID:  job.ID,
		Kind:        job.Kind,
		Queue:       job.Queue,
		AttemptedAt: *job.AttemptedAt,
		FinalizedAt: finalizedAt,
		Attempt:     job.Attempt,
		State:       string(job.State),
		QueueWaitMs: ev.JobStats.QueueWaitDuration.Milliseconds(),
	}, true
}

// Run drains ch until it is closed (River closes every subscription channel on
// client Stop), stamping CreatedAt = accelerated.GetCurrentTime() and writing
// one row per event. It logs a rate-limited WARN when the channel is near its
// buffer cap (the residual lossy-capture signal), and logs + continues on a
// write error (best-effort capture must never be fatal or backpressure work).
func (r *Recorder) Run(ctx context.Context, ch <-chan *river.Event) {
	capacity := cap(ch)
	var lastWarn time.Time

	for ev := range ch {
		if capacity > 0 {
			if now := accelerated.GetCurrentTime(); float64(len(ch)) > nearFullFraction*float64(capacity) &&
				now.Sub(lastWarn) >= warnThrottle {
				lastWarn = now
				logger.Warn().
					Int("buffered", len(ch)).
					Int("capacity", capacity).
					Msg("job_exec_sample subscription near buffer cap; events may be dropped")
			}
		}

		row, ok := eventToSample(ev)
		if !ok {
			continue
		}
		row.CreatedAt = accelerated.GetCurrentTime()

		if err := r.writer.InsertJobExecSample(ctx, row); err != nil {
			logger.Error().Err(err).
				Int64("river_job_id", row.RiverJobID).
				Str("kind", row.Kind).
				Msg("failed to write job_exec_sample row; continuing")
		}
	}
}
