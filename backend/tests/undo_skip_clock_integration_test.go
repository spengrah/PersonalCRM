//go:build integration_testdb

package tests

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/stretchr/testify/require"
)

func TestUndoSkipClock_RestoresFutureDate(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	h := newSkipHarness(t, ctx)
	c := createMonthlySkipContact(t, h, ctx)
	seeded := skipUTCDate(cadence.Today(accelerated.GetCurrentTime()), 5)
	seedSkipContactBy(t, h, ctx, c.ID, seeded)
	_, err := h.contactSvc.SkipCycle(ctx, c.ID)
	require.NoError(t, err)
	got, err := h.contactSvc.UndoSkip(ctx, c.ID)
	require.NoError(t, err)
	require.Equal(t, cadence.CalendarDate(seeded), cadence.CalendarDate(*got.ContactBy))
	require.Nil(t, got.LastSkippedAt)
	require.Nil(t, got.LastSkippedContactBy)
	require.Nil(t, got.LastSkipReason)
	require.False(t, cadence.UndoSkipAvailable(got.LastSkippedAt, got.ContactBy, accelerated.GetCurrentTime()))
	_, err = h.contactSvc.UndoSkip(ctx, c.ID)
	require.ErrorIs(t, err, service.ErrUndoSkipUnavailable)
}

func TestUndoSkipClock_RestoresPastDateExactly(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	h := newSkipHarness(t, ctx)
	c := createMonthlySkipContact(t, h, ctx)
	seeded := skipUTCDate(cadence.Today(accelerated.GetCurrentTime()), -10)
	seedSkipContactBy(t, h, ctx, c.ID, seeded)
	_, err := h.contactSvc.SkipCycle(ctx, c.ID)
	require.NoError(t, err)
	got, err := h.contactSvc.UndoSkip(ctx, c.ID)
	require.NoError(t, err)
	require.Equal(t, cadence.CalendarDate(seeded), cadence.CalendarDate(*got.ContactBy))
	require.Nil(t, got.LastSkippedAt)
	require.Nil(t, got.LastSkippedContactBy)
	require.Nil(t, got.LastSkipReason)
	require.False(t, cadence.UndoSkipAvailable(got.LastSkippedAt, got.ContactBy, accelerated.GetCurrentTime()))
	_, err = h.contactSvc.UndoSkip(ctx, c.ID)
	require.ErrorIs(t, err, service.ErrUndoSkipUnavailable)
}

func TestUndoSkipClock_DoesNotReopenWindowOrReminder(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	h := newSkipHarness(t, ctx)
	c := createMonthlySkipContact(t, h, ctx)
	now := accelerated.GetCurrentTime()
	expiry := now.AddDate(0, 0, 7)
	seedSkipContactBy(t, h, ctx, c.ID, skipUTCDate(now, -10))
	require.NoError(t, h.contactRepo.TestSeedContactCadenceFields(ctx, c.ID, repository.TestCadenceSeed{LastOutreachAt: timePtrUndo(now.Add(-time.Hour)), AwaitingReplyUntil: &expiry}))
	task := seedSkipTask(t, h, ctx, c.ID, repository.ContactTaskStateManaged, "td-undo")
	_, err := h.contactSvc.SkipCycle(ctx, c.ID)
	require.NoError(t, err)
	_, err = h.contactSvc.UndoSkip(ctx, c.ID)
	require.NoError(t, err)
	got, err := h.contactRepo.GetContact(ctx, c.ID)
	require.NoError(t, err)
	require.Nil(t, got.AwaitingReplyUntil)
	require.Nil(t, got.LastSkippedAt)
	require.Nil(t, got.LastSkippedContactBy)
	require.Nil(t, got.LastSkipReason)
	require.False(t, cadence.UndoSkipAvailable(got.LastSkippedAt, got.ContactBy, accelerated.GetCurrentTime()))
	_, err = h.contactSvc.UndoSkip(ctx, c.ID)
	require.ErrorIs(t, err, service.ErrUndoSkipUnavailable)
	updated, err := h.taskRepo.GetContactTask(ctx, task.ID)
	require.NoError(t, err)
	require.Equal(t, repository.ContactTaskStateCompleted, updated.State)
	require.Equal(t, int64(1), closeOpCount(t, h, ctx, task.ID))
}

func TestUndoSkipClock_UnavailableOnceTodayReachesContactBy(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	for _, offset := range []int{0, -1} {
		t.Run(string(rune('a'+offset+1)), func(t *testing.T) {
			h := newSkipHarness(t, ctx)
			c := createMonthlySkipContact(t, h, ctx)
			seeded := skipUTCDate(cadence.Today(accelerated.GetCurrentTime()), -10)
			seedSkipContactBy(t, h, ctx, c.ID, seeded)
			_, err := h.contactSvc.SkipCycle(ctx, c.ID)
			require.NoError(t, err)
			reached := skipUTCDate(cadence.Today(accelerated.GetCurrentTime()), offset)
			seedSkipContactBy(t, h, ctx, c.ID, reached)
			current, err := h.contactRepo.GetContact(ctx, c.ID)
			require.NoError(t, err)
			require.False(t, cadence.UndoSkipAvailable(current.LastSkippedAt, current.ContactBy, accelerated.GetCurrentTime()))
			_, err = h.contactSvc.UndoSkip(ctx, c.ID)
			require.ErrorIs(t, err, service.ErrUndoSkipUnavailable)
		})
	}
}

func TestUndoSkipClock_ClearedByInboundWithoutClockMove(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	h := newSkipHarness(t, ctx)
	c := createMonthlySkipContact(t, h, ctx)
	seed := skipUTCDate(cadence.Today(accelerated.GetCurrentTime()), -10)
	seedSkipContactBy(t, h, ctx, c.ID, seed)
	_, err := h.contactSvc.SkipCycle(ctx, c.ID)
	require.NoError(t, err)
	skipped, err := h.contactRepo.GetContact(ctx, c.ID)
	require.NoError(t, err)
	before := *skipped.ContactBy
	_, err = h.contactSvc.RecordInteraction(ctx, repository.RecordInteractionRequest{ContactID: c.ID, Source: repository.InteractionSourceManual, Direction: repository.InteractionDirectionInbound, OccurredAt: accelerated.GetCurrentTime()})
	require.NoError(t, err)
	got, err := h.contactRepo.GetContact(ctx, c.ID)
	require.NoError(t, err)
	require.Equal(t, cadence.CalendarDate(before), cadence.CalendarDate(*got.ContactBy))
	require.Nil(t, got.LastSkippedAt)
	require.Nil(t, got.LastSkippedContactBy)
	require.Nil(t, got.LastSkipReason)
	require.False(t, cadence.UndoSkipAvailable(got.LastSkippedAt, got.ContactBy, accelerated.GetCurrentTime()))
	_, err = h.contactSvc.UndoSkip(ctx, c.ID)
	require.ErrorIs(t, err, service.ErrUndoSkipUnavailable)
}

func TestUndoSkipClock_ClearedByMutualWithClockMove(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	h := newSkipHarness(t, ctx)
	c := createMonthlySkipContact(t, h, ctx)
	seedSkipContactBy(t, h, ctx, c.ID, skipUTCDate(cadence.Today(accelerated.GetCurrentTime()), -10))
	_, err := h.contactSvc.SkipCycle(ctx, c.ID)
	require.NoError(t, err)
	seedSkipContactBy(t, h, ctx, c.ID, skipUTCDate(cadence.Today(accelerated.GetCurrentTime()), -1))
	_, err = h.contactSvc.RecordInteraction(ctx, repository.RecordInteractionRequest{ContactID: c.ID, Source: repository.InteractionSourceManual, Direction: repository.InteractionDirectionMutual, OccurredAt: accelerated.GetCurrentTime()})
	require.NoError(t, err)
	got, err := h.contactRepo.GetContact(ctx, c.ID)
	require.NoError(t, err)
	require.Nil(t, got.LastSkippedAt)
	require.Nil(t, got.LastSkippedContactBy)
	require.Nil(t, got.LastSkipReason)
	require.False(t, cadence.UndoSkipAvailable(got.LastSkippedAt, got.ContactBy, accelerated.GetCurrentTime()))
	_, err = h.contactSvc.UndoSkip(ctx, c.ID)
	require.ErrorIs(t, err, service.ErrUndoSkipUnavailable)
}

func TestUndoSkipClock_ClearedByCadenceEdit(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	h := newSkipHarness(t, ctx)
	c := createMonthlySkipContact(t, h, ctx)
	seedSkipContactBy(t, h, ctx, c.ID, skipUTCDate(cadence.Today(accelerated.GetCurrentTime()), -10))
	_, err := h.contactSvc.SkipCycle(ctx, c.ID)
	require.NoError(t, err)
	weekly := "weekly"
	_, err = h.contactSvc.UpdateContact(ctx, c.ID, repository.UpdateContactRequest{FullName: c.FullName, Cadence: &weekly})
	require.NoError(t, err)
	got, err := h.contactRepo.GetContact(ctx, c.ID)
	require.NoError(t, err)
	require.Nil(t, got.LastSkippedAt)
	require.Nil(t, got.LastSkippedContactBy)
	require.Nil(t, got.LastSkipReason)
	require.False(t, cadence.UndoSkipAvailable(got.LastSkippedAt, got.ContactBy, accelerated.GetCurrentTime()))
	_, err = h.contactSvc.UndoSkip(ctx, c.ID)
	require.ErrorIs(t, err, service.ErrUndoSkipUnavailable)
}

func TestUndoSkipClock_ClearedByContactByOverride(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	h := newSkipHarness(t, ctx)
	c := createMonthlySkipContact(t, h, ctx)
	seedSkipContactBy(t, h, ctx, c.ID, skipUTCDate(cadence.Today(accelerated.GetCurrentTime()), -10))
	_, err := h.contactSvc.SkipCycle(ctx, c.ID)
	require.NoError(t, err)
	override := skipUTCDate(cadence.Today(accelerated.GetCurrentTime()), 30)
	tx, err := h.database.Pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, h.cadenceUpdater.ApplyContactByOverride(ctx, tx, c.ID, &override))
	require.NoError(t, tx.Commit(ctx))
	got, err := h.contactRepo.GetContact(ctx, c.ID)
	require.NoError(t, err)
	require.Nil(t, got.LastSkippedAt)
	require.Nil(t, got.LastSkippedContactBy)
	require.Nil(t, got.LastSkipReason)
	require.False(t, cadence.UndoSkipAvailable(got.LastSkippedAt, got.ContactBy, accelerated.GetCurrentTime()))
	_, err = h.contactSvc.UndoSkip(ctx, c.ID)
	require.ErrorIs(t, err, service.ErrUndoSkipUnavailable)
}

func TestUndoSkipClock_ClearedByDeleteRollbackRecompute(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	h := newSkipHarness(t, ctx)
	c := createMonthlySkipContact(t, h, ctx)
	seedSkipContactBy(t, h, ctx, c.ID, skipUTCDate(cadence.Today(accelerated.GetCurrentTime()), -10))
	_, err := h.contactSvc.SkipCycle(ctx, c.ID)
	require.NoError(t, err)
	tx, err := h.database.Pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, h.contactRepo.RecomputeContactDatesAfterDeleteTx(ctx, tx, c.ID, accelerated.GetCurrentTime(), config.TestConfig().Watchdog))
	require.NoError(t, tx.Commit(ctx))
	got, err := h.contactRepo.GetContact(ctx, c.ID)
	require.NoError(t, err)
	require.Nil(t, got.LastSkippedAt)
	require.Nil(t, got.LastSkippedContactBy)
	require.Nil(t, got.LastSkipReason)
	require.False(t, cadence.UndoSkipAvailable(got.LastSkippedAt, got.ContactBy, accelerated.GetCurrentTime()))
	_, err = h.contactSvc.UndoSkip(ctx, c.ID)
	require.ErrorIs(t, err, service.ErrUndoSkipUnavailable)
}

func TestUndoSkipClock_ClearedByMerge(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	h := newSkipHarness(t, ctx)
	source := createMonthlySkipContact(t, h, ctx)
	target := createMonthlySkipContact(t, h, ctx)
	seedSkipContactBy(t, h, ctx, target.ID, skipUTCDate(cadence.Today(accelerated.GetCurrentTime()), -10))
	_, err := h.contactSvc.SkipCycle(ctx, target.ID)
	require.NoError(t, err)
	_, err = h.contactSvc.MergeContacts(ctx, service.MergeContactsRequest{SourceContactID: source.ID, TargetContactID: target.ID})
	require.NoError(t, err)
	got, err := h.contactRepo.GetContact(ctx, target.ID)
	require.NoError(t, err)
	require.Nil(t, got.LastSkippedAt)
	require.Nil(t, got.LastSkippedContactBy)
	require.Nil(t, got.LastSkipReason)
	require.False(t, cadence.UndoSkipAvailable(got.LastSkippedAt, got.ContactBy, accelerated.GetCurrentTime()))
	_, err = h.contactSvc.UndoSkip(ctx, target.ID)
	require.ErrorIs(t, err, service.ErrUndoSkipUnavailable)
}

func timePtrUndo(value time.Time) *time.Time { return &value }
