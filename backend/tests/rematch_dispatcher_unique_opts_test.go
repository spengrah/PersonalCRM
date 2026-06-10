package tests

import (
	"context"
	"testing"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
)

// TestRematch_RiverUniqueOpts_DedupsByContactAndJobID exercises the
// SECOND idempotency layer: River's UniqueOpts{ByArgs} with
// river:"unique" tags on ContactID + RematchJobID.
//
// To exercise it specifically we bypass the event.source_id dedup by
// using DIFFERENT source_ids on each publish. Same (ContactID,
// RematchJobID) in both payloads → River's ByArgs hashes those fields
// and dedupes the second InsertTx to one queued job. Final assertion:
// one river_job row, not two.
//
// Companion test rematch_event_dedup_test.go covers the first layer
// (same source_id → event.source_id unique).
func TestRematch_RiverUniqueOpts_DedupsByContactAndJobID(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Per-test isolated clone: this client exercises River UniqueOpts dedup
	// (the count is already (contactID, jobID)-scoped, so it doesn't collide
	// on the shared DB; the clone keeps the cluster uniform).
	ctx := context.Background()
	database, _ := newIsolatedRiverTestDB(t, ctx)

	// Noop worker for rematch_dispatcher so river accepts the kind.
	// We assert row counts, not handler execution.
	workers := river.NewWorkers()
	river.AddWorker(workers, &rematchDispatcherNoopWorker{})

	riverClient, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 1},
		},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)
	// NOT starting the client — we want the first job to stay in the
	// queue so the second InsertTx hits UniqueOpts dedup against a
	// still-scheduled row.

	eventRepo := repository.NewEventRepository(database.Queries)
	bus := events.NewBus(database.Pool, riverClient, eventRepo)

	contactID := uuid.New()
	jobID := uuid.New()

	// Publish two events with DIFFERENT source_ids but SAME (contactID,
	// jobID) in payload. The event layer sees them as distinct (two
	// event rows); River's UniqueOpts{ByArgs} sees the args
	// {ContactID, RematchJobID} as identical and dedupes.
	publishWithSourceID := func(sourceID string) error {
		return pgx.BeginTxFunc(ctx, database.Pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
			payload, marshalErr := events.Marshal(events.KindContactMethodsAdded, events.ContactMethodsAddedPayload{
				Version:      1,
				ContactID:    contactID,
				Methods:      []events.ContactMethodRef{{Type: "email", Value: "a@b.c"}},
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

	sourceA := "src-A-" + uuid.NewString()
	sourceB := "src-B-" + uuid.NewString()

	require.NoError(t, publishWithSourceID(sourceA), "first publish")
	require.NoError(t, publishWithSourceID(sourceB), "second publish (UniqueOpts dedup)")

	// Two distinct event rows (source_id differs) — assert via the
	// sqlc-generated repo query (core.md forbids raw SQL in Go).
	envA, err := eventRepo.FindEventBySource(ctx, "manual", sourceA)
	require.NoError(t, err)
	require.NotNil(t, envA, "event A must persist")
	envB, err := eventRepo.FindEventBySource(ctx, "manual", sourceB)
	require.NoError(t, err)
	require.NotNil(t, envB, "event B must persist (different source_id → no event-layer dedup)")

	// Only one river job for the (ContactID, RematchJobID) tuple.
	// ByArgs with river:"unique" tags on ContactID + RematchJobID
	// collapses the second InsertTx to the existing job. Count via
	// sqlc-generated repo helper (raw SQL is forbidden by core.md).
	riverJobCount, err := eventRepo.CountRematchDispatcherJobs(ctx, contactID, jobID)
	require.NoError(t, err)
	require.Equal(t, int64(1), riverJobCount, "River UniqueOpts{ByArgs} must dedupe the second enqueue")
}
