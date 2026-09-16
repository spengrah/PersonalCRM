//go:build integration_testdb

package tests

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/todoist"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
)

type skipHarness struct {
	database        *db.Database
	contactSvc      *service.ContactService
	contactRepo     *repository.ContactRepository
	taskRepo        *repository.ContactTaskRepository
	interactionRepo *repository.InteractionRepository
	cadenceUpdater  *consumer.CadenceUpdater
}

func newSkipHarness(t *testing.T, ctx context.Context) *skipHarness {
	t.Helper()
	database, cfg := newIsolatedRiverTestDB(t, ctx)
	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	methodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	taskRepo := repository.NewContactTaskRepository(database.Queries)
	eventRepo := repository.NewEventRepository(database.Queries)
	workers := river.NewWorkers()
	river.AddWorker(workers, &knowledgeCacheNoopWorker{})
	river.AddWorker(workers, &mergeCloseNoopWorker{})
	river.AddWorker(workers, &followUpTestNoopOp{})
	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues:  map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency}},
		Workers: workers, TestOnly: true,
	})
	require.NoError(t, err)
	bus := events.NewBus(database.Pool, client, eventRepo)
	assertSvc, cache := buildKnowledgeDeps(t, database, bus)
	cadenceUpdater := buildCadenceUpdaterForTest(t, database)
	contactSvc := service.NewContactService(database, contactRepo, methodRepo, interactionRepo, taskRepo, nil, nil, cadenceUpdater, assertSvc, cache, nil)
	contactSvc.SetTaskCloseEnqueuer(client, true)
	return &skipHarness{database: database, contactSvc: contactSvc, contactRepo: contactRepo, taskRepo: taskRepo, interactionRepo: interactionRepo, cadenceUpdater: cadenceUpdater}
}

func createMonthlySkipContact(t *testing.T, h *skipHarness, ctx context.Context) *repository.Contact {
	t.Helper()
	cadenceValue := "monthly"
	c, _, err := h.contactSvc.CreateContact(ctx, repository.CreateContactRequest{FullName: "Skip Clock Contact", Cadence: &cadenceValue}, nil)
	require.NoError(t, err)
	return c
}

func seedSkipContactBy(t *testing.T, h *skipHarness, ctx context.Context, id uuid.UUID, date time.Time) {
	t.Helper()
	require.NoError(t, h.contactRepo.TestSeedContactCadenceFields(ctx, id, repository.TestCadenceSeed{ContactBy: &date}))
}

func skipUTCDate(base time.Time, offset int) time.Time {
	return time.Date(base.Year(), base.Month(), base.Day()+offset, 0, 0, 0, 0, time.UTC)
}

func seedSkipTask(t *testing.T, h *skipHarness, ctx context.Context, contactID uuid.UUID, state repository.ContactTaskState, externalID string) *repository.ContactTask {
	t.Helper()
	task, err := h.taskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{ContactID: contactID, Provider: todoist.SourceName, Kind: contacttask.KindReachOut, Lifecycle: contacttask.LifecycleFollowUpLoop, State: string(state), ExternalTaskID: externalID})
	require.NoError(t, err)
	return task
}

func closeOpCount(t *testing.T, h *skipHarness, ctx context.Context, taskID uuid.UUID) int64 {
	t.Helper()
	n, err := h.taskRepo.CountTodoistOpJobsByOp(ctx, taskID, consumerjobs.TaskOpClose)
	require.NoError(t, err)
	return n
}

func TestSkipClock_AdvancesOneCycleLaterOf(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	h := newSkipHarness(t, ctx)
	today := cadence.Today(accelerated.GetCurrentTime())
	for _, offset := range []int{-10, 5} {
		c := createMonthlySkipContact(t, h, ctx)
		seeded := skipUTCDate(today, offset)
		seedSkipContactBy(t, h, ctx, c.ID, seeded)
		_, err := h.contactSvc.SkipCycle(ctx, c.ID)
		require.NoError(t, err)
		got, err := h.contactRepo.GetContact(ctx, c.ID)
		require.NoError(t, err)
		base := today.Add(12 * time.Hour)
		if offset > 0 {
			base = seeded.In(time.Local).Add(12 * time.Hour)
		}
		want := cadence.CalculateContactBy(base, cadence.CadenceMonthly)
		require.Equal(t, cadence.CalendarDate(want), cadence.CalendarDate(*got.ContactBy))
	}
}

func TestSkipClock_RecordsSkipState(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	h := newSkipHarness(t, ctx)
	today := cadence.Today(accelerated.GetCurrentTime())
	c := createMonthlySkipContact(t, h, ctx)
	seeded := skipUTCDate(today, -10)
	now := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	lastContacted := now.Add(-4 * time.Hour).Truncate(time.Microsecond)
	lastInteractionAt := now.Add(-3 * time.Hour).Truncate(time.Microsecond)
	lastOutreachAt := now.Add(-2 * time.Hour).Truncate(time.Microsecond)
	lastResponseAt := now.Add(-time.Hour).Truncate(time.Microsecond)
	require.NoError(t, h.contactRepo.TestSeedContactCadenceFields(ctx, c.ID, repository.TestCadenceSeed{
		ContactBy:         &seeded,
		LastContacted:     &lastContacted,
		LastInteractionAt: &lastInteractionAt,
		LastOutreachAt:    &lastOutreachAt,
		LastResponseAt:    &lastResponseAt,
	}))
	before, err := h.contactRepo.GetContact(ctx, c.ID)
	require.NoError(t, err)
	capturedAt := accelerated.GetCurrentTime()
	_, err = h.contactSvc.SkipCycle(ctx, c.ID)
	require.NoError(t, err)
	got, err := h.contactRepo.GetContact(ctx, c.ID)
	require.NoError(t, err)
	require.Equal(t, cadence.CalendarDate(seeded), cadence.CalendarDate(*got.LastSkippedContactBy))
	require.NotNil(t, got.LastSkippedAt)
	require.Equal(t, service.SkipReasonUI, *got.LastSkipReason)
	delta := got.LastSkippedAt.Sub(capturedAt)
	if delta < 0 {
		delta = -delta
	}
	require.LessOrEqual(t, delta, time.Minute)
	require.Equal(t, before.LastContacted, got.LastContacted)
	require.Equal(t, before.LastInteractionAt, got.LastInteractionAt)
	require.Equal(t, before.LastOutreachAt, got.LastOutreachAt)
	require.Equal(t, before.LastResponseAt, got.LastResponseAt)
	require.Equal(t, int64(0), func() int64 {
		n, e := h.interactionRepo.CountContactInteractions(ctx, c.ID)
		require.NoError(t, e)
		return n
	}())
}

func TestSkipClock_EndsWindowAndClosesReminderExactlyOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	for _, tc := range []struct {
		name, external string
		state          repository.ContactTaskState
		wantClose      int64
	}{
		{name: "finished create", state: repository.ContactTaskStateManaged, external: "td-finished", wantClose: 1},
		{name: "create in flight", state: repository.ContactTaskStatePendingRemoteCreate, external: "", wantClose: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newSkipHarness(t, ctx)
			c := createMonthlySkipContact(t, h, ctx)
			now := accelerated.GetCurrentTime()
			expiry := now.AddDate(0, 0, 7)
			outbound := now.Add(-time.Hour)
			require.NoError(t, h.contactRepo.TestSeedContactCadenceFields(ctx, c.ID, repository.TestCadenceSeed{LastOutreachAt: &outbound, AwaitingReplyUntil: &expiry}))
			task := seedSkipTask(t, h, ctx, c.ID, tc.state, tc.external)
			_, err := h.contactSvc.SkipCycle(ctx, c.ID)
			require.NoError(t, err)
			got, err := h.contactRepo.GetContact(ctx, c.ID)
			require.NoError(t, err)
			require.Nil(t, got.AwaitingReplyUntil)
			require.False(t, cadence.IsAwaitingReply(got.LastOutreachAt, got.LastResponseAt, got.AwaitingReplyUntil, accelerated.GetCurrentTime()))
			updated, err := h.taskRepo.GetContactTask(ctx, task.ID)
			require.NoError(t, err)
			require.Equal(t, repository.ContactTaskStateCompleted, updated.State)
			require.Equal(t, tc.external, updated.ExternalTaskID)
			require.Equal(t, tc.wantClose, closeOpCount(t, h, ctx, task.ID))
			// For an in-flight create, finalizeCreate enqueues the close after recording the remote id.
		})
	}
}

func TestSkipClock_LeavesOverdueSet(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	h := newSkipHarness(t, ctx)
	today := cadence.Today(accelerated.GetCurrentTime())
	a := createMonthlySkipContact(t, h, ctx)
	b := createMonthlySkipContact(t, h, ctx)
	aDate := skipUTCDate(today, -10)
	bDate := skipUTCDate(today, 5)
	seedSkipContactBy(t, h, ctx, a.ID, aDate)
	seedSkipContactBy(t, h, ctx, b.ID, bDate)
	before, err := h.contactSvc.ListOverdueContacts(ctx)
	require.NoError(t, err)
	require.Contains(t, overdueIDs(before), a.ID)
	require.NotContains(t, overdueIDs(before), b.ID)
	_, err = h.contactSvc.SkipCycle(ctx, a.ID)
	require.NoError(t, err)
	after, err := h.contactSvc.ListOverdueContacts(ctx)
	require.NoError(t, err)
	require.NotContains(t, overdueIDs(after), a.ID)
}

func overdueIDs(entries []service.OverdueContact) []uuid.UUID {
	ids := make([]uuid.UUID, len(entries))
	for i := range entries {
		ids[i] = entries[i].Contact.ID
	}
	return ids
}

func TestSkipClock_RequiresCadence(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	h := newSkipHarness(t, ctx)
	c := createMonthlySkipContact(t, h, ctx)
	_, err := h.contactSvc.UpdateContact(ctx, c.ID, repository.UpdateContactRequest{FullName: c.FullName, Cadence: nil})
	require.NoError(t, err)
	before, err := h.contactRepo.GetContact(ctx, c.ID)
	require.NoError(t, err)
	_, err = h.contactSvc.SkipCycle(ctx, c.ID)
	require.ErrorIs(t, err, service.ErrSkipRequiresCadence)
	after, err := h.contactRepo.GetContact(ctx, c.ID)
	require.NoError(t, err)
	require.Equal(t, before.ContactBy, after.ContactBy)
}
