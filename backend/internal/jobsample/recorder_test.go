package jobsample

import (
	"context"
	"errors"
	"testing"
	"time"

	"personal-crm/backend/internal/repository"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptrTime is a small helper for the nullable *time.Time JobRow fields.
func ptrTime(t time.Time) *time.Time { return &t }

func TestEventToSample_Completed(t *testing.T) {
	t.Parallel()
	attempted := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	finalized := attempted.Add(3 * time.Second)

	ev := &river.Event{
		Kind: river.EventKindJobCompleted,
		Job: &rivertype.JobRow{
			ID:          42,
			Kind:        "test_kind",
			Queue:       "default",
			Attempt:     1,
			AttemptedAt: ptrTime(attempted),
			FinalizedAt: ptrTime(finalized),
			State:       rivertype.JobStateCompleted,
		},
		JobStats: &river.JobStatistics{
			RunDuration:       3 * time.Second,
			QueueWaitDuration: 250 * time.Millisecond,
		},
	}

	row, ok := eventToSample(ev)
	require.True(t, ok)
	assert.Equal(t, int64(42), row.RiverJobID)
	assert.Equal(t, "test_kind", row.Kind)
	assert.Equal(t, "default", row.Queue)
	assert.Equal(t, attempted, row.AttemptedAt)
	// FinalizedAt is River's authoritative value (passthrough).
	assert.Equal(t, finalized, row.FinalizedAt)
	assert.Equal(t, 1, row.Attempt)
	assert.Equal(t, "completed", row.State)
	assert.Equal(t, int64(250), row.QueueWaitMs)
	// eventToSample must NOT stamp CreatedAt.
	assert.True(t, row.CreatedAt.IsZero())
}

func TestEventToSample_ClockSkew_ClampsFinalizedAt(t *testing.T) {
	t.Parallel()
	// AttemptedAt comes from the DB clock and FinalizedAt from the client
	// clock; a client clock lagging the DB clock by more than the run time
	// yields FinalizedAt < AttemptedAt, which the interval check constraint
	// would reject. eventToSample must clamp instead of emitting a row the
	// insert will drop.
	attempted := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	finalized := attempted.Add(-40 * time.Millisecond)

	ev := &river.Event{
		Kind: river.EventKindJobCompleted,
		Job: &rivertype.JobRow{
			ID:          43,
			Kind:        "test_kind",
			Queue:       "default",
			Attempt:     1,
			AttemptedAt: ptrTime(attempted),
			FinalizedAt: ptrTime(finalized),
			State:       rivertype.JobStateCompleted,
		},
		JobStats: &river.JobStatistics{
			RunDuration:       2 * time.Millisecond,
			QueueWaitDuration: 250 * time.Millisecond,
		},
	}

	row, ok := eventToSample(ev)
	require.True(t, ok)
	assert.Equal(t, attempted, row.AttemptedAt)
	assert.Equal(t, attempted, row.FinalizedAt, "FinalizedAt must be clamped to AttemptedAt, never before it")
}

func TestEventToSample_RetryableFailure_SynthesizesFinalizedAt(t *testing.T) {
	t.Parallel()
	attempted := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	ev := &river.Event{
		Kind: river.EventKindJobFailed,
		Job: &rivertype.JobRow{
			ID:          7,
			Kind:        "external_kind",
			Queue:       "default",
			Attempt:     2,
			AttemptedAt: ptrTime(attempted),
			FinalizedAt: nil, // retryable failure — not finalized
			// River bumped scheduled_at to a FUTURE retry time; irrelevant here
			// because wait comes from QueueWaitDuration, not scheduled_at.
			ScheduledAt: attempted.Add(30 * time.Second),
			State:       rivertype.JobStateRetryable,
		},
		JobStats: &river.JobStatistics{
			RunDuration:       5 * time.Second,
			QueueWaitDuration: 1200 * time.Millisecond,
		},
	}

	row, ok := eventToSample(ev)
	require.True(t, ok)
	// finalized_at synthesized as attempted_at + RunDuration.
	assert.Equal(t, attempted.Add(5*time.Second), row.FinalizedAt)
	assert.Equal(t, "retryable", row.State)
	assert.Equal(t, 2, row.Attempt)
	assert.Equal(t, int64(1200), row.QueueWaitMs)
}

func TestEventToSample_FinalDiscard(t *testing.T) {
	t.Parallel()
	attempted := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	finalized := attempted.Add(2 * time.Second)

	ev := &river.Event{
		Kind: river.EventKindJobFailed,
		Job: &rivertype.JobRow{
			ID:          9,
			Kind:        "external_kind",
			Queue:       "default",
			Attempt:     5,
			AttemptedAt: ptrTime(attempted),
			FinalizedAt: ptrTime(finalized), // final discard IS finalized
			State:       rivertype.JobStateDiscarded,
		},
		JobStats: &river.JobStatistics{
			RunDuration:       2 * time.Second,
			QueueWaitDuration: 0,
		},
	}

	row, ok := eventToSample(ev)
	require.True(t, ok)
	assert.Equal(t, finalized, row.FinalizedAt)
	assert.Equal(t, "discarded", row.State)
	assert.Equal(t, int64(0), row.QueueWaitMs)
}

func TestEventToSample_Snoozed_SynthesizesFinalizedAt(t *testing.T) {
	t.Parallel()
	attempted := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	ev := &river.Event{
		Kind: river.EventKindJobSnoozed,
		Job: &rivertype.JobRow{
			ID:          21,
			Kind:        "todoist_task_op",
			Queue:       "default",
			Attempt:     0, // snooze decrements attempt; still occupied a slot
			AttemptedAt: ptrTime(attempted),
			FinalizedAt: nil, // snoozed jobs are rescheduled, not finalized
			// River bumps scheduled_at to the snooze time before the event.
			ScheduledAt: attempted.Add(5 * time.Minute),
			State:       rivertype.JobStateScheduled,
		},
		JobStats: &river.JobStatistics{
			RunDuration:       2 * time.Second,
			QueueWaitDuration: 300 * time.Millisecond,
		},
	}

	row, ok := eventToSample(ev)
	require.True(t, ok)
	// finalized_at synthesized as attempted_at + RunDuration (slot release).
	assert.Equal(t, attempted.Add(2*time.Second), row.FinalizedAt)
	assert.Equal(t, "scheduled", row.State)
	assert.Equal(t, 0, row.Attempt)
	assert.Equal(t, int64(300), row.QueueWaitMs)
	assert.GreaterOrEqual(t, row.QueueWaitMs, int64(0))
}

func TestEventToSample_Cancelled(t *testing.T) {
	t.Parallel()
	attempted := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	finalized := attempted.Add(1 * time.Second)

	ev := &river.Event{
		Kind: river.EventKindJobCancelled,
		Job: &rivertype.JobRow{
			ID:          22,
			Kind:        "todoist_task_op",
			Queue:       "default",
			Attempt:     1,
			AttemptedAt: ptrTime(attempted),
			FinalizedAt: ptrTime(finalized), // cancelled-while-running IS finalized
			State:       rivertype.JobStateCancelled,
		},
		JobStats: &river.JobStatistics{
			RunDuration:       1 * time.Second,
			QueueWaitDuration: 0,
		},
	}

	row, ok := eventToSample(ev)
	require.True(t, ok)
	assert.Equal(t, finalized, row.FinalizedAt)
	assert.Equal(t, "cancelled", row.State)
	assert.Equal(t, 1, row.Attempt)
}

func TestEventToSample_CancelledBeforeRun_Skipped(t *testing.T) {
	t.Parallel()
	// A job cancelled before it was ever worked has a nil AttemptedAt — it
	// occupied no slot, so it must be skipped (ok=false), not recorded.
	ev := &river.Event{
		Kind: river.EventKindJobCancelled,
		Job: &rivertype.JobRow{
			ID:          23,
			Kind:        "todoist_task_op",
			Queue:       "default",
			AttemptedAt: nil,
			FinalizedAt: ptrTime(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)),
			State:       rivertype.JobStateCancelled,
		},
		JobStats: &river.JobStatistics{},
	}

	_, ok := eventToSample(ev)
	assert.False(t, ok)
}

// TestEventToSample_NegativeWaitGuard is the direct regression test for the
// scheduled_at retry-mutation trap: a retryable event whose (mutated)
// scheduled_at is in the FUTURE must still produce a NON-NEGATIVE queue_wait_ms,
// because wait comes from QueueWaitDuration, not attempted_at - scheduled_at.
func TestEventToSample_NegativeWaitGuard(t *testing.T) {
	t.Parallel()
	attempted := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	ev := &river.Event{
		Kind: river.EventKindJobFailed,
		Job: &rivertype.JobRow{
			ID:          11,
			Kind:        "external_kind",
			Queue:       "default",
			Attempt:     1,
			AttemptedAt: ptrTime(attempted),
			FinalizedAt: nil,
			// scheduled_at AHEAD of attempted_at by a minute — attempted_at -
			// scheduled_at would be -60s. The stored wait must not be negative.
			ScheduledAt: attempted.Add(time.Minute),
			State:       rivertype.JobStateRetryable,
		},
		JobStats: &river.JobStatistics{
			RunDuration:       time.Second,
			QueueWaitDuration: 400 * time.Millisecond,
		},
	}

	row, ok := eventToSample(ev)
	require.True(t, ok)
	assert.GreaterOrEqual(t, row.QueueWaitMs, int64(0))
	assert.Equal(t, int64(400), row.QueueWaitMs)
}

func TestEventToSample_NilJobStats_SkippedNoPanic(t *testing.T) {
	t.Parallel()
	attempted := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	ev := &river.Event{
		Kind: river.EventKindJobCompleted,
		Job: &rivertype.JobRow{
			ID:          1,
			Kind:        "test_kind",
			Queue:       "default",
			AttemptedAt: ptrTime(attempted),
			FinalizedAt: ptrTime(attempted.Add(time.Second)),
			State:       rivertype.JobStateCompleted,
		},
		JobStats: nil, // must be skipped, not dereferenced (no panic)
	}

	require.NotPanics(t, func() {
		_, ok := eventToSample(ev)
		assert.False(t, ok)
	})
}

func TestEventToSample_NilAttemptedAt_Skipped(t *testing.T) {
	t.Parallel()
	ev := &river.Event{
		Kind: river.EventKindJobCompleted,
		Job: &rivertype.JobRow{
			ID:          1,
			Kind:        "test_kind",
			Queue:       "default",
			AttemptedAt: nil, // should never happen for completed/failed; guarded
			State:       rivertype.JobStateCompleted,
		},
		JobStats: &river.JobStatistics{},
	}

	_, ok := eventToSample(ev)
	assert.False(t, ok)
}

// fakeWriter records inserts and can fail on demand.
type fakeWriter struct {
	rows    []repository.JobExecSampleRow
	failIdx int // 1-based index of the call that should return an error; 0 = never
	calls   int
}

func (f *fakeWriter) InsertJobExecSample(_ context.Context, s repository.JobExecSampleRow) error {
	f.calls++
	if f.failIdx != 0 && f.calls == f.failIdx {
		return errors.New("simulated write failure")
	}
	f.rows = append(f.rows, s)
	return nil
}

func newCompletedEvent(id int64) *river.Event {
	attempted := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	return &river.Event{
		Kind: river.EventKindJobCompleted,
		Job: &rivertype.JobRow{
			ID:          id,
			Kind:        "test_kind",
			Queue:       "default",
			Attempt:     1,
			AttemptedAt: ptrTime(attempted),
			FinalizedAt: ptrTime(attempted.Add(time.Second)),
			State:       rivertype.JobStateCompleted,
		},
		JobStats: &river.JobStatistics{RunDuration: time.Second, QueueWaitDuration: 0},
	}
}

func TestRecorderRun_WritesEachEventAndStampsCreatedAt(t *testing.T) {
	t.Parallel()
	fw := &fakeWriter{}
	rec := NewRecorder(fw)

	ch := make(chan *river.Event, 4)
	for i := int64(1); i <= 3; i++ {
		ch <- newCompletedEvent(i)
	}
	close(ch)

	rec.Run(context.Background(), ch)

	require.Len(t, fw.rows, 3)
	for _, row := range fw.rows {
		assert.False(t, row.CreatedAt.IsZero(), "Run must stamp CreatedAt")
	}
}

func TestRecorderRun_WriteErrorDoesNotStopLoop(t *testing.T) {
	t.Parallel()
	// Fail the 2nd write; the 1st and 3rd must still be written.
	fw := &fakeWriter{failIdx: 2}
	rec := NewRecorder(fw)

	ch := make(chan *river.Event, 4)
	for i := int64(1); i <= 3; i++ {
		ch <- newCompletedEvent(i)
	}
	close(ch)

	rec.Run(context.Background(), ch)

	assert.Equal(t, 3, fw.calls, "all events must be processed despite a mid-stream write error")
	require.Len(t, fw.rows, 2, "the failed write must not append a row")
}
