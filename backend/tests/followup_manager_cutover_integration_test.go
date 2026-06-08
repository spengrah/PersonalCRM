// Integration coverage for the post-cutover FollowUpManager.
//
// Acceptance matrix:
//   - Backdated outbound (non-manual) → no contact_task row, no Todoist call.
//     Closes #267. Covered by TestIntegration_FollowUpManager_BackdatedOutbound.
//   - Out-of-order delivery (inbound at T, outbound at T-1d) → guard 2
//     fires; no follow-up created. Covered by
//     TestIntegration_FollowUpManager_OutOfOrderSkips.
//   - Cutover outbound-fresh → pending_remote_create row inserted + Todoist
//     create job enqueued inside the event tx. Covered by
//     TestIntegration_FollowUpManager_OutboundFreshCreatesPendingRow.
//   - Inbound completion → existing pending_remote_create row transitions to
//     completed in tx; create worker handles the close-while-pending race
//     itself (single-owner). Covered by
//     TestIntegration_FollowUpManager_InboundCompletesPending.
//   - Manual source bypasses the backdated guard. Covered by
//     TestIntegration_FollowUpManager_ManualSourceBypassesBackdatedGuard.
//
// These tests exercise the consumer's HandleEvent against a live DB,
// focusing on observable SQL side-effects (contact_task rows + states +
// river_job enqueues). Worker-level Todoist behavior is unit-tested in
// consumer/todoist_followup_workers_test.go with a Todoist client mock;
// the create worker's three-phase body isn't re-exercised here because
// the phase 2 HTTP call requires a real or mocked Todoist server
// harness beyond the scope of a DB integration test.
package tests

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/todoist"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// getMigrationsPathFollowUp returns the migrations path for CI test bootstrap.
// Integration tests in backend/tests/ must call db.RunMigrations before
// db.NewDatabase because CI runs bare PostgreSQL.
func getMigrationsPathFollowUp() string {
	return "../migrations"
}

// newFollowUpIntegrationDB returns a live DB connection for integration tests.
func newFollowUpIntegrationDB(t *testing.T) (*db.Database, func()) {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	ctx := context.Background()
	require.NoError(t, db.RunMigrations(ctx, databaseURL, getMigrationsPathFollowUp()))
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	return database, func() { database.Close() }
}

// followUpIntegrationEnv bundles the repos + consumer the cutover tests
// share. The tests use their own river client (not the test-event-bus
// harness) because follow-up create / close jobs need to stay in the
// enqueued state for inspection — we don't want a worker to pick them
// up and HTTP out to a nonexistent Todoist.
type followUpIntegrationEnv struct {
	database    *db.Database
	gen         *factory.Generator
	contactRepo *repository.ContactRepository
	taskRepo    *repository.ContactTaskRepository
	interRepo   *repository.InteractionRepository
	eventRepo   *repository.EventRepository
	claimRepo   *repository.EventConsumerClaimRepository
	riverClient *river.Client[pgx.Tx]
	manager     *consumer.FollowUpManager
	bus         *events.Bus
	watchdog    config.WatchdogConfig
	// decisions is populated by the DecisionObserver installed on the
	// manager; integration tests inspect it to assert on the manager's
	// terminal classification (action, skip reason) for each branch
	// without a DB round trip.
	decisions *[]consumer.Decision
}

// newFollowUpIntegrationEnv builds a FollowUpManager with a real river
// client that has NO worker registered for the Todoist follow-up
// create/close kinds. That lets these tests assert on the river_job
// table (presence + kind + args) without the worker actually running
// and drifting state during assertions.
func newFollowUpIntegrationEnv(t *testing.T) (*followUpIntegrationEnv, func()) {
	t.Helper()
	database, closeFn := newFollowUpIntegrationDB(t)
	ctx := context.Background()

	contactRepo := repository.NewContactRepository(database.Queries)
	contactRepo.SetPool(database.Pool)
	taskRepo := repository.NewContactTaskRepository(database.Queries)
	interRepo := repository.NewInteractionRepository(database.Queries)
	eventRepo := repository.NewEventRepository(database.Queries)
	claimRepo := repository.NewEventConsumerClaimRepository(database.Queries)

	// River client with zero workers — jobs enqueue into river_job but
	// never run. Exactly what we need to assert on enqueue + dedupe
	// without worker-driven HTTP side effects.
	workers := river.NewWorkers()
	river.AddWorker(workers, &followUpTestNoopCreate{})
	river.AddWorker(workers, &followUpTestNoopClose{})
	river.AddWorker(workers, &followUpTestNoopRefresh{})
	// PublishTx's fanout for interaction.recorded enqueues Cadence +
	// FollowUpManager jobs; those kinds must be registered even though
	// the test never runs them.
	river.AddWorker(workers, &followUpTestNoopCadence{})
	river.AddWorker(workers, &followUpTestNoopFollowUpMgr{})
	river.AddWorker(workers, &followUpTestNoopInteractionRec{})
	riverClient, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 1},
		},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)
	bus := events.NewBus(database.Pool, riverClient, eventRepo)

	settings := func(context.Context) (*todoist.Settings, string, error) {
		return &todoist.Settings{
			ProjectID:             "proj-test",
			LabelName:             "followup",
			IntegrationInstanceID: "inst-test",
		}, "token-test", nil
	}
	clientFactory := func(string) todoist.Client {
		return &followUpIntegrationNoopClient{}
	}

	watchdog := config.TestConfig().Watchdog
	if watchdog.WeeklyDays == 0 {
		watchdog = config.WatchdogConfig{
			WeeklyDays: 3, BiweeklyDays: 5, MonthlyDays: 7,
			QuarterlyDays: 14, BiannualDays: 21, AnnualDays: 21,
		}
	}
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
		clientFactory,
		"http://localhost:3000",
		watchdog,
	)
	var decisions []consumer.Decision
	manager.SetDecisionObserver(func(d consumer.Decision) {
		decisions = append(decisions, d)
	})

	gen, _ := migrationGenerator(t)
	env := &followUpIntegrationEnv{
		database:    database,
		gen:         gen,
		contactRepo: contactRepo,
		taskRepo:    taskRepo,
		interRepo:   interRepo,
		eventRepo:   eventRepo,
		claimRepo:   claimRepo,
		riverClient: riverClient,
		manager:     manager,
		bus:         bus,
		watchdog:    watchdog,
		decisions:   &decisions,
	}
	return env, func() {
		// Don't Start() the client — no workers, and we only want the
		// database-side InsertTx path. Stop is a no-op on an unstarted
		// client but call it for symmetry.
		_ = riverClient.Stop(ctx)
		closeFn()
	}
}

// followUpTestNoopCreate / noop close / noop refresh satisfy river's
// "known kind" requirement so InsertTx succeeds; Work never runs
// because we never call Start().
type followUpTestNoopCreate struct {
	river.WorkerDefaults[consumerjobs.TodoistFollowUpCreateJobArgs]
}

func (*followUpTestNoopCreate) Work(context.Context, *river.Job[consumerjobs.TodoistFollowUpCreateJobArgs]) error {
	return nil
}

type followUpTestNoopClose struct {
	river.WorkerDefaults[consumerjobs.TodoistFollowUpCloseJobArgs]
}

func (*followUpTestNoopClose) Work(context.Context, *river.Job[consumerjobs.TodoistFollowUpCloseJobArgs]) error {
	return nil
}

type followUpTestNoopRefresh struct {
	river.WorkerDefaults[consumerjobs.TodoistFollowUpRefreshJobArgs]
}

func (*followUpTestNoopRefresh) Work(context.Context, *river.Job[consumerjobs.TodoistFollowUpRefreshJobArgs]) error {
	return nil
}

type followUpTestNoopCadence struct {
	river.WorkerDefaults[consumerjobs.CadenceUpdaterJobArgs]
}

func (*followUpTestNoopCadence) Work(context.Context, *river.Job[consumerjobs.CadenceUpdaterJobArgs]) error {
	return nil
}

type followUpTestNoopFollowUpMgr struct {
	river.WorkerDefaults[consumerjobs.FollowUpManagerJobArgs]
}

func (*followUpTestNoopFollowUpMgr) Work(context.Context, *river.Job[consumerjobs.FollowUpManagerJobArgs]) error {
	return nil
}

type followUpTestNoopInteractionRec struct {
	river.WorkerDefaults[consumerjobs.InteractionRecorderJobArgs]
}

func (*followUpTestNoopInteractionRec) Work(context.Context, *river.Job[consumerjobs.InteractionRecorderJobArgs]) error {
	return nil
}

// followUpIntegrationNoopClient is a Todoist client that never gets
// called — integration tests don't invoke workers, so the client
// factory's returned value is held but not exercised. If something
// accidentally invokes Sync the test fails loudly.
type followUpIntegrationNoopClient struct{}

func (*followUpIntegrationNoopClient) QuickAdd(context.Context, string, string) (*todoist.QuickAddTask, error) {
	panic("followUpIntegrationNoopClient.QuickAdd must not be called in integration tests")
}

func (*followUpIntegrationNoopClient) Sync(context.Context, string, []string, []todoist.SyncCommand) (*todoist.SyncResponse, error) {
	panic("followUpIntegrationNoopClient.Sync must not be called in integration tests")
}

// seedContact creates a contact with an optional cadence.
func (e *followUpIntegrationEnv) seedContact(t *testing.T, cadenceStr string) *repository.Contact {
	t.Helper()
	ctx := context.Background()
	req := repository.CreateContactRequest{FullName: e.gen.Contact(factory.WithNoMethods()).FullName}
	if cadenceStr != "" {
		req.Cadence = &cadenceStr
	}
	c, err := e.contactRepo.CreateContact(ctx, req)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = e.contactRepo.HardDeleteContact(ctx, c.ID)
	})
	return c
}

// recordedEnv builds a V2 interaction.recorded envelope + inserts the
// event row (so event_consumer_claim.TryClaimTx's FK is valid).
func (e *followUpIntegrationEnv) recordedEnv(
	t *testing.T, contactID uuid.UUID, direction, source string, occurredAt time.Time, cadenceStr string,
) *events.Envelope {
	t.Helper()
	payload := events.InteractionRecordedPayload{
		Version:       2,
		ContactID:     contactID,
		InteractionID: uuid.New(),
		Direction:     direction,
		OccurredAt:    occurredAt,
		Source:        source,
	}
	if cadenceStr != "" {
		payload.PrevCadenceValue = &cadenceStr
	}
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	env := &events.Envelope{
		ID:         uuid.New(),
		Kind:       events.KindInteractionRecorded,
		Source:     source,
		SourceID:   uuid.NewString(),
		Payload:    raw,
		ObservedAt: occurredAt,
	}
	ctx := context.Background()
	// Insert the event row so claim + shadow FK constraints hold.
	require.NoError(t, pgx.BeginTxFunc(ctx, e.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		return e.bus.PublishTx(ctx, tx, env)
	}))
	return env
}

// countContactTaskRows counts follow-up tasks for the contact.
func (e *followUpIntegrationEnv) countContactTaskRows(t *testing.T, contactID uuid.UUID) int {
	t.Helper()
	ctx := context.Background()
	rows, err := e.taskRepo.ListContactTasksFiltered(ctx, contactID, nil, nil, ptr(contacttask.LifecycleFollowUpLoop))
	require.NoError(t, err)
	return len(rows)
}

// countRiverJobsByKind returns the count of river_job rows of a given
// kind related to the contact_task (via args JSON membership).
// Delegates to the repository's test-only sqlc wrapper so Go test code
// doesn't inline raw SQL (core.md rule 2).
func (e *followUpIntegrationEnv) countRiverJobsByKind(t *testing.T, kind string, contactTaskID uuid.UUID) int {
	t.Helper()
	ctx := context.Background()
	n, err := e.taskRepo.CountRiverJobsByContactTask(ctx, kind, contactTaskID)
	require.NoError(t, err)
	return int(n)
}

func ptr[T any](v T) *T { return &v }

// applyInEventTx opens a tx, calls manager.HandleEvent inside, and
// commits. Returns the post-commit closure (may be nil) and any error.
func (e *followUpIntegrationEnv) applyInEventTx(t *testing.T, env *events.Envelope) func(context.Context) {
	t.Helper()
	ctx := context.Background()
	var postCommit func(context.Context)
	err := pgx.BeginTxFunc(ctx, e.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		pc, err := e.manager.HandleEvent(ctx, tx, env)
		postCommit = pc
		return err
	})
	require.NoError(t, err)
	return postCommit
}

// TestIntegration_FollowUpManager_BackdatedOutbound asserts guard 1: a
// 90-day-old telegram outbound with weekly cadence (3-day watchdog)
// produces a skip observation and NO contact_task row. Closes #267.
func TestIntegration_FollowUpManager_BackdatedOutbound(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newFollowUpIntegrationEnv(t)
	defer cleanup()
	contact := env.seedContact(t, "weekly")

	occurred := accelerated.GetCurrentTime().Add(-90 * 24 * time.Hour)
	recorded := env.recordedEnv(t, contact.ID, repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, occurred, "weekly")

	postCommit := env.applyInEventTx(t, recorded)
	assert.Nil(t, postCommit, "backdated skip must not return a post-commit closure")

	assert.Equal(t, 0, env.countContactTaskRows(t, contact.ID), "backdated outbound must not create a contact_task row")

	// Decision classification: skip with reason=backdated.
	require.Len(t, *env.decisions, 1)
	d := (*env.decisions)[0]
	assert.Equal(t, repository.FollowUpActionSkip, d.Action)
	assert.Equal(t, repository.FollowUpSkipReasonBackdated, d.SkipReason)
}

// TestIntegration_FollowUpManager_ManualSourceBypassesBackdatedGuard
// asserts that a 90-day-old MANUAL outbound is NOT skipped by guard 1;
// it proceeds to create the pending_remote_create row.
func TestIntegration_FollowUpManager_ManualSourceBypassesBackdatedGuard(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newFollowUpIntegrationEnv(t)
	defer cleanup()
	contact := env.seedContact(t, "weekly")

	occurred := accelerated.GetCurrentTime().Add(-90 * 24 * time.Hour)
	recorded := env.recordedEnv(t, contact.ID, repository.InteractionDirectionOutbound, repository.InteractionSourceManual, occurred, "weekly")

	env.applyInEventTx(t, recorded)

	assert.Equal(t, 1, env.countContactTaskRows(t, contact.ID),
		"manual source must bypass the backdated guard and create a pending_remote_create row")
}

// TestIntegration_FollowUpManager_OutOfOrderSkips asserts guard 2: if
// an inbound response is already on record after the outbound's
// occurred_at, the outbound is skipped.
func TestIntegration_FollowUpManager_OutOfOrderSkips(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newFollowUpIntegrationEnv(t)
	defer cleanup()
	ctx := context.Background()
	contact := env.seedContact(t, "weekly")

	// Record an inbound at T+1h FIRST — this is the "response after the
	// outbound" guard 2 checks for.
	outboundAt := accelerated.GetCurrentTime().Add(-1 * time.Hour)
	inboundAt := outboundAt.Add(1 * time.Hour)
	_, err := env.interRepo.CreateInteraction(ctx, repository.CreateInteractionRequest{
		ContactID:  contact.ID,
		Source:     repository.InteractionSourceTelegram,
		OccurredAt: inboundAt,
		Direction:  repository.InteractionDirectionInbound,
	})
	require.NoError(t, err)

	// Now publish the "older" outbound — guard 2 should fire.
	recorded := env.recordedEnv(t, contact.ID, repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, outboundAt, "weekly")
	env.applyInEventTx(t, recorded)

	assert.Equal(t, 0, env.countContactTaskRows(t, contact.ID), "out-of-order outbound must not create a follow-up")
	require.Len(t, *env.decisions, 1)
	d := (*env.decisions)[0]
	assert.Equal(t, repository.FollowUpActionSkip, d.Action)
	assert.Equal(t, repository.FollowUpSkipReasonOutOfOrder, d.SkipReason)
}

// TestIntegration_FollowUpManager_OutboundFreshCreatesPendingRow asserts
// the cutover happy-path: an outbound with weekly cadence + no prior
// response yields a pending_remote_create row with a non-empty
// idempotency key, and enqueues a TodoistFollowUpCreateJob.
func TestIntegration_FollowUpManager_OutboundFreshCreatesPendingRow(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newFollowUpIntegrationEnv(t)
	defer cleanup()
	ctx := context.Background()
	contact := env.seedContact(t, "weekly")

	occurred := accelerated.GetCurrentTime()
	recorded := env.recordedEnv(t, contact.ID, repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, occurred, "weekly")
	env.applyInEventTx(t, recorded)

	pending, err := env.taskRepo.FindPendingFollowUp(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Equal(t, repository.ContactTaskStatePendingRemoteCreate, pending.State, "step-1 insert lands in state=pending_remote_create")
	assert.Empty(t, pending.ExternalTaskID, "step-1 insert has empty external_task_id until step-2 finalize")
	require.NotNil(t, pending.IdempotencyKey, "step-1 insert must populate idempotency_key")
	assert.NotEmpty(t, *pending.IdempotencyKey)

	// E2 call-site wire-format lock: the stored marker_json must decode to
	// (reach_out, followup_loop) with the contact's id and the configured
	// integration instance — proving the follow-up create passes the correct
	// fields to EncodeMarker.
	markerJSON, ok := pending.Metadata["marker_json"].(string)
	require.True(t, ok, "pending follow-up metadata must carry marker_json")
	marker, ok := contacttask.DecodeMarker(markerJSON)
	require.True(t, ok, "marker_json must decode as a CRM marker")
	assert.Equal(t, contact.ID.String(), marker.ContactID)
	assert.Equal(t, contacttask.KindReachOut, marker.Kind)
	assert.Equal(t, contacttask.LifecycleFollowUpLoop, marker.Lifecycle)
	assert.Equal(t, "inst-test", marker.Instance)

	// river_job row: kind=todoist_followup_create, args contains contact_task_id.
	n := env.countRiverJobsByKind(t, (consumerjobs.TodoistFollowUpCreateJobArgs{}).Kind(), pending.ID)
	assert.Equal(t, 1, n, "a single TodoistFollowUpCreateJob must be enqueued in the same tx")

	// Decision classification: create action with matching idempotency key.
	require.Len(t, *env.decisions, 1)
	d := (*env.decisions)[0]
	assert.Equal(t, repository.FollowUpActionCreate, d.Action)
	require.NotNil(t, d.WouldIdempotencyKey)
	assert.Equal(t, *pending.IdempotencyKey, *d.WouldIdempotencyKey)
	_ = recorded // envelope id is no longer read after decision-observer move
}

// TestIntegration_FollowUpManager_InboundCompletesPending asserts that
// when a pending_remote_create follow-up exists and an inbound fires,
// the row transitions to state='completed' and — per the single-owner
// rule — NO TodoistFollowUpCloseJob is enqueued (create worker will
// handle the race when it runs).
func TestIntegration_FollowUpManager_InboundCompletesPending(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newFollowUpIntegrationEnv(t)
	defer cleanup()
	ctx := context.Background()
	contact := env.seedContact(t, "weekly")

	// Seed a pending_remote_create row via the outbound path first.
	occurred := accelerated.GetCurrentTime()
	outboundEnv := env.recordedEnv(t, contact.ID, repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, occurred, "weekly")
	env.applyInEventTx(t, outboundEnv)
	pending, err := env.taskRepo.FindPendingFollowUp(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Equal(t, repository.ContactTaskStatePendingRemoteCreate, pending.State)

	// Then publish an inbound response. It should transition the
	// pending row to completed in the event tx.
	inboundEnv := env.recordedEnv(t, contact.ID, repository.InteractionDirectionInbound, repository.InteractionSourceTelegram, occurred.Add(1*time.Hour), "weekly")
	env.applyInEventTx(t, inboundEnv)

	got, err := env.taskRepo.GetContactTask(ctx, pending.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.ContactTaskStateCompleted, got.State, "inbound must flip pending_remote_create → completed")

	// Single-owner rule: no close job enqueued for a pending_remote_create
	// row. The create worker handles the create-then-close race itself.
	closeN := env.countRiverJobsByKind(t, (consumerjobs.TodoistFollowUpCloseJobArgs{}).Kind(), pending.ID)
	assert.Equal(t, 0, closeN, "single-owner rule: no close job when pending row was still pending_remote_create")

	// Decision classification: outbound→create, then inbound→complete.
	require.Len(t, *env.decisions, 2)
	assert.Equal(t, repository.FollowUpActionCreate, (*env.decisions)[0].Action)
	assert.Equal(t, repository.FollowUpActionComplete, (*env.decisions)[1].Action)
	_ = inboundEnv // envelope id is no longer read after decision-observer move
}

// TestIntegration_FollowUpManager_DuplicateEventClaimBlocks asserts
// that replaying the same event envelope a second time finds the
// existing claim and returns early without inserting a duplicate row.
// Guards the durable-dedupe invariant: two deliveries of the same
// event never both land follow-up work.
func TestIntegration_FollowUpManager_DuplicateEventClaimBlocks(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newFollowUpIntegrationEnv(t)
	defer cleanup()
	contact := env.seedContact(t, "weekly")
	ctx := context.Background()

	occurred := accelerated.GetCurrentTime()
	recorded := env.recordedEnv(t, contact.ID, repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, occurred, "weekly")
	env.applyInEventTx(t, recorded)

	first, err := env.taskRepo.FindPendingFollowUp(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, first)

	// Replay the same envelope — the claim already exists, so the
	// consumer returns nil immediately.
	env.applyInEventTx(t, recorded)

	rows, err := env.taskRepo.ListContactTasksFiltered(ctx, contact.ID, nil, nil, ptr(contacttask.LifecycleFollowUpLoop))
	require.NoError(t, err)
	require.Len(t, rows, 1, "replay must not insert a duplicate row — claim dedupe enforcement")
	assert.Equal(t, first.ID, rows[0].ID, "same row returned on replay")
}

// TestIntegration_FollowUpManager_UniqueLiveCollisionRecovers forces
// the savepoint recovery path by driving two concurrent outbound
// HandleEvent calls for the same contact. Each tx opens its own DB
// session, both pass guard 3 (neither sees a pending follow-up), and
// one hits idx_contact_task_followup_unique_live when inserting.
// The losing tx must recover via savepoint rollback + refresh rather
// than aborting the outer interaction tx.
//
// Uses SERIALIZABLE isolation to make the race deterministic: both
// txs observe an empty guard-3 snapshot, then the second tx's insert
// collides and triggers the recovery. We don't need goroutines — we
// can just open two sequential txs that each keep their snapshot open
// past their own guard-3 read.
func TestIntegration_FollowUpManager_UniqueLiveCollisionRecovers(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newFollowUpIntegrationEnv(t)
	defer cleanup()
	ctx := context.Background()
	contact := env.seedContact(t, "weekly")

	// Build two distinct envelopes for the same contact so the consumer
	// takes distinct claims (event_consumer_claim is keyed by event_id).
	eventAOccurred := accelerated.GetCurrentTime().Add(-2 * time.Hour)
	eventBOccurred := accelerated.GetCurrentTime().Add(-1 * time.Hour)
	envA := env.recordedEnv(t, contact.ID, repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, eventAOccurred, "weekly")
	envB := env.recordedEnv(t, contact.ID, repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, eventBOccurred, "weekly")

	// Open tx A and run guard 3 by starting HandleEvent inside tx A.
	// We can't cleanly pause mid-HandleEvent, so instead we simulate
	// the race by: (1) opening tx A with REPEATABLE READ, snapshotting
	// before any follow-up row exists; (2) starting tx B, running
	// HandleEvent to completion (winner inserts the row); (3) resuming
	// tx A — it still sees no row in its snapshot, so its insert hits
	// the unique index and triggers recovery. After A commits, only
	// one row exists.
	txA, err := env.database.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	require.NoError(t, err)
	defer func() { _ = txA.Rollback(ctx) }()

	// Prime tx A's snapshot with a no-op read so the snapshot is taken
	// BEFORE tx B commits.
	_, err = env.taskRepo.FindPendingFollowUpTx(ctx, txA, contact.ID)
	require.True(t, err == nil || errors.Is(err, db.ErrNotFound))

	// Tx B: full HandleEvent, commits. Winner row is now visible to new
	// transactions but NOT to tx A's snapshot.
	_, err = txA.Conn().Exec(ctx, "-- snapshot primed")
	require.NoError(t, err)

	pcB, err := runHandleEventInFreshTx(ctx, env, envB)
	require.NoError(t, err)
	assert.Nil(t, pcB)

	// Tx A: HandleEvent. Guard 3 reads txA's snapshot → no row.
	// The insert hits idx_contact_task_followup_unique_live → savepoint
	// rollback → recovery via re-read of the winner's row → refresh.
	// NOTE: the re-read after savepoint rollback uses the outer tx
	// (tx A) which STILL doesn't see the winner in REPEATABLE READ,
	// so the recovery path falls through to "no live row on re-read;
	// skipping" and returns nil. The outer tx must commit cleanly.
	pcA, err := env.manager.HandleEvent(ctx, txA, envA)
	require.NoError(t, err, "savepoint recovery must not bubble an error out to the outer tx")
	assert.Nil(t, pcA)
	require.NoError(t, txA.Commit(ctx))

	// Only one follow-up row exists.
	rows, err := env.taskRepo.ListContactTasksFiltered(ctx, contact.ID, nil, nil, ptr(contacttask.LifecycleFollowUpLoop))
	require.NoError(t, err)
	require.Len(t, rows, 1, "collision recovery must not insert a duplicate row")
}

// runHandleEventInFreshTx opens a fresh tx on the pool, runs
// HandleEvent, commits, and returns the post-commit closure + error.
// Used by the concurrency test so it can drive a second consumer
// invocation while another tx holds an older snapshot.
func runHandleEventInFreshTx(ctx context.Context, env *followUpIntegrationEnv, envelope *events.Envelope) (func(context.Context), error) {
	var pc func(context.Context)
	err := pgx.BeginTxFunc(ctx, env.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		p, err := env.manager.HandleEvent(ctx, tx, envelope)
		pc = p
		return err
	})
	return pc, err
}

// TestIntegration_FollowUpManager_InboundClosesFinalizedRow asserts the
// post-update close-decision path: when a pending_remote_create row has
// been finalized to managed + populated external_task_id between guard 3
// and the completion update, the close job MUST still be enqueued.
// Pre-update state said "pending, skip close" but the row is actually
// finalized; post-update state carries the external_task_id which is
// the authoritative signal.
func TestIntegration_FollowUpManager_InboundClosesFinalizedRow(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := newFollowUpIntegrationEnv(t)
	defer cleanup()
	ctx := context.Background()
	contact := env.seedContact(t, "weekly")

	// Seed a fully-finalized follow-up row: state=managed with a
	// populated external_task_id, as if the create worker already ran.
	occurred := accelerated.GetCurrentTime()
	outbound := env.recordedEnv(t, contact.ID, repository.InteractionDirectionOutbound, repository.InteractionSourceTelegram, occurred, "weekly")
	env.applyInEventTx(t, outbound)
	pending, err := env.taskRepo.FindPendingFollowUp(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, pending)

	// Finalize the row to managed + external_task_id directly (simulating
	// the create worker's phase 3 write).
	require.NoError(t, pgx.BeginTxFunc(ctx, env.database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		_, err := env.taskRepo.UpdateContactTaskExternalIDTx(ctx, tx, pending.ID, "ext-finalized-"+uuid.NewString()[:8])
		return err
	}))

	// Now publish an inbound response. The consumer must enqueue a close
	// job because the row now has an external_task_id.
	inbound := env.recordedEnv(t, contact.ID, repository.InteractionDirectionInbound, repository.InteractionSourceTelegram, occurred.Add(1*time.Hour), "weekly")
	env.applyInEventTx(t, inbound)

	got, err := env.taskRepo.GetContactTask(ctx, pending.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.ContactTaskStateCompleted, got.State)
	assert.NotEmpty(t, got.ExternalTaskID, "external_task_id preserved through state update")

	closeN := env.countRiverJobsByKind(t, (consumerjobs.TodoistFollowUpCloseJobArgs{}).Kind(), pending.ID)
	assert.Equal(t, 1, closeN, "inbound on a finalized row must enqueue a close job (post-update state check)")
}

// silence "imported and not used" when rivertype isn't directly
// referenced on some tagged builds.
var _ rivertype.JobInsertResult
