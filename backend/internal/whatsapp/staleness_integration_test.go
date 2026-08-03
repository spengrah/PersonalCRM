//go:build integration_testdb

// End-to-end proof that a terminal WhatsApp disconnect reaches the sync-staleness
// watchdog.
//
// It lives in this package, not in backend/tests, because the seeding has to run
// through the PRODUCTION writer — the manager's own terminal event handler,
// which is unexported. A fixture row hand-built to satisfy the watchdog's
// ordinary error predicate would prove nothing: the whole finding was that the
// row this path actually writes (one error, freshly created, not
// scheduler-enabled) can never satisfy that predicate.
package whatsapp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/testdb"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mau.fi/whatsmeow/types/events"
)

// TestMain is declared in this build-tagged file only: the package's other
// tests need no database, and under `make test-unit` (no tag) there is no
// TestMain at all.
func TestMain(m *testing.M) {
	os.Exit(testdb.SetupPackage(m, testdb.WithMigrationsPath(migrationsPathForWhatsAppTest())))
}

// migrationsPathForWhatsAppTest resolves the migrations dir relative to this
// file (internal/whatsapp -> ../../migrations), honoring an absolute
// MIGRATIONS_PATH override.
func migrationsPathForWhatsAppTest() string {
	if path := os.Getenv("MIGRATIONS_PATH"); path != "" && filepath.IsAbs(path) {
		return path
	}
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..", "migrations")
}

func TestWhatsAppManager_TerminalDisconnectOpensAStalenessBreach(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}

	ctx := context.Background()
	cloneURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)

	cfg := config.TestConfig()
	cfg.Database.URL = cloneURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	waLog := NewWALogger("whatsapp-test")
	container, err := NewDeviceContainer(ctx, database.Pool, waLog)
	require.NoError(t, err)

	syncRepo := repository.NewSyncRepository(database.Queries)
	waRepo := repository.NewWhatsAppRepository(database.Queries)

	m := NewManager(container, waLog, &cfg.WhatsApp, syncRepo, waRepo)
	m.SetIngestor(stalenessTestIngestor{})
	m.SetHistoryRecorder(NewHistoryRecorder(waRepo))
	m.SetHistoryDrainReady()
	require.NoError(t, m.Start(ctx))
	t.Cleanup(m.Stop)

	// The production terminal path: one error write, then it stops.
	require.True(t, m.handleEvent(&events.LoggedOut{}))

	state, err := syncRepo.GetSyncStateBySource(ctx, repository.InteractionSourceWhatsApp, nil)
	require.NoError(t, err)
	require.Equal(t, repository.SyncStatusError, state.Status)
	reason, ok := repository.SyncStateTerminalReason(*state)
	require.True(t, ok, "the terminal reason must be durably recorded")
	require.Equal(t, ReasonLoggedOut, reason)
	require.False(t, state.Enabled, "a manager-driven row stays out of the scheduler's queue")
	require.Less(t, int(state.ErrorCount), cfg.Staleness.ErrorMinCount,
		"the row the production path writes sits BELOW the ordinary error floor — that is the finding")

	// The real watchdog, with external sync OFF (the harder case).
	staleness := service.NewStalenessService(
		cfg.Staleness,
		false,
		syncRepo,
		repository.NewMacHostRepository(database.Queries),
		repository.NewStalenessRepository(database.Queries),
	)
	require.NoError(t, staleness.RunChecks(ctx))

	breaches, err := staleness.ListActiveBreaches(ctx)
	require.NoError(t, err)

	var found *repository.StalenessBreach
	for i := range breaches {
		if breaches[i].Source == repository.InteractionSourceWhatsApp && breaches[i].BreachType == repository.BreachTypeSyncError {
			found = &breaches[i]
			break
		}
	}
	require.NotNil(t, found, "a terminal WhatsApp disconnect must surface as an open sync_error breach")
	assert.Contains(t, found.Details, ReasonLoggedOut, "the breach names the reason")
}

type stalenessTestIngestor struct{}

func (stalenessTestIngestor) IngestMessage(context.Context, IngestedMessage) error { return nil }
