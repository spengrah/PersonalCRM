package repository

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/db"
)

// JobSampleRepository reads and writes job_exec_sample (the River
// job-execution sampling table) and runs the Tier-0 read over live river_job.
// It mirrors HealthRepository: it takes only a db.Querier and owns no
// cross-cutting logic. Every write is a plain per-event insert, so there is
// no *pgxpool.Pool field.
type JobSampleRepository struct {
	queries db.Querier
}

// NewJobSampleRepository constructs a JobSampleRepository.
func NewJobSampleRepository(queries db.Querier) *JobSampleRepository {
	return &JobSampleRepository{queries: queries}
}

// JobExecSampleRow mirrors the insert/read columns of job_exec_sample.
type JobExecSampleRow struct {
	RiverJobID  int64
	Kind        string
	Queue       string
	AttemptedAt time.Time
	FinalizedAt time.Time
	Attempt     int
	State       string
	QueueWaitMs int64
	CreatedAt   time.Time
}

// KindDurationStats is the per-kind duration summary shared by the run-duration
// and overall-wait queries (COUNT + p50/p95/max in seconds).
type KindDurationStats struct {
	Kind string
	N    int64
	P50  float64
	P95  float64
	Max  float64
}

// WaitSaturationRow is one per-kind row of the metric-2 gate query
// (JobExecWaitDuringSaturationByKind).
type WaitSaturationRow struct {
	Kind            string
	NWaiters        int64
	TotalWaitS      float64
	WaitedInWindowS float64
	SaturatedWaitS  float64
	P95WaitS        float64
}

// WaitBlameRow is one (wait_kind, running_kind, blame_slot_s) row of the
// decisive metric-2 slot-blame query.
type WaitBlameRow struct {
	WaitKind    string
	RunningKind string
	BlameSlotS  float64
}

// Tier0Row is one per-kind row of the Tier-0 read over live river_job.
type Tier0Row struct {
	Kind     string
	N        int64
	P50WaitS float64
	P50RunS  float64
}

// InsertJobExecSample writes one per-execution sample. CreatedAt is supplied by
// the caller from accelerated time; ON CONFLICT (river_job_id, attempt,
// attempted_at) dedups a re-delivered event without erroring (attempted_at is
// in the key because River reuses the decremented attempt across snooze
// re-executions, so it distinguishes distinct occupancy intervals).
func (r *JobSampleRepository) InsertJobExecSample(ctx context.Context, s JobExecSampleRow) error {
	if err := r.queries.InsertJobExecSample(ctx, db.InsertJobExecSampleParams{
		RiverJobID:  s.RiverJobID,
		Kind:        s.Kind,
		Queue:       s.Queue,
		AttemptedAt: s.AttemptedAt,
		FinalizedAt: s.FinalizedAt,
		Attempt:     int32(s.Attempt),
		State:       s.State,
		QueueWaitMs: s.QueueWaitMs,
		CreatedAt:   s.CreatedAt,
	}); err != nil {
		return fmt.Errorf("insert job exec sample: %w", err)
	}
	return nil
}

// TrimJobExecSamples deletes rows older than cutoff (accelerated-now minus the
// retention window, computed by the caller). Returns the number of rows deleted.
func (r *JobSampleRepository) TrimJobExecSamples(ctx context.Context, cutoff time.Time) (int64, error) {
	n, err := r.queries.TrimJobExecSamples(ctx, cutoff)
	if err != nil {
		return 0, fmt.Errorf("trim job exec samples: %w", err)
	}
	return n, nil
}

// MaxConcurrency returns the peak concurrent-slot count over [from,to].
func (r *JobSampleRepository) MaxConcurrency(ctx context.Context, from, to time.Time) (int, error) {
	n, err := r.queries.JobExecMaxConcurrency(ctx, db.JobExecMaxConcurrencyParams{
		WindowStart: from,
		WindowEnd:   to,
	})
	if err != nil {
		return 0, fmt.Errorf("job exec max concurrency: %w", err)
	}
	return int(n), nil
}

// SaturatedSeconds returns the total wall-seconds spent at concurrency >=
// threshold over [from,to].
func (r *JobSampleRepository) SaturatedSeconds(ctx context.Context, from, to time.Time, threshold int) (float64, error) {
	s, err := r.queries.JobExecSaturatedSeconds(ctx, db.JobExecSaturatedSecondsParams{
		Threshold:   int32(threshold),
		WindowStart: from,
		WindowEnd:   to,
	})
	if err != nil {
		return 0, fmt.Errorf("job exec saturated seconds: %w", err)
	}
	return s, nil
}

// RunDurationByKind returns per-kind run-duration percentiles over [from,to].
func (r *JobSampleRepository) RunDurationByKind(ctx context.Context, from, to time.Time) ([]KindDurationStats, error) {
	rows, err := r.queries.JobExecRunDurationByKind(ctx, db.JobExecRunDurationByKindParams{
		WindowStart: from,
		WindowEnd:   to,
	})
	if err != nil {
		return nil, fmt.Errorf("job exec run duration by kind: %w", err)
	}
	out := make([]KindDurationStats, 0, len(rows))
	for _, row := range rows {
		out = append(out, KindDurationStats{
			Kind: row.Kind,
			N:    row.N,
			P50:  row.P50RunS,
			P95:  row.P95RunS,
			Max:  row.MaxRunS,
		})
	}
	return out, nil
}

// WaitByKind returns per-kind overall wait percentiles over [from,to], from the
// stored queue_wait_ms.
func (r *JobSampleRepository) WaitByKind(ctx context.Context, from, to time.Time) ([]KindDurationStats, error) {
	rows, err := r.queries.JobExecWaitByKind(ctx, db.JobExecWaitByKindParams{
		WindowStart: from,
		WindowEnd:   to,
	})
	if err != nil {
		return nil, fmt.Errorf("job exec wait by kind: %w", err)
	}
	out := make([]KindDurationStats, 0, len(rows))
	for _, row := range rows {
		out = append(out, KindDurationStats{
			Kind: row.Kind,
			N:    row.N,
			P50:  row.P50WaitS,
			P95:  row.P95WaitS,
			Max:  row.MaxWaitS,
		})
	}
	return out, nil
}

// WaitDuringSaturationByKind returns the metric-2 gate rows over [from,to] at
// the given saturation threshold.
func (r *JobSampleRepository) WaitDuringSaturationByKind(ctx context.Context, from, to time.Time, threshold int) ([]WaitSaturationRow, error) {
	rows, err := r.queries.JobExecWaitDuringSaturationByKind(ctx, db.JobExecWaitDuringSaturationByKindParams{
		WindowStart: from,
		WindowEnd:   to,
		Threshold:   int32(threshold),
	})
	if err != nil {
		return nil, fmt.Errorf("job exec wait during saturation by kind: %w", err)
	}
	out := make([]WaitSaturationRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, WaitSaturationRow{
			Kind:            row.Kind,
			NWaiters:        row.NWaiters,
			TotalWaitS:      row.TotalWaitS,
			WaitedInWindowS: row.WaitedInWindowS,
			SaturatedWaitS:  row.SaturatedWaitS,
			P95WaitS:        row.P95WaitS,
		})
	}
	return out, nil
}

// WaitSlotBlameByKind returns the decisive metric-2 slot-blame rows
// (wait_kind, running_kind, blame_slot_s) over [from,to] at the given threshold.
func (r *JobSampleRepository) WaitSlotBlameByKind(ctx context.Context, from, to time.Time, threshold int) ([]WaitBlameRow, error) {
	rows, err := r.queries.JobExecWaitSlotBlameByKind(ctx, db.JobExecWaitSlotBlameByKindParams{
		WindowStart: from,
		WindowEnd:   to,
		Threshold:   int32(threshold),
	})
	if err != nil {
		return nil, fmt.Errorf("job exec wait slot blame by kind: %w", err)
	}
	out := make([]WaitBlameRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, WaitBlameRow{
			WaitKind:    row.WaitKind,
			RunningKind: row.RunningKind,
			BlameSlotS:  row.BlameSlotS,
		})
	}
	return out, nil
}

// Tier0StatsByKind runs the Tier-0 one-shot wait/run-by-kind read over live
// river_job rows finalized since cutoff (accelerated-now minus the window).
func (r *JobSampleRepository) Tier0StatsByKind(ctx context.Context, cutoff time.Time) ([]Tier0Row, error) {
	rows, err := r.queries.Tier0RiverJobStatsByKind(ctx, cutoff)
	if err != nil {
		return nil, fmt.Errorf("tier0 river job stats by kind: %w", err)
	}
	out := make([]Tier0Row, 0, len(rows))
	for _, row := range rows {
		out = append(out, Tier0Row{
			Kind:     row.Kind,
			N:        row.N,
			P50WaitS: row.P50WaitS,
			P50RunS:  row.P50RunS,
		})
	}
	return out, nil
}

// InsertRiverJobFullTimingForTest plants one FINISHED river_job with an
// explicit kind, state, scheduled_at, attempted_at, and finalized_at. TEST ONLY
// — the Tier-0 integration test uses it to drive Tier0StatsByKind's wait
// (attempted_at - scheduled_at) and run (finalized_at - attempted_at)
// percentiles against known values; production code never inserts river_job
// directly.
func (r *JobSampleRepository) InsertRiverJobFullTimingForTest(ctx context.Context, kind string, state db.RiverJobState, scheduledAt, attemptedAt, finalizedAt time.Time) error {
	if err := r.queries.TestInsertRiverJobFullTimingForTest(ctx, db.TestInsertRiverJobFullTimingForTestParams{
		Kind:        kind,
		State:       state,
		ScheduledAt: scheduledAt,
		AttemptedAt: &attemptedAt,
		FinalizedAt: &finalizedAt,
	}); err != nil {
		return fmt.Errorf("insert river job full timing for test: %w", err)
	}
	return nil
}

// ListJobExecSamplesByRiverJobIDForTest reads sample rows for the given
// river_job_ids. TEST ONLY — the real-Subscribe integration test reads rows
// back through it (raw SQL is banned in Go tests).
func (r *JobSampleRepository) ListJobExecSamplesByRiverJobIDForTest(ctx context.Context, riverJobIDs []int64) ([]JobExecSampleRow, error) {
	rows, err := r.queries.ListJobExecSamplesByRiverJobIDForTest(ctx, riverJobIDs)
	if err != nil {
		return nil, fmt.Errorf("list job exec samples by river job id: %w", err)
	}
	out := make([]JobExecSampleRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, JobExecSampleRow{
			RiverJobID:  row.RiverJobID,
			Kind:        row.Kind,
			Queue:       row.Queue,
			AttemptedAt: row.AttemptedAt,
			FinalizedAt: row.FinalizedAt,
			Attempt:     int(row.Attempt),
			State:       row.State,
			QueueWaitMs: row.QueueWaitMs,
			CreatedAt:   row.CreatedAt,
		})
	}
	return out, nil
}
