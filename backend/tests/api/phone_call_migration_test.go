package api

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/golang-migrate/migrate/v4"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPhoneCallMigration053 verifies the up + down behaviour of migration
// 053. Three cases covering the row-bearing-guard semantics:
//
//  1. Clean up + empty down: apply 053 up, then 053 down with no rows
//     in either phone_call or interaction(source='phone_calls'). The
//     down must succeed; phone_call table dropped, CHECK reverted to
//     the pre-053 set.
//  2. Down refuses while phone_call rows exist: apply up, insert a
//     phone_call row via the repository, attempt down. Guard raises.
//     Hard-delete the row, re-run down — succeeds.
//  3. Down refuses while interaction(source='phone_calls') rows exist:
//     apply up, insert an interaction with source='phone_calls', attempt
//     down. Guard raises. Hard-delete (NOT soft-delete: guard counts
//     rows regardless of deleted_at), re-run down — succeeds.
//
// Gated by MAC_HOST_MIGRATION_TEST because this test mutates shared
// schema (rolls down to 052, re-applies 053). Same isolation reasoning
// as TestMacHostMigrations.
func TestPhoneCallMigration053(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	if os.Getenv("MAC_HOST_MIGRATION_TEST") == "" {
		t.Skip("MAC_HOST_MIGRATION_TEST not set; this test mutates shared schema and must run in isolation")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	ctx := context.Background()

	// Always restore the schema to HEAD on exit.
	t.Cleanup(func() {
		m, err := newMigrator(databaseURL)
		if err != nil {
			t.Logf("cleanup migrator: %v", err)
			return
		}
		defer closeMigrator(t, m)
		if version, dirty, vErr := m.Version(); vErr == nil && dirty {
			t.Logf("cleanup: forcing migration version %d (was dirty)", version)
			if fErr := m.Force(int(version)); fErr != nil {
				t.Logf("cleanup force: %v", fErr)
			}
		}
		if err := m.Up(); err != nil && err != migrate.ErrNoChange {
			t.Logf("cleanup Up: %v", err)
		}
	})

	// Roll down to 052 so we can re-apply 053 from scratch.
	mig, err := newMigrator(databaseURL)
	require.NoError(t, err)
	require.NoError(t, mig.Migrate(52), "migrate to 052")
	closeMigrator(t, mig)

	// Apply 053 — phone_call table created, CHECK extended.
	mig, err = newMigrator(databaseURL)
	require.NoError(t, err)
	require.NoError(t, mig.Steps(1), "053 up")
	closeMigrator(t, mig)

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	phoneCallRepo := repository.NewPhoneCallRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)
	macHostRepo := repository.NewMacHostRepository(database.Queries)

	suffix := uuid.NewString()[:8]

	// Case 1: clean up + empty down. With no rows present, down succeeds.
	t.Run("CleanDown", func(t *testing.T) {
		mig, err := newMigrator(databaseURL)
		require.NoError(t, err)
		require.NoError(t, mig.Steps(-1), "053 down with empty tables must succeed")
		closeMigrator(t, mig)

		// Re-apply for the next sub-tests.
		mig, err = newMigrator(databaseURL)
		require.NoError(t, err)
		require.NoError(t, mig.Steps(1), "053 up (re-apply)")
		closeMigrator(t, mig)
	})

	// Case 2: a phone_call row blocks the down migration.
	t.Run("DownRefusesPhoneCallRows", func(t *testing.T) {
		// Seed a per-test host for FK + cleanup.
		host, err := macHostRepo.SeedRevokedHostForTest(ctx,
			"mig-pc-host-"+suffix+"-2", "0.0.0", 1, "h2-"+suffix)
		require.NoError(t, err)

		uniqueID := "mig-pc-call-" + suffix + "-2"
		answered := true
		_, err = phoneCallRepo.UpsertCall(ctx, repository.UpsertPhoneCallParams{
			CallUniqueID:    uniqueID,
			PeerHandle:      "+15551234567",
			PeerNormalized:  "+15551234567",
			Service:         repository.PhoneCallServiceVoice,
			Direction:       repository.PhoneCallDirectionInbound,
			Answered:        &answered,
			DurationSeconds: 1,
			StartedAt:       accelerated.GetCurrentTime().Truncate(time.Microsecond),
			MacHostID:       &host.ID,
		})
		require.NoError(t, err)

		// Down must refuse.
		mig, err := newMigrator(databaseURL)
		require.NoError(t, err)
		downErr := mig.Steps(-1)
		require.Error(t, downErr, "053 down must fail while phone_call rows exist")
		assert.True(t,
			strings.Contains(downErr.Error(), "cannot drop phone_call") ||
				strings.Contains(downErr.Error(), "phone_call"),
			"error should mention phone_call guard, got: %v", downErr)
		require.NoError(t, mig.Force(53), "clear dirty migration flag")
		closeMigrator(t, mig)

		// Hard-delete the row, then down succeeds.
		require.NoError(t, phoneCallRepo.HardDeleteByUniqueID(ctx, uniqueID))
		mig, err = newMigrator(databaseURL)
		require.NoError(t, err)
		require.NoError(t, mig.Steps(-1), "053 down with no phone_call rows must succeed")
		closeMigrator(t, mig)

		// Re-apply for the next sub-test.
		mig, err = newMigrator(databaseURL)
		require.NoError(t, err)
		require.NoError(t, mig.Steps(1), "053 up (re-apply)")
		closeMigrator(t, mig)
	})

	// Case 3: an interaction with source='phone_calls' blocks the down
	// migration.
	t.Run("DownRefusesPhoneCallsInteraction", func(t *testing.T) {
		contact, err := contactRepo.CreateContact(ctx,
			repository.CreateContactRequest{FullName: "Mig PC Contact " + suffix})
		require.NoError(t, err)
		t.Cleanup(func() { _ = contactRepo.SoftDeleteContact(ctx, contact.ID) })

		ref := "mig-pc-ix-" + suffix
		interaction, err := interactionRepo.CreateInteraction(ctx, repository.CreateInteractionRequest{
			ContactID:  contact.ID,
			Source:     "phone_calls",
			SourceRef:  &ref,
			OccurredAt: accelerated.GetCurrentTime().Truncate(time.Microsecond),
			Direction:  repository.InteractionDirectionInbound,
		})
		require.NoError(t, err)
		_ = interaction // referenced for explicitness

		// Down must refuse on the CHECK-revert guard.
		mig, err := newMigrator(databaseURL)
		require.NoError(t, err)
		downErr := mig.Steps(-1)
		require.Error(t, downErr, "053 down must fail while phone_calls interactions exist")
		assert.True(t,
			strings.Contains(downErr.Error(), "phone_calls") ||
				strings.Contains(downErr.Error(), "interaction.source"),
			"error should mention phone_calls/source guard, got: %v", downErr)
		require.NoError(t, mig.Force(53), "clear dirty migration flag")
		closeMigrator(t, mig)

		// Hard-delete (NOT soft-delete: the guard counts rows regardless
		// of deleted_at). Use the source-ref-prefix helper.
		require.NoError(t, interactionRepo.HardDeleteInteractionsBySourceRefPrefix(ctx, "phone_calls", "mig-pc-ix-%"))

		mig, err = newMigrator(databaseURL)
		require.NoError(t, err)
		require.NoError(t, mig.Steps(-1), "053 down with no phone_calls interactions must succeed")
		closeMigrator(t, mig)

		// Confirm the CHECK reverted: inserting a phone_calls
		// interaction must now fail.
		ref2 := "mig-pc-ix-post-" + suffix
		_, err = interactionRepo.CreateInteraction(ctx, repository.CreateInteractionRequest{
			ContactID:  contact.ID,
			Source:     "phone_calls",
			SourceRef:  &ref2,
			OccurredAt: accelerated.GetCurrentTime().Truncate(time.Microsecond),
			Direction:  repository.InteractionDirectionInbound,
		})
		require.Error(t, err, "phone_calls source must be rejected after 053 down")
		assert.True(t, strings.Contains(err.Error(), "interaction_source_check") ||
			strings.Contains(err.Error(), "check constraint"),
			"error should mention check constraint, got: %v", err)

		// Re-apply 053 so subsequent cleanup is consistent.
		mig, err = newMigrator(databaseURL)
		require.NoError(t, err)
		require.NoError(t, mig.Steps(1), "053 up (re-apply for cleanup)")
		closeMigrator(t, mig)
	})

	// Final sanity: the table exists post-test (the outer Cleanup will
	// migrate.Up() if not).
	_, err = phoneCallRepo.GetCallByUniqueID(ctx, "definitely-not-here-"+suffix)
	assert.True(t, errors.Is(err, db.ErrNotFound))
}
