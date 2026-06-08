package tests

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// phoneCallTestEnv bundles the per-test repos, generator, and host ID. The
// InteractionRepository is included for tests that need to create
// interactions for FK references. gen scopes every string identifier the
// sub-tests construct to a unique per-test namespace, and seeds contacts via
// seedMigrationContact.
type phoneCallTestEnv struct {
	ctx             context.Context
	database        *db.Database
	gen             *factory.Generator
	repo            *repository.PhoneCallRepository
	interactionRepo *repository.InteractionRepository
	hostID          uuid.UUID
}

// setupPhoneCallTest provisions a per-test mac_host row + repo bundle via the
// lightweight synthetic path (a factory.Generator, no replay harness / River
// client — so the file stays in the fast PR gate). The mac_host is seeded
// pre-revoked and namespace-prefixed so the singleton index never collides with
// a parallel tests/api package, mirroring setupMessagesMessageTest. Cleanup
// hard-deletes scoped by mac_host_id (upsert does not clear deleted_at on
// conflict, so a soft delete would resurrect rows across runs).
func setupPhoneCallTest(t *testing.T) (phoneCallTestEnv, func()) {
	t.Helper()
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)

	gen, _ := migrationGenerator(t)

	repo := repository.NewPhoneCallRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)

	macHostRepo := repository.NewMacHostRepository(database.Queries)
	prefix := gen.Prefix()
	host, err := macHostRepo.SeedRevokedHostForTest(ctx,
		prefix+"pc-host", "test-version", 1, prefix+"pc-hash")
	require.NoError(t, err)

	cleanup := func() {
		_ = repo.HardDeleteByMacHost(ctx, host.ID)
		database.Close()
	}
	return phoneCallTestEnv{
		ctx:             ctx,
		database:        database,
		gen:             gen,
		repo:            repo,
		interactionRepo: interactionRepo,
		hostID:          host.ID,
	}, cleanup
}

func TestPhoneCallRepository_UpsertAndGet(t *testing.T) {
	env, cleanup := setupPhoneCallTest(t)
	defer cleanup()

	contact, contactCleanup := seedMigrationContact(env.ctx, t, env.database, env.gen)
	defer contactCleanup()

	prefix := env.gen.Prefix()
	startedAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	answered := true
	uniqueID := prefix + "call"

	call, err := env.repo.UpsertCall(env.ctx, repository.UpsertPhoneCallParams{
		CallUniqueID:     uniqueID,
		PeerHandle:       "+15551234567",
		PeerNormalized:   "+15551234567",
		Service:          repository.PhoneCallServiceVoice,
		Direction:        repository.PhoneCallDirectionInbound,
		Answered:         &answered,
		HasVoicemail:     false,
		DurationSeconds:  42,
		StartedAt:        startedAt,
		MatchedContactID: &contact.ID,
		MacHostID:        &env.hostID,
	})
	require.NoError(t, err)
	require.NotNil(t, call)
	assert.Equal(t, uniqueID, call.CallUniqueID)
	assert.Equal(t, repository.PhoneCallServiceVoice, call.Service)
	assert.Equal(t, repository.PhoneCallDirectionInbound, call.Direction)
	require.NotNil(t, call.Answered)
	assert.True(t, *call.Answered)
	assert.Equal(t, int32(42), call.DurationSeconds)
	require.NotNil(t, call.MatchedContactID)
	assert.Equal(t, contact.ID, *call.MatchedContactID)

	got, err := env.repo.GetCallByUniqueID(env.ctx, uniqueID)
	require.NoError(t, err)
	assert.Equal(t, call.ID, got.ID)
	assert.Equal(t, call.CallUniqueID, got.CallUniqueID)

	// Second upsert with same unique ID returns the existing row (dedup).
	call2, err := env.repo.UpsertCall(env.ctx, repository.UpsertPhoneCallParams{
		CallUniqueID:    uniqueID,
		PeerHandle:      "+15551234567",
		PeerNormalized:  "+15551234567",
		Service:         repository.PhoneCallServiceVoice,
		Direction:       repository.PhoneCallDirectionInbound,
		Answered:        &answered,
		DurationSeconds: 42,
		StartedAt:       startedAt,
		MacHostID:       &env.hostID,
	})
	require.NoError(t, err)
	assert.Equal(t, call.ID, call2.ID, "dedup must return same row id")
}

func TestPhoneCallRepository_GetByUniqueID_NotFound(t *testing.T) {
	env, cleanup := setupPhoneCallTest(t)
	defer cleanup()

	_, err := env.repo.GetCallByUniqueID(env.ctx, env.gen.Prefix()+"nonexistent-call")
	require.Error(t, err)
	assert.True(t, errors.Is(err, db.ErrNotFound))
}

func TestPhoneCallRepository_MarkProcessed_WithInteraction(t *testing.T) {
	env, cleanup := setupPhoneCallTest(t)
	defer cleanup()

	contact, contactCleanup := seedMigrationContact(env.ctx, t, env.database, env.gen)
	defer contactCleanup()

	prefix := env.gen.Prefix()
	ref := prefix + "call-mp"
	defer func() {
		_ = env.interactionRepo.HardDeleteInteractionsBySourceRefPrefix(env.ctx, "phone_calls", ref+"%")
	}()
	interaction, err := env.interactionRepo.CreateInteraction(env.ctx, repository.CreateInteractionRequest{
		ContactID:  contact.ID,
		Source:     "phone_calls",
		SourceRef:  &ref,
		OccurredAt: accelerated.GetCurrentTime().Truncate(time.Microsecond),
		Direction:  repository.InteractionDirectionInbound,
	})
	require.NoError(t, err)

	answered := true
	call, err := env.repo.UpsertCall(env.ctx, repository.UpsertPhoneCallParams{
		CallUniqueID:     ref,
		PeerHandle:       "+15551234567",
		PeerNormalized:   "+15551234567",
		Service:          repository.PhoneCallServiceVoice,
		Direction:        repository.PhoneCallDirectionInbound,
		Answered:         &answered,
		DurationSeconds:  10,
		StartedAt:        accelerated.GetCurrentTime().Truncate(time.Microsecond),
		MatchedContactID: &contact.ID,
		MacHostID:        &env.hostID,
	})
	require.NoError(t, err)

	err = env.repo.MarkProcessed(env.ctx, repository.MarkProcessedParams{
		ID:            call.ID,
		InteractionID: &interaction.ID,
	})
	require.NoError(t, err)

	got, err := env.repo.GetCallByUniqueID(env.ctx, call.CallUniqueID)
	require.NoError(t, err)
	require.NotNil(t, got.ProcessedAt)
	require.NotNil(t, got.InteractionID)
	assert.Equal(t, interaction.ID, *got.InteractionID)
}

func TestPhoneCallRepository_MarkProcessed_NoInteraction(t *testing.T) {
	env, cleanup := setupPhoneCallTest(t)
	defer cleanup()

	answered := false
	call, err := env.repo.UpsertCall(env.ctx, repository.UpsertPhoneCallParams{
		CallUniqueID:    env.gen.Prefix() + "call-noix",
		PeerHandle:      "+15551234567",
		PeerNormalized:  "+15551234567",
		Service:         repository.PhoneCallServiceVoice,
		Direction:       repository.PhoneCallDirectionInbound,
		Answered:        &answered,
		HasVoicemail:    false,
		DurationSeconds: 0,
		StartedAt:       accelerated.GetCurrentTime().Truncate(time.Microsecond),
		MacHostID:       &env.hostID,
	})
	require.NoError(t, err)

	// Missed-no-voicemail: mark processed with nil interaction.
	err = env.repo.MarkProcessed(env.ctx, repository.MarkProcessedParams{
		ID:            call.ID,
		InteractionID: nil,
	})
	require.NoError(t, err)

	got, err := env.repo.GetCallByUniqueID(env.ctx, call.CallUniqueID)
	require.NoError(t, err)
	require.NotNil(t, got.ProcessedAt)
	assert.Nil(t, got.InteractionID, "missed-no-voicemail row keeps interaction_id NULL")
}

func TestPhoneCallRepository_HardDeleteByMacHost(t *testing.T) {
	env, cleanup := setupPhoneCallTest(t)
	defer cleanup()

	uniqueID := env.gen.Prefix() + "call-del"
	answered := true
	_, err := env.repo.UpsertCall(env.ctx, repository.UpsertPhoneCallParams{
		CallUniqueID:    uniqueID,
		PeerHandle:      "+15551234567",
		PeerNormalized:  "+15551234567",
		Service:         repository.PhoneCallServiceVoice,
		Direction:       repository.PhoneCallDirectionInbound,
		Answered:        &answered,
		DurationSeconds: 1,
		StartedAt:       accelerated.GetCurrentTime().Truncate(time.Microsecond),
		MacHostID:       &env.hostID,
	})
	require.NoError(t, err)

	err = env.repo.HardDeleteByMacHost(env.ctx, env.hostID)
	require.NoError(t, err)

	_, err = env.repo.GetCallByUniqueID(env.ctx, uniqueID)
	assert.True(t, errors.Is(err, db.ErrNotFound))
}
