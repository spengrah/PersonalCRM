package tests

import (
	"context"
	"fmt"
	"testing"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
)

// setupTestEventBus wires a real river client + InteractionRecorder +
// ContactService against the provided database. The client runs live
// (NOT TestOnly) so published events are picked up by the consumer
// worker and written asynchronously — the same flow production runs.
// Cleanup stops the river client on t.Cleanup.
//
// Use the returned *events.Bus in place of nil when constructing
// CalendarSyncProvider / CalendarRematchHandler / AggregationEngine in
// tests. After the publisher runs, call waitForInteractionBySourceRef /
// waitForInteractionCount / waitForTelegramMessagesProcessed to wait for
// the async consumer write.
//
// This replaces the PR 5 "direct path wrote first" synchronous behavior
// that pre-cutover tests relied on. Post-PR-6 there is no direct path —
// the async consumer is the sole writer (spec §3.4.1; plan Decision 4a).
func setupTestEventBus(
	t *testing.T,
	ctx context.Context,
	database *db.Database,
	contactService *service.ContactService,
) *events.Bus {
	t.Helper()

	eventRepo := repository.NewEventRepository(database.Queries)
	telegramMessageRepo := repository.NewTelegramMessageRepository(database.Queries)

	cfg := config.TestConfig()
	if cfg.River.WorkerConcurrency <= 0 {
		cfg.River.WorkerConcurrency = 4
	}

	// River client + worker registration has a chicken-and-egg shape:
	//  - river.NewClient needs the workers bundle.
	//  - The real InteractionRecorderWorker needs bus + recorder, which
	//    need the client to construct.
	//
	// Resolve by deferring the recorder inside the worker shim. A shared
	// pointer is filled after both bus and recorder exist — before the
	// client Start runs any jobs.
	workers := river.NewWorkers()
	shim := &deferredRecorderWorker{}
	river.AddWorker(workers, shim)

	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers:  workers,
		TestOnly: true, // disable staggered maintenance startup so test-scoped clients start processing jobs immediately
	})
	require.NoError(t, err)

	bus := events.NewBus(database.Pool, client, eventRepo)
	recorder := consumer.NewInteractionRecorder(contactService, telegramMessageRepo, bus)
	// Fill the shim's real worker now that bus + recorder exist.
	shim.real = consumer.NewInteractionRecorderWorker(bus, database.Pool, recorder)

	// IMPORTANT: pass the OUTER ctx (not a timeout-derived one) to Start.
	// River derives its fetch/work context from whatever Start() receives;
	// if we pass a context that gets cancelled on this helper's return,
	// the river client silently stops fetching jobs and tests hang.
	require.NoError(t, client.Start(ctx))

	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = client.Stop(stopCtx)
	})

	return bus
}

// defaultInteractionWaitTimeout is the budget we give the async consumer
// to pick up an enqueued job and commit the interaction row. Generous to
// avoid flakes under CI load; integration tests that time out at this
// duration indicate a real wiring regression.
const defaultInteractionWaitTimeout = 30 * time.Second

// deferredRecorderWorker is a thin shim that lets us register a
// River worker on a workers bundle BEFORE the real worker exists —
// necessary because river.NewClient consumes the workers bundle at
// construction time but the real InteractionRecorderWorker needs a bus,
// and the bus needs the client. Work() delegates to whichever real
// worker has been assigned by the time jobs run.
type deferredRecorderWorker struct {
	river.WorkerDefaults[consumerjobs.InteractionRecorderJobArgs]
	real *consumer.InteractionRecorderWorker
}

func (w *deferredRecorderWorker) Work(ctx context.Context, j *river.Job[consumerjobs.InteractionRecorderJobArgs]) error {
	if w.real == nil {
		return fmt.Errorf("deferredRecorderWorker invoked before real worker assignment")
	}
	return w.real.Work(ctx, j)
}

func (w *deferredRecorderWorker) Timeout(j *river.Job[consumerjobs.InteractionRecorderJobArgs]) time.Duration {
	if w.real == nil {
		return 30 * time.Second
	}
	return w.real.Timeout(j)
}

// waitForInteractionBySourceRef polls InteractionRepository.FindBySourceRef
// until the row appears or the timeout elapses. Used after the publisher
// runs so the async InteractionRecorder consumer has time to commit.
func waitForInteractionBySourceRef(
	t *testing.T,
	ctx context.Context,
	repo *repository.InteractionRepository,
	contactID uuid.UUID,
	source string,
	sourceRef string,
	timeout time.Duration,
) *repository.Interaction {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		got, err := repo.FindBySourceRef(ctx, contactID, source, sourceRef)
		if err == nil {
			return got
		}
		lastErr = err
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for interaction (contact=%s source=%s source_ref=%s): %v",
		contactID, source, sourceRef, lastErr)
	return nil
}

// waitForTelegramInteractionCount polls the DB for `want` telegram
// interactions attached to the contact (or timeout). Used by aggregation
// tests after publishing events.
func waitForTelegramInteractionCount(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	contactID uuid.UUID,
	want int,
	timeout time.Duration,
) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last int
	for time.Now().Before(deadline) {
		err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM interaction
			 WHERE contact_id = $1 AND source = 'telegram' AND deleted_at IS NULL`,
			contactID,
		).Scan(&last)
		require.NoError(t, err)
		if last >= want {
			return last
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d telegram interactions (got %d) for contact=%s",
		want, last, contactID)
	return last
}

// waitForInteractionCountExact polls ListContactInteractions until
// exactly `want` rows are visible (or timeout). Returns the row slice
// so callers can assert on direction/source/etc. post-wait.
func waitForInteractionCountExact(
	t *testing.T,
	ctx context.Context,
	repo *repository.InteractionRepository,
	contactID uuid.UUID,
	want int,
	timeout time.Duration,
) []repository.Interaction {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []repository.Interaction
	for time.Now().Before(deadline) {
		rows, err := repo.ListContactInteractions(ctx, contactID, 100, 0)
		require.NoError(t, err)
		last = rows
		if len(rows) == want {
			return rows
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for exactly %d interactions for contact=%s (got %d)",
		want, contactID, len(last))
	return last
}

// waitForInteractionDirection polls until the contact has an interaction
// with the given direction (useful for reply-bridge tests where
// outbound → mutual promotion is what we're waiting on).
func waitForInteractionDirection(
	t *testing.T,
	ctx context.Context,
	repo *repository.InteractionRepository,
	contactID uuid.UUID,
	direction string,
	timeout time.Duration,
) []repository.Interaction {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []repository.Interaction
	for time.Now().Before(deadline) {
		rows, err := repo.ListContactInteractions(ctx, contactID, 100, 0)
		require.NoError(t, err)
		last = rows
		for _, r := range rows {
			if r.Direction == direction {
				return rows
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for direction=%s interaction for contact=%s (got %d rows)",
		direction, contactID, len(last))
	return last
}

// waitForTelegramMessagesProcessed polls until `want` telegram messages
// for the given peer are marked processed_at != NULL. In cutover the
// consumer marks these in the interaction-insert tx (plan Decision 10).
func waitForTelegramMessagesProcessed(
	t *testing.T,
	ctx context.Context,
	pool *pgxpool.Pool,
	peerUserID int64,
	contactID uuid.UUID,
	want int,
	timeout time.Duration,
) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last int
	for time.Now().Before(deadline) {
		err := pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM telegram_message
			 WHERE peer_user_id = $1 AND matched_contact_id = $2
			   AND processed_at IS NOT NULL AND deleted_at IS NULL`,
			peerUserID, contactID,
		).Scan(&last)
		require.NoError(t, err)
		if last >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d processed messages (got %d) for peer=%d contact=%s",
		want, last, peerUserID, contactID)
}
