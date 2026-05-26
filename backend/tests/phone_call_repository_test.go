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

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// phoneCallTestEnv bundles the per-test repos and host ID. The
// InteractionRepository is included for tests that need to create
// interactions for FK references.
type phoneCallTestEnv struct {
	ctx             context.Context
	repo            *repository.PhoneCallRepository
	contactRepo     *repository.ContactRepository
	interactionRepo *repository.InteractionRepository
	hostID          uuid.UUID
}

// setupPhoneCallTest provisions a per-test mac_host row + repo bundle.
// Cleanup hard-deletes scoped by mac_host_id. Matches the pattern from
// setupMessagesMessageTest — same parallel-runner singleton-slot
// constraint applies.
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

	repo := repository.NewPhoneCallRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)

	macHostRepo := repository.NewMacHostRepository(database.Queries)
	suffix := randomSuffix(t)
	host, err := macHostRepo.SeedRevokedHostForTest(ctx,
		"test-pc-host-"+suffix, "test-version", 1, "test-hash-"+suffix)
	require.NoError(t, err)

	cleanup := func() {
		_ = repo.HardDeleteByMacHost(ctx, host.ID)
		database.Close()
	}
	return phoneCallTestEnv{
		ctx:             ctx,
		repo:            repo,
		contactRepo:     contactRepo,
		interactionRepo: interactionRepo,
		hostID:          host.ID,
	}, cleanup
}

func TestPhoneCallRepository_UpsertAndGet(t *testing.T) {
	env, cleanup := setupPhoneCallTest(t)
	defer cleanup()

	suffix := randomSuffix(t)
	contact, err := env.contactRepo.CreateContact(env.ctx, repository.CreateContactRequest{FullName: "Test PC Upsert " + suffix})
	require.NoError(t, err)
	defer func() { _ = env.contactRepo.SoftDeleteContact(env.ctx, contact.ID) }()

	startedAt := accelerated.GetCurrentTime().Truncate(time.Microsecond)
	answered := true
	uniqueID := "call-" + suffix

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

	_, err := env.repo.GetCallByUniqueID(env.ctx, "nonexistent-call-"+randomSuffix(t))
	require.Error(t, err)
	assert.True(t, errors.Is(err, db.ErrNotFound))
}

func TestPhoneCallRepository_MarkProcessed_WithInteraction(t *testing.T) {
	env, cleanup := setupPhoneCallTest(t)
	defer cleanup()

	suffix := randomSuffix(t)
	contact, err := env.contactRepo.CreateContact(env.ctx, repository.CreateContactRequest{FullName: "Test PC Processed " + suffix})
	require.NoError(t, err)
	defer func() { _ = env.contactRepo.SoftDeleteContact(env.ctx, contact.ID) }()

	ref := "call-mp-" + suffix
	defer func() {
		_ = env.interactionRepo.HardDeleteInteractionsBySourceRefPrefix(env.ctx, "phone_calls", "call-mp-%")
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

	suffix := randomSuffix(t)
	answered := false
	call, err := env.repo.UpsertCall(env.ctx, repository.UpsertPhoneCallParams{
		CallUniqueID:    "call-noix-" + suffix,
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

	suffix := randomSuffix(t)
	answered := true
	_, err := env.repo.UpsertCall(env.ctx, repository.UpsertPhoneCallParams{
		CallUniqueID:    "call-del-" + suffix,
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

	_, err = env.repo.GetCallByUniqueID(env.ctx, "call-del-"+suffix)
	assert.True(t, errors.Is(err, db.ErrNotFound))
}
