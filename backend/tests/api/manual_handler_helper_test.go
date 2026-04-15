package api

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/require"
)

// buildManualHandlerForTest wires the PR 6 cutover manual-interaction
// pipeline (bus → consumer → manual handler) against a real DB + river
// client. TestOnly=true means the dispatcher never runs jobs; ManualInteractionHandler
// invokes HandleEvent inline.
//
// Returns the manual handler, the contact service (for callers that still
// exercise the service directly), and a cleanup func.
func buildManualHandlerForTest(t *testing.T, ctx context.Context, database *db.Database, cfg *config.Config) (*service.ManualInteractionHandler, *service.ContactService, func()) {
	t.Helper()

	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	telegramMessageRepo := repository.NewTelegramMessageRepository(database.Queries)
	eventRepo := repository.NewEventRepository(database.Queries)

	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, contactTaskRepo)

	workers := river.NewWorkers()
	// Pre-register a placeholder interaction_recorder worker. TestOnly
	// ensures the dispatcher never runs — the manual handler invokes
	// HandleEvent inline per plan Decision 1.
	river.AddWorker(workers, &manualTestNoopWorker{})

	client, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 1},
		},
		Workers:  workers,
		TestOnly: true,
	})
	require.NoError(t, err)

	bus := events.NewBus(database.Pool, client, eventRepo)
	recorder := consumer.NewInteractionRecorder(contactService, telegramMessageRepo, bus)
	manualHandler := service.NewManualInteractionHandler(database.Pool, bus, recorder)

	startCtx, startCancel := context.WithTimeout(ctx, 10*time.Second)
	defer startCancel()
	require.NoError(t, client.Start(startCtx))

	cleanup := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer stopCancel()
		_ = client.Stop(stopCtx)
	}

	return manualHandler, contactService, cleanup
}

// manualTestNoopWorker is a placeholder registration so river accepts
// Insert calls for interaction_recorder-kind events without actually
// running a worker (TestOnly=true).
type manualTestNoopWorker struct {
	river.WorkerDefaults[manualTestJobArgs]
}

type manualTestJobArgs struct{}

func (manualTestJobArgs) Kind() string { return "interaction_recorder" }

func (*manualTestNoopWorker) Work(_ context.Context, _ *river.Job[manualTestJobArgs]) error {
	return nil
}
