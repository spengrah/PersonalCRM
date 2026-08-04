//go:build integration_testdb

// The feature-on guard for the WhatsApp integration.
//
// It lives in a tagged file rather than beside the other wiring tests because
// wireWhatsApp opens the device store and therefore needs a database, and
// TestMain plus testdb.NewEphemeralClone live in this build tag (wire_golden_test.go).
package main

import (
	"context"
	"os"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/events"
	"personal-crm/backend/internal/logger"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/testdb"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wireWhatsAppHarness is a real database, a real River client and a registrar
// with its periodic bundle attached.
//
// The attached bundle is not optional: registerWhatsAppHistoryDrain calls
// reg.addPeriodic, which PANICS when the bundle is nil, so a bare
// newRiverRegistrar would fail for a reason unrelated to what is under test.
// The disabled case needs it too, since its assertion is that nothing was added.
type wireWhatsAppHarness struct {
	cfg      *config.Config
	database *db.Database
	reg      *riverRegistrar
}

func newWireWhatsAppHarness(t *testing.T, mutate func(*config.Config)) wireWhatsAppHarness {
	t.Helper()
	ctx := context.Background()

	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)

	cfg := config.TestConfig()
	cfg.Database.URL = cloneURL
	cfg.Database.MigrationsPath = migrationsPathForTest()
	mutate(cfg)

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(func() { database.Close() })

	riverWorkers := river.NewWorkers()
	reg := newRiverRegistrar(riverWorkers)
	riverClient, err := river.NewClient(riverpgxv5.New(database.Pool), &river.Config{
		JobTimeout: cfg.River.JobTimeout,
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.River.WorkerConcurrency},
		},
		Workers:      riverWorkers,
		ErrorHandler: events.NewRiverErrorHandler(logger.Get()),
		Logger:       logger.NewSlogLogger(logger.Get()),
	})
	require.NoError(t, err)
	reg.periodic = riverClient.PeriodicJobs()

	return wireWhatsAppHarness{cfg: cfg, database: database, reg: reg}
}

// TestWireWhatsApp_TurnsTheFeatureOn is THE feature-on guard.
//
// Everything else in this PR exists to make this transition safe, and until the
// composition block was extracted into wireWhatsApp no test could read it: the
// other wiring tests build their own whatsappPrereqs literal, and the golden
// chain omits the WhatsApp stack entirely. So a mutant that set DrainReady back
// to false would have landed with everything green.
func TestWireWhatsApp_TurnsTheFeatureOn(t *testing.T) {
	t.Parallel()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	h := newWireWhatsAppHarness(t, func(c *config.Config) {
		c.Features.EnableExternalSync = true
		c.Features.EnableWhatsAppSync = true
	})

	// The real ingestor, built the way run() builds it — so the gate the
	// drainer is handed is the same instance SetIngestor binds.
	ingestor := buildWhatsAppIngestor(
		h.cfg,
		h.database,
		repository.NewCommsMessageRepository(h.database.Queries),
		service.NewIdentityService(repository.NewIdentityRepository(h.database.Queries)),
		repository.NewExternalContactRepository(h.database.Queries),
		nil,
	)

	stk := wireWhatsApp(context.Background(), h.cfg, h.database, h.reg, ingestor)
	require.NotNil(t, stk.Manager, "the device store opens against a real database")
	t.Cleanup(stk.Manager.Stop)

	ready, missing := stk.Manager.Ready()
	assert.True(t, ready, "every readiness prerequisite is satisfied, so the client may connect")
	assert.Empty(t, missing)
	assert.NotEqual(t, "not_ready", stk.Manager.Status().State,
		"the gate passed, so Start proceeded past it")

	assert.Contains(t, h.reg.workerKinds, "whatsapp_history_drain",
		"DrainReady may only be true because a worker really was registered")
	assert.Contains(t, h.reg.periodicKinds, "whatsapp_history_drain",
		"a registered worker with no periodic would never run")
}

// TestWireWhatsApp_RegistersNothingWhenDisabled is the off half: a
// WhatsApp-disabled deployment gains no manager, no worker and no periodic.
func TestWireWhatsApp_RegistersNothingWhenDisabled(t *testing.T) {
	t.Parallel()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	h := newWireWhatsAppHarness(t, func(c *config.Config) {})

	stk := wireWhatsApp(context.Background(), h.cfg, h.database, h.reg, nil)

	assert.Nil(t, stk.Manager)
	assert.Nil(t, stk.Handler)
	assert.NotContains(t, h.reg.workerKinds, "whatsapp_history_drain")
	assert.NotContains(t, h.reg.periodicKinds, "whatsapp_history_drain")
}
