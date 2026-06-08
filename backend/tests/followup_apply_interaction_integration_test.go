// Integration coverage for FollowUpManager.ApplyInteraction — the
// direct-invoke entrypoint used by ContactService.RecordInteraction's
// non-bus wrapper + Promote / Extend paths. ApplyInteraction hands a
// nil envelope to the cutover branches, so this file guards two
// specific regression risks:
//
//   - Nil-envelope log sites must not panic (envIDString returns a
//     placeholder for nil env so create/refresh/complete log lines
//     don't dereference env.ID).
//   - ErrTodoistUnconfigured must NOT roll back the interaction write.
//     The sentinel degrades follow-up creation to a log-only skip.
package tests

import (
	"context"
	"errors"
	"testing"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/todoist"

	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// applyInteractionEnv builds a cutover-mode FollowUpManager with a
// configurable Todoist settings closure so tests can inject success or
// ErrTodoistUnconfigured.
type applyInteractionEnv struct {
	database    *db.Database
	gen         *factory.Generator
	contactRepo *repository.ContactRepository
	taskRepo    *repository.ContactTaskRepository
	riverClient *river.Client[pgx.Tx]
	manager     *consumer.FollowUpManager
}

func newApplyInteractionEnv(t *testing.T, settingsErr error) (*applyInteractionEnv, func()) {
	t.Helper()
	database, closeFn := newFollowUpIntegrationDB(t)
	ctx := context.Background()

	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	taskRepo := repository.NewContactTaskRepository(database.Queries)
	interRepo := repository.NewInteractionRepository(database.Queries)
	claimRepo := repository.NewEventConsumerClaimRepository(database.Queries)

	workers := river.NewWorkers()
	river.AddWorker(workers, &followUpTestNoopCreate{})
	river.AddWorker(workers, &followUpTestNoopClose{})
	river.AddWorker(workers, &followUpTestNoopRefresh{})
	riverClient, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues:   map[string]river.QueueConfig{river.QueueDefault: {MaxWorkers: 1}},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)

	settings := func(context.Context) (*todoist.Settings, string, error) {
		if settingsErr != nil {
			return nil, "", settingsErr
		}
		return &todoist.Settings{ProjectID: "proj", LabelName: "followup", IntegrationInstanceID: "inst"}, "token", nil
	}
	factory := func(string) todoist.Client {
		return &followUpIntegrationNoopClient{}
	}
	watchdog := config.WatchdogConfig{WeeklyDays: 3, BiweeklyDays: 5, MonthlyDays: 7, QuarterlyDays: 14, BiannualDays: 21, AnnualDays: 21}

	manager := consumer.NewFollowUpManager(
		consumer.FollowUpModeCutover,
		claimRepo,
		contactRepo,
		taskRepo,
		taskRepo,
		interRepo,
		riverClient,
		database.Pool,
		settings,
		factory,
		"http://localhost:3000",
		watchdog,
	)

	gen, _ := migrationGenerator(t)
	env := &applyInteractionEnv{
		database:    database,
		gen:         gen,
		contactRepo: contactRepo,
		taskRepo:    taskRepo,
		riverClient: riverClient,
		manager:     manager,
	}
	return env, func() {
		_ = riverClient.Stop(ctx)
		closeFn()
	}
}

func (e *applyInteractionEnv) seedContact(t *testing.T, cadence string) *repository.Contact {
	t.Helper()
	ctx := context.Background()
	req := repository.CreateContactRequest{FullName: e.gen.Contact(factory.WithNoMethods()).FullName}
	if cadence != "" {
		req.Cadence = &cadence
	}
	contact, err := e.contactRepo.CreateContact(ctx, req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.contactRepo.HardDeleteContact(ctx, contact.ID) })
	return contact
}

// TestIntegration_ApplyInteraction_NilEnvelope_DoesNotPanic asserts the
// direct-invoke outbound create path does not dereference env.ID (which
// is nil here) in its log sites. Prior to envIDString, this panicked at
// the "idempotency key already present; skipping insert" and
// "todoist not configured; skipping create" WARN log lines.
func TestIntegration_ApplyInteraction_NilEnvelope_DoesNotPanic(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newApplyInteractionEnv(t, nil)
	defer cleanup()
	ctx := context.Background()
	contact := env.seedContact(t, "weekly")

	var postCommit func(context.Context)
	require.NotPanics(t, func() {
		require.NoError(t, pgx.BeginTxFunc(ctx, env.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			pc, err := env.manager.ApplyInteraction(ctx, tx, repository.ApplyInteractionRequest{
				ContactID:  contact.ID,
				Direction:  repository.InteractionDirectionOutbound,
				Source:     repository.InteractionSourceManual,
				OccurredAt: accelerated.GetCurrentTime(),
			})
			postCommit = pc
			return err
		}))
	})
	assert.Nil(t, postCommit, "outbound fresh create returns nil postCommit")

	pending, err := env.taskRepo.FindPendingFollowUp(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, pending, "direct-invoke create lands a pending_remote_create row")
	assert.Equal(t, repository.ContactTaskStatePendingRemoteCreate, pending.State)
}

// TestIntegration_ApplyInteraction_TodoistUnconfigured_NoRowNoRollback
// asserts that when the Todoist settings closure returns
// ErrTodoistUnconfigured, the outbound create branch degrades to a
// log-only skip: no contact_task row is written and the overall tx is
// NOT rolled back (the interaction layer caller can commit its other
// work — cadence advance, etc.).
func TestIntegration_ApplyInteraction_TodoistUnconfigured_NoRowNoRollback(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newApplyInteractionEnv(t, consumer.ErrTodoistUnconfigured)
	defer cleanup()
	ctx := context.Background()
	contact := env.seedContact(t, "weekly")

	// Drive the direct-invoke outbound path. The caller commits its own
	// tx; ApplyInteraction must not return an error that would force
	// rollback, and no contact_task row gets written.
	var applyErr error
	commitErr := pgx.BeginTxFunc(ctx, env.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, applyErr = env.manager.ApplyInteraction(ctx, tx, repository.ApplyInteractionRequest{
			ContactID:  contact.ID,
			Direction:  repository.InteractionDirectionOutbound,
			Source:     repository.InteractionSourceManual,
			OccurredAt: accelerated.GetCurrentTime(),
		})
		return applyErr
	})
	require.NoError(t, applyErr, "ErrTodoistUnconfigured must be non-fatal")
	require.NoError(t, commitErr, "tx must commit cleanly with no follow-up rollback")

	_, findErr := env.taskRepo.FindPendingFollowUp(ctx, contact.ID)
	require.ErrorIs(t, findErr, db.ErrNotFound, "no contact_task row created when Todoist is unconfigured")
}

// TestIntegration_ApplyInteraction_SettingsError_PropagatesRollback
// asserts the negative case: a non-sentinel error from the settings
// closure DOES propagate, so a transient Todoist outage during cutover
// doesn't silently swallow the state.
func TestIntegration_ApplyInteraction_SettingsError_PropagatesRollback(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newApplyInteractionEnv(t, errors.New("transient 503"))
	defer cleanup()
	ctx := context.Background()
	contact := env.seedContact(t, "weekly")

	var applyErr error
	commitErr := pgx.BeginTxFunc(ctx, env.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, applyErr = env.manager.ApplyInteraction(ctx, tx, repository.ApplyInteractionRequest{
			ContactID:  contact.ID,
			Direction:  repository.InteractionDirectionOutbound,
			Source:     repository.InteractionSourceManual,
			OccurredAt: accelerated.GetCurrentTime(),
		})
		return applyErr
	})
	require.Error(t, applyErr, "non-sentinel settings error must propagate")
	require.Error(t, commitErr, "tx must roll back on propagated error")

	_, findErr := env.taskRepo.FindPendingFollowUp(ctx, contact.ID)
	require.ErrorIs(t, findErr, db.ErrNotFound, "no partial state on rollback")
}
