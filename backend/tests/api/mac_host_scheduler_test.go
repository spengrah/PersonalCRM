package api

import (
	"context"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	syncpkg "personal-crm/backend/internal/sync"

	"github.com/stretchr/testify/require"
)

// pushTestProvider is a no-op syncpkg.SyncProvider used to register a
// push-strategy source so SyncService.ListDueAccounts has something to
// skip.
type pushTestProvider struct {
	cfg syncpkg.SourceConfig
}

func (p *pushTestProvider) Config() syncpkg.SourceConfig { return p.cfg }
func (p *pushTestProvider) Sync(_ context.Context, _ *repository.SyncState, _ []repository.Contact) (*syncpkg.SyncResult, error) {
	return &syncpkg.SyncResult{}, nil
}
func (p *pushTestProvider) ValidateCredentials(_ context.Context, _ *string) error {
	return nil
}

// contactDrivenTestProvider is the control: a normal poll-driven provider
// that should be returned by ListDueAccounts when its row is due.
type contactDrivenTestProvider struct {
	cfg syncpkg.SourceConfig
}

func (p *contactDrivenTestProvider) Config() syncpkg.SourceConfig { return p.cfg }
func (p *contactDrivenTestProvider) Sync(_ context.Context, _ *repository.SyncState, _ []repository.Contact) (*syncpkg.SyncResult, error) {
	return &syncpkg.SyncResult{}, nil
}
func (p *contactDrivenTestProvider) ValidateCredentials(_ context.Context, _ *string) error {
	return nil
}

func TestSyncService_ListDueAccounts_ExcludesPushStrategy(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	// Build a real SyncRepository against the DB.
	syncRepo := repository.NewSyncRepositoryWithPool(database.Queries, database.Pool)
	contactRepo := repository.NewContactRepository(database.Queries)

	// Build a registry with one push provider + one contact_driven control.
	pushSource := "test-mac-push-" + nowSuffix()
	pollSource := "test-poll-" + nowSuffix()
	registry := syncpkg.NewProviderRegistry()
	registry.Register(&pushTestProvider{cfg: syncpkg.SourceConfig{
		Name: pushSource, Strategy: repository.SyncStrategyPush,
	}})
	registry.Register(&contactDrivenTestProvider{cfg: syncpkg.SourceConfig{
		Name: pollSource, Strategy: repository.SyncStrategyContactDriven,
	}})

	// Seed one due row per source.
	nextSync := accelerated.GetCurrentTime().Add(-1 * time.Minute) // due (in the past)
	pushAccountID := "host-x"
	_, err = database.Queries.SeedExternalSyncState(ctx, db.SeedExternalSyncStateParams{
		Source:     pushSource,
		AccountID:  &pushAccountID,
		Enabled:    true,
		Status:     "idle",
		Strategy:   "push",
		NextSyncAt: &nextSync,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		// Clean up seeded rows so the shared test DB stays small.
		_ = database.Queries.DeleteSyncState
		// Hard-delete by id is not exposed; use the test-only delete-all
		// for these two sources via direct re-read + DeleteSyncState.
		states, _ := database.Queries.ListSyncStates(ctx)
		for _, s := range states {
			if s.Source == pushSource || s.Source == pollSource {
				_ = database.Queries.DeleteSyncState(ctx, s.ID)
			}
		}
	})

	_, err = database.Queries.SeedExternalSyncState(ctx, db.SeedExternalSyncStateParams{
		Source:     pollSource,
		AccountID:  nil,
		Enabled:    true,
		Status:     "idle",
		Strategy:   "contact_driven",
		NextSyncAt: &nextSync,
	})
	require.NoError(t, err)

	svc := service.NewSyncService(syncRepo, contactRepo, registry)
	accounts, err := svc.ListDueAccounts(ctx)
	require.NoError(t, err)

	// Expect: pollSource present, pushSource absent.
	var sawPush, sawPoll bool
	for _, acct := range accounts {
		if acct.Source == pushSource {
			sawPush = true
		}
		if acct.Source == pollSource {
			sawPoll = true
		}
	}
	require.False(t, sawPush, "push-strategy provider must be excluded")
	require.True(t, sawPoll, "contact_driven control must be included")
}

func nowSuffix() string {
	return accelerated.GetCurrentTime().UTC().Format("20060102150405.000000000")
}
