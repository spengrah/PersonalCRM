package tests

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
)

// eventBusNoopArgs is a test-only river job kind registered so the client
// has at least one worker at Start (river rejects empty Workers). No job is
// ever enqueued against it in PR 2 — the event Bus's registry stub returns
// an empty slice for every kind.
type eventBusNoopArgs struct{}

func (eventBusNoopArgs) Kind() string { return "event_bus_test_noop" }

type eventBusNoopWorker struct {
	river.WorkerDefaults[eventBusNoopArgs]
}

func (*eventBusNoopWorker) Work(_ context.Context, _ *river.Job[eventBusNoopArgs]) error {
	return nil
}

// newEventBusTestDB opens the package-clone DB. These tests publish only
// interaction.manual events, which do not route to async consumer jobs, so they
// do not need a live River fetch loop or a per-test database clone.
func newEventBusTestDB(t *testing.T, ctx context.Context) (*db.Database, *config.Config) {
	t.Helper()
	return newSharedTestDB(t, ctx)
}

// newEventBusTestBus builds a Bus with a River client that is never started.
// InsertTx does not need a fetch loop, and these tests use event kinds whose
// routing table returns no jobs.
func newEventBusTestBus(t *testing.T, ctx context.Context, database *db.Database, cfg *config.Config) *events.Bus {
	t.Helper()
	eventRepo := repository.NewEventRepository(database.Queries)

	workers := river.NewWorkers()
	river.AddWorker(workers, &eventBusNoopWorker{})

	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)

	return events.NewBus(database.Pool, client, eventRepo)
}

// uniqueSourceID returns a per-subtest unique source_id prefix so shared-DB
// row accumulation from previous runs never collides.
func uniqueSourceID(prefix string) string {
	return prefix + ":" + uuid.NewString()
}

// mustMarshalManualPayload builds a valid JSON payload for
// interaction.manual. Used as a simple, schema-compliant payload across
// several tests; the specific kind doesn't matter here because we're
// exercising the storage path, not the decode path.
func mustMarshalManualPayload(t *testing.T, contactID uuid.UUID, at time.Time) json.RawMessage {
	t.Helper()
	raw, err := events.Marshal(events.KindInteractionManual, events.InteractionManualPayload{
		Version:    1,
		ContactID:  contactID,
		Direction:  "mutual",
		OccurredAt: at,
	})
	require.NoError(t, err)
	return raw
}

func TestEventRepository_InsertAndFetch_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	database, _ := newEventBusTestDB(t, ctx)
	repo := repository.NewEventRepository(database.Queries)

	contactID := uuid.New()
	observed := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	payload := mustMarshalManualPayload(t, contactID, observed)

	env := &events.Envelope{
		Source:     "telegram",
		SourceID:   uniqueSourceID("insert_and_fetch"),
		Kind:       events.KindInteractionManual,
		Payload:    payload,
		ObservedAt: observed,
	}

	tx, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, repo.InsertEvent(ctx, tx, env))
	require.NoError(t, tx.Commit(ctx))
	require.NotEqual(t, uuid.Nil, env.ID, "envelope ID should be populated post-insert")

	got, err := repo.GetEvent(ctx, env.ID)
	require.NoError(t, err)
	require.Equal(t, env.ID, got.ID)
	require.Equal(t, env.Source, got.Source)
	require.Equal(t, env.SourceID, got.SourceID)
	require.Equal(t, env.Kind, got.Kind)
	require.Equal(t, env.ObservedAt, got.ObservedAt)

	found, err := repo.FindEventBySource(ctx, env.Source, env.SourceID)
	require.NoError(t, err)
	require.Equal(t, env.ID, found.ID)
}

func TestEventRepository_DuplicateSourceID_ReturnsErrDuplicate_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	database, _ := newEventBusTestDB(t, ctx)
	repo := repository.NewEventRepository(database.Queries)

	sourceID := uniqueSourceID("dup")
	observed := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	payload := mustMarshalManualPayload(t, uuid.New(), observed)

	// First insert — succeeds.
	env1 := &events.Envelope{
		Source:     "telegram",
		SourceID:   sourceID,
		Kind:       events.KindInteractionManual,
		Payload:    payload,
		ObservedAt: observed,
	}
	tx1, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, repo.InsertEvent(ctx, tx1, env1))
	require.NoError(t, tx1.Commit(ctx))
	originalID := env1.ID
	require.NotEqual(t, uuid.Nil, originalID)

	// Second insert with the SAME (source, source_id) — ErrDuplicate.
	env2 := &events.Envelope{
		Source:     "telegram",
		SourceID:   sourceID,
		Kind:       events.KindInteractionManual,
		Payload:    payload,
		ObservedAt: observed,
	}
	tx2, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	err = repo.InsertEvent(ctx, tx2, env2)
	require.ErrorIs(t, err, db.ErrDuplicate)
	require.NoError(t, tx2.Rollback(ctx))
	// env2.ID should remain unset since the insert failed.
	require.Equal(t, uuid.Nil, env2.ID)

	// Original row is unchanged.
	got, err := repo.GetEvent(ctx, originalID)
	require.NoError(t, err)
	require.Equal(t, originalID, got.ID)
}

func TestEventRepository_NullSourceID_AllowsMultipleInserts_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	database, _ := newEventBusTestDB(t, ctx)
	repo := repository.NewEventRepository(database.Queries)

	observed := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	payload := mustMarshalManualPayload(t, uuid.New(), observed)

	insert := func() uuid.UUID {
		env := &events.Envelope{
			Source:     "manual_null_test",
			SourceID:   "", // → NULL at the DB
			Kind:       events.KindInteractionManual,
			Payload:    payload,
			ObservedAt: observed,
		}
		tx, err := database.Pool.Begin(ctx)
		require.NoError(t, err)
		require.NoError(t, repo.InsertEvent(ctx, tx, env))
		require.NoError(t, tx.Commit(ctx))
		require.NotEqual(t, uuid.Nil, env.ID)
		return env.ID
	}

	id1 := insert()
	id2 := insert()
	require.NotEqual(t, id1, id2, "NULL source_id rows must get distinct DB-generated IDs")

	// FindEventBySource with empty sourceID short-circuits to ErrNotFound.
	_, err := repo.FindEventBySource(ctx, "manual_null_test", "")
	require.ErrorIs(t, err, db.ErrNotFound)
}

func TestEventRepository_GetEvent_NotFound_ReturnsErrNotFound_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	database, _ := newEventBusTestDB(t, ctx)
	repo := repository.NewEventRepository(database.Queries)

	_, err := repo.GetEvent(ctx, uuid.New())
	require.ErrorIs(t, err, db.ErrNotFound)
}

func TestEventRepository_FindEventBySource_NotFound_ReturnsErrNotFound_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	database, _ := newEventBusTestDB(t, ctx)
	repo := repository.NewEventRepository(database.Queries)

	_, err := repo.FindEventBySource(ctx, "nonexistent_source", uniqueSourceID("missing"))
	require.ErrorIs(t, err, db.ErrNotFound)
}

func TestBus_PublishTx_Succeeds_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	database, cfg := newEventBusTestDB(t, ctx)
	bus := newEventBusTestBus(t, ctx, database, cfg)
	repo := repository.NewEventRepository(database.Queries)

	sourceID := uniqueSourceID("publish_tx_ok")
	observed := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	env := &events.Envelope{
		Source:     "telegram",
		SourceID:   sourceID,
		Kind:       events.KindInteractionManual,
		Payload:    mustMarshalManualPayload(t, uuid.New(), observed),
		ObservedAt: observed,
	}

	tx, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, bus.PublishTx(ctx, tx, env))
	require.NoError(t, tx.Commit(ctx))
	require.NotEqual(t, uuid.Nil, env.ID)

	// Row persisted.
	found, err := repo.FindEventBySource(ctx, env.Source, env.SourceID)
	require.NoError(t, err)
	require.Equal(t, env.ID, found.ID)
}

func TestBus_PublishTx_DuplicateSourceID_IsNoOp_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	database, cfg := newEventBusTestDB(t, ctx)
	bus := newEventBusTestBus(t, ctx, database, cfg)
	repo := repository.NewEventRepository(database.Queries)

	sourceID := uniqueSourceID("publish_tx_dup")
	observed := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	payload := mustMarshalManualPayload(t, uuid.New(), observed)

	env1 := &events.Envelope{
		Source:     "telegram",
		SourceID:   sourceID,
		Kind:       events.KindInteractionManual,
		Payload:    payload,
		ObservedAt: observed,
	}
	tx1, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, bus.PublishTx(ctx, tx1, env1))
	require.NoError(t, tx1.Commit(ctx))
	firstID := env1.ID

	// Second publish with same (source, source_id) — idempotent no-op.
	env2 := &events.Envelope{
		Source:     "telegram",
		SourceID:   sourceID,
		Kind:       events.KindInteractionManual,
		Payload:    payload,
		ObservedAt: observed,
	}
	tx2, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, bus.PublishTx(ctx, tx2, env2))
	require.NoError(t, tx2.Commit(ctx))

	// FindEventBySource still returns exactly one row — the original.
	found, err := repo.FindEventBySource(ctx, env1.Source, env1.SourceID)
	require.NoError(t, err)
	require.Equal(t, firstID, found.ID)
}

func TestBus_Publish_OpensOwnTransaction_AndCommits_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	database, cfg := newEventBusTestDB(t, ctx)
	bus := newEventBusTestBus(t, ctx, database, cfg)
	repo := repository.NewEventRepository(database.Queries)

	sourceID := uniqueSourceID("publish")
	observed := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	env := &events.Envelope{
		Source:     "telegram",
		SourceID:   sourceID,
		Kind:       events.KindInteractionManual,
		Payload:    mustMarshalManualPayload(t, uuid.New(), observed),
		ObservedAt: observed,
	}

	require.NoError(t, bus.Publish(ctx, env))
	require.NotEqual(t, uuid.Nil, env.ID)

	found, err := repo.FindEventBySource(ctx, env.Source, env.SourceID)
	require.NoError(t, err)
	require.Equal(t, env.ID, found.ID)
}

func TestBus_PublishTx_ValidationError_NoRowPersisted_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	database, cfg := newEventBusTestDB(t, ctx)
	bus := newEventBusTestBus(t, ctx, database, cfg)
	repo := repository.NewEventRepository(database.Queries)

	sourceID := uniqueSourceID("publish_tx_invalid")
	observed := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

	tx, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	// Envelope missing Kind — validation fails before InsertEvent runs.
	env := &events.Envelope{
		Source:     "telegram",
		SourceID:   sourceID,
		Payload:    mustMarshalManualPayload(t, uuid.New(), observed),
		ObservedAt: observed,
	}
	err = bus.PublishTx(ctx, tx, env)
	require.Error(t, err)
	require.NoError(t, tx.Rollback(ctx))

	_, err = repo.FindEventBySource(ctx, "telegram", sourceID)
	require.True(t, errors.Is(err, db.ErrNotFound), "validation-failed row must not be persisted")
}

func TestBus_PublishTx_PreGeneratedID_IsRespected_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()
	ctx := context.Background()
	database, cfg := newEventBusTestDB(t, ctx)
	bus := newEventBusTestBus(t, ctx, database, cfg)
	repo := repository.NewEventRepository(database.Queries)

	// Pre-generate the ID — the INSERT query's COALESCE(narg_id, gen_random_uuid())
	// must respect it (Design Decision 2).
	preID := uuid.New()
	sourceID := uniqueSourceID("pregen_id")
	observed := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	env := &events.Envelope{
		ID:         preID,
		Source:     "telegram",
		SourceID:   sourceID,
		Kind:       events.KindInteractionManual,
		Payload:    mustMarshalManualPayload(t, uuid.New(), observed),
		ObservedAt: observed,
	}

	tx, err := database.Pool.Begin(ctx)
	require.NoError(t, err)
	require.NoError(t, bus.PublishTx(ctx, tx, env))
	require.NoError(t, tx.Commit(ctx))
	require.Equal(t, preID, env.ID)

	got, err := repo.GetEvent(ctx, preID)
	require.NoError(t, err)
	require.Equal(t, preID, got.ID)
}
