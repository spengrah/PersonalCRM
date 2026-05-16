package api

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

// ----------------------------------------------------------------------------
// IngestBatch tx-internal host-liveness FOR UPDATE coverage
//
// Exercises the SELECT ... FOR UPDATE recheck on mac_host that runs
// at the top of every IngestBatch tx. The recheck closes the race
// window between the auth-middleware host lookup and the batch's
// commit. Auth-middleware separately blocks revoked-host requests
// (covered by mac_host_auth_test.go); this file isolates the in-batch
// recheck so a regression that drops the FOR UPDATE acquire surfaces
// even if the auth-middleware path stays correct.
// ----------------------------------------------------------------------------

// stubFailingHostLiveness always returns db.ErrNotFound, simulating
// "host revoked between auth and the batch's lock acquire."
type stubFailingHostLiveness struct {
	calls int
}

func (s *stubFailingHostLiveness) GetActiveHostByIDForUpdateTx(
	_ context.Context, _ pgx.Tx, _ uuid.UUID,
) (*repository.MacHost, error) {
	s.calls++
	return nil, db.ErrNotFound
}

func TestIngest_HostRevokedMidTx_AbortsBatch(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	eventRepo := repository.NewEventRepository(database.Queries)
	eventBus := events.NewBus(database.Pool, nil, eventRepo)
	externalRepo := repository.NewExternalContactRepository(database.Queries)

	// Inject the failing liveness checker. The batch must abort with
	// ErrHostRevokedDuringBatch BEFORE any envelope dispatches.
	liveness := &stubFailingHostLiveness{}
	svc := service.NewIngestService(
		database, eventBus,
		nil, // identity unused — we never reach the per-event handler
		nil, // messages unused
		nil, // river unused
		externalRepo,
		liveness,
	)

	// Build a structurally-valid envelope so the batch precondition
	// checks pass and the loop is reached. The FOR UPDATE check must
	// fire before any per-event dispatch.
	hostID := uuid.New()
	entityID := "host-revoked-test-" + uuid.NewString()[:8]
	payload, err := events.Marshal(events.KindExternalContactUpserted, events.ExternalContactUpsertedPayload{
		Version:  1,
		HostID:   hostID,
		Source:   "icloud_contacts",
		EntityID: entityID,
	})
	require.NoError(t, err)
	hashHex, err := service.ComputeContentHash(payload)
	require.NoError(t, err)
	env := &events.Envelope{
		Source:     "icloud_contacts",
		SourceID:   entityID + "@" + hashHex,
		Kind:       events.KindExternalContactUpserted,
		Payload:    payload,
		ObservedAt: accelerated.GetCurrentTime(),
	}

	accepted, duplicate, rejections, err := svc.IngestBatch(
		ctx, []*events.Envelope{env}, []int{0}, &hostID,
	)
	require.Error(t, err, "batch must abort when liveness check reports revoked host")
	require.True(t, errors.Is(err, service.ErrHostRevokedDuringBatch),
		"abort error must be ErrHostRevokedDuringBatch (got %v)", err)
	require.Equal(t, 0, accepted)
	require.Equal(t, 0, duplicate)
	require.Empty(t, rejections)
	require.Equal(t, 1, liveness.calls, "FOR UPDATE check must fire exactly once per batch")

	// No event-log row may have committed. Cleanup-safe query: source
	// matches our test's synthetic prefix.
	cleanCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	logged, err := eventRepo.FindEventBySource(cleanCtx, "icloud_contacts", env.SourceID)
	if err == nil {
		require.Nil(t, logged, "no event-log row may commit when batch aborts")
	} else {
		require.True(t, errors.Is(err, db.ErrNotFound),
			"event-log row absence expected; got error %v", err)
	}
}

// TestIngest_HostLivenessNil_SkipsCheck documents the test-fixture
// path: NewIngestService(... nil) leaves the recheck disabled so
// existing test wiring without a real mac_host repo continues to work.
// Production wires a real repo so the check always runs.
func TestIngest_HostLivenessNil_SkipsCheck(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}
	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	defer database.Close()

	eventRepo := repository.NewEventRepository(database.Queries)
	eventBus := events.NewBus(database.Pool, nil, eventRepo)

	// nil hostLiveness — recheck is skipped.
	svc := service.NewIngestService(database, eventBus, nil, nil, nil, nil, nil)

	// An empty batch passes the precondition layer immediately and
	// never opens a tx. The test confirms wiring without the recheck
	// does not panic.
	accepted, duplicate, rejections, err := svc.IngestBatch(ctx, nil, nil, nil)
	require.NoError(t, err)
	require.Equal(t, 0, accepted)
	require.Equal(t, 0, duplicate)
	require.Empty(t, rejections)
}
