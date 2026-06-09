package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/scheduler"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"
)

func TestPairingTokenJanitor_DeletesExpired(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	database, _ := newAPISharedTestDB(t, ctx)

	tokenRepo := repository.NewMacHostPairingTokenRepository(database.Queries)

	// Clean any pre-existing rows so the test asserts on a known state.
	_, err := database.Queries.DeleteAllPairingTokens(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		// best-effort cleanup
		_, _ = database.Queries.DeleteAllPairingTokens(ctx)
	})

	// Seed 3 expired + 2 active tokens. Use unique hashes so the unique
	// constraint isn't violated.
	now := accelerated.GetCurrentTime()
	seedToken := func(suffix string, expires time.Time) {
		hashBytes := sha256.Sum256([]byte("janitor-test-" + suffix))
		_, err := database.Queries.SeedPairingToken(ctx, db.SeedPairingTokenParams{
			TokenHash: hex.EncodeToString(hashBytes[:]),
			ExpiresAt: pgtype.Timestamptz{Time: expires, Valid: true},
		})
		require.NoError(t, err)
	}
	for i := 0; i < 3; i++ {
		seedToken(fmt.Sprintf("exp-%d", i), now.Add(-1*time.Hour))
	}
	for i := 0; i < 2; i++ {
		seedToken(fmt.Sprintf("act-%d", i), now.Add(1*time.Hour))
	}

	// Run the worker directly.
	worker := scheduler.NewPairingTokenJanitorWorker(tokenRepo)
	err = worker.Work(ctx, &river.Job[scheduler.PairingTokenJanitorArgs]{Args: scheduler.PairingTokenJanitorArgs{}})
	require.NoError(t, err)

	// Expect: 2 rows remain (the active ones). Count via DeleteAll.
	deleted, err := database.Queries.DeleteAllPairingTokens(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), deleted, "after janitor, exactly the 2 active tokens should remain")
}
