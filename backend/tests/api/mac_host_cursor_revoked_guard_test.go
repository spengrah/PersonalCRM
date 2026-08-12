package api

import (
	"context"
	"errors"
	"testing"

	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestMacHost_CommitCursor_RevokedHostRejected discriminates the §5.9
// revoked-host cursor guard (sync.go's Stage A ApiKeyRevokedAt check in
// CommitMacHostCursor, the post-flip `epochRow.ApiKeyRevokedAt != nil`
// condition). GetCursorEpochForCommitTx has no other caller and no other
// test that drives a revoked host this far, so an inverted polarity there
// (`== nil`) would treat a revoked API key as live and pass every other
// gate silently — this test is the sole discriminator.
//
// Asserting only the rejection is not a sufficient guard test: a commit path
// that rejects everything would also pass a rejection-only assertion. The
// live host's commit succeeding is the other half of the pair.
func TestMacHost_CommitCursor_RevokedHostRejected(t *testing.T) {

	env := setupMacHostEnv(t)
	ctx := context.Background()

	liveName := "cursor-guard-live-" + uuid.NewString()[:8]
	live, err := env.hostRepo.SeedHostForTest(ctx, liveName, "0.1.0", 1, "$2a$04$live", nil, nil)
	require.NoError(t, err)

	revokedName := "cursor-guard-revoked-" + uuid.NewString()[:8]
	revoked, err := env.hostRepo.SeedRevokedHostForTest(ctx, revokedName, "0.1.0", 1, "$2a$04$rvkd")
	require.NoError(t, err)

	// Live host: the commit must succeed.
	err = env.macService.CommitCursor(ctx, repository.CommitMacHostCursorParams{
		HostID:       live.ID,
		Source:       "messages",
		BaseCursor:   "",
		NewCursor:    "live-cursor-1",
		ClaimedEpoch: live.CursorEpoch,
	})
	require.NoError(t, err, "a live host's cursor commit must succeed")

	// Revoked host: the same commit shape, at its own current epoch, must be
	// rejected as db.ErrNotFound — the guard fires before the epoch or base
	// checks, so a revoked host's own correct epoch still must not commit.
	err = env.macService.CommitCursor(ctx, repository.CommitMacHostCursorParams{
		HostID:       revoked.ID,
		Source:       "messages",
		BaseCursor:   "",
		NewCursor:    "revoked-cursor-1",
		ClaimedEpoch: revoked.CursorEpoch,
	})
	require.Error(t, err, "a revoked host's cursor commit must be rejected")
	require.True(t, errors.Is(err, db.ErrNotFound), "revoked-host commit must surface db.ErrNotFound, got %v", err)
}
