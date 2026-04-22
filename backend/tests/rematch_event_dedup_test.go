package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
)

// TestRematch_EventSourceIDDedup exercises the FIRST idempotency layer:
// event.source_id unique-index. Two PublishTx calls with the same
// (source, source_id) must produce exactly one event row and one river
// job, even though a plausible publisher retry would call PublishTx
// twice (spec §3.4.4 + spec §4).
//
// Scenario: publish envelope with source="manual", source_id=<jobID>
// twice. First succeeds, second hits ON CONFLICT DO NOTHING and returns
// nil without enqueueing. DB assertions: 1 event row, 1 river job.
//
// This is a DIFFERENT test from rematch_dispatcher_unique_opts_test
// (which exercises River's UniqueOpts{ByArgs} as the second dedup
// layer, bypassing the event-layer dedup via distinct source_ids).
func TestRematch_EventSourceIDDedup(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	cfg.Database.MigrationsPath = getMigrationsPath()

	ctx := context.Background()
	require.NoError(t, db.RunMigrations(ctx, cfg.Database.URL, cfg.Database.MigrationsPath))

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	// Live river client so InsertTx behaves like production. TestOnly
	// skips maintenance startup delays. Noop worker registered for
	// rematch_dispatcher so river accepts the kind; we assert row
	// counts, not execution.
	workers := river.NewWorkers()
	river.AddWorker(workers, &rematchDispatcherNoopWorker{})

	riverClient, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 2},
		},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)
	require.NoError(t, riverClient.Start(ctx))
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer stopCancel()
		_ = riverClient.Stop(stopCtx)
	})

	eventRepo := repository.NewEventRepository(database.Queries)
	bus := events.NewBus(database.Pool, riverClient, eventRepo)

	contactID := uuid.New()
	jobID := uuid.New()
	sourceID := jobID.String()

	// Publish twice inside separate tx — same (source, source_id).
	publish := func() error {
		return pgx.BeginTxFunc(ctx, database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			payload, marshalErr := events.Marshal(events.KindContactMethodsAdded, events.ContactMethodsAddedPayload{
				Version:      1,
				ContactID:    contactID,
				Methods:      []events.ContactMethodRef{{Type: "email", Value: "dedup@example.com"}},
				RematchJobID: jobID,
			})
			if marshalErr != nil {
				return marshalErr
			}
			env := &events.Envelope{
				Source:     "manual",
				SourceID:   sourceID,
				Kind:       events.KindContactMethodsAdded,
				Payload:    payload,
				ObservedAt: accelerated.GetCurrentTime(),
			}
			return bus.PublishTx(ctx, tx, env)
		})
	}
	require.NoError(t, publish(), "first publish")
	require.NoError(t, publish(), "second publish (dedup)")

	// Exactly one event row for (source, source_id) — via sqlc-generated
	// repository query (core.md forbids raw SQL in Go, incl. tests).
	env1, err := eventRepo.FindEventBySource(ctx, "manual", sourceID)
	require.NoError(t, err)
	require.NotNil(t, env1)
	require.Equal(t, "manual", env1.Source)
	require.Equal(t, sourceID, env1.SourceID)
	// A second event row with the same (source, source_id) cannot exist —
	// the partial unique index enforces this at the DB level, so the
	// Find returning a single row IS the dedup evidence.

	// River job count requires an arbitrary args JSON filter which is
	// not reachable via the generated Querier. The river_job table is
	// not sqlc'd (it belongs to the river queue library). Raw SQL is
	// the only option for this assertion — pattern matches existing
	// sync_worker_leased_retry_test.go / sync_worker_load_test.go use.
	var riverJobCount int
	err = database.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM river_job
		 WHERE kind = 'rematch_dispatcher'
		   AND args->>'contact_id' = $1
		   AND args->>'rematch_job_id' = $2`,
		contactID.String(), jobID.String(),
	).Scan(&riverJobCount)
	require.NoError(t, err)
	require.Equal(t, 1, riverJobCount, "only one river job enqueued for deduped event")
}

// rematchDispatcherNoopWorker accepts and completes the kind without
// work. Used in dedup tests where we assert row counts, not execution.
type rematchDispatcherNoopWorker struct {
	river.WorkerDefaults[rematchDispatcherNoopArgs]
}

// rematchDispatcherNoopArgs mirrors consumerjobs.RematchDispatcherJobArgs
// so the Kind name matches.
type rematchDispatcherNoopArgs struct {
	EventID      uuid.UUID `json:"event_id"`
	ContactID    uuid.UUID `json:"contact_id"`
	RematchJobID uuid.UUID `json:"rematch_job_id"`
}

func (rematchDispatcherNoopArgs) Kind() string { return "rematch_dispatcher" }

func (*rematchDispatcherNoopWorker) Work(_ context.Context, _ *river.Job[rematchDispatcherNoopArgs]) error {
	return nil
}
