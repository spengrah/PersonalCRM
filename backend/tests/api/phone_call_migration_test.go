//go:build integration_testdb

package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/testdb"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPhoneCallMigration055 verifies the up + down behaviour of migration
// 055. The down migration refuses to drop data-bearing structures if rows
// referencing them still exist. Three cases:
//
//  1. Clean up + empty down: apply 055 up, then 055 down with no rows
//     in either phone_call or interaction(source='phone_calls'). The
//     down must succeed; phone_call table dropped, CHECK reverted to
//     the pre-055 set (which still includes 'anarlog_sessions' added
//     by migration 053).
//  2. Down refuses while phone_call rows exist: apply up, insert a
//     phone_call row via the repository, attempt down. Guard raises.
//     Hard-delete the row, re-run down — succeeds.
//  3. Down refuses while interaction(source='phone_calls') rows exist:
//     apply up, insert an interaction with source='phone_calls', attempt
//     down. Guard raises. Hard-delete (NOT soft-delete: guard counts
//     rows regardless of deleted_at), re-run down — succeeds.
//
// Runs against its own ephemeral clone (testdb.NewEphemeralClone), so the
// mid-test schema rollback (down to 054, re-apply 055) cannot affect the
// package clone or sibling packages. Same isolation reasoning as
// TestMacHostMigrations.
func TestPhoneCallMigration055(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// Dedicated ephemeral clone: dropped wholesale on cleanup, so no
	// HEAD-restore is needed.
	databaseURL, drop := testdb.NewEphemeralClone(t)
	t.Cleanup(drop)

	ctx := context.Background()

	// Roll down to 054 so we can re-apply 055 from scratch.
	mig, err := newMigrator(databaseURL)
	require.NoError(t, err)
	require.NoError(t, mig.Migrate(54), "migrate to 054")
	closeMigrator(t, mig)

	// Apply 055 — phone_call table created, CHECK extended.
	mig, err = newMigrator(databaseURL)
	require.NoError(t, err)
	require.NoError(t, mig.Steps(1), "055 up")
	closeMigrator(t, mig)

	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	require.NoError(t, err)
	t.Cleanup(database.Close)

	phoneCallRepo := repository.NewPhoneCallRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactRepo := repository.NewContactRepository(database.Queries)

	suffix := uuid.NewString()[:8]

	// Case 1: clean up + empty down. With no rows present, down succeeds.
	t.Run("CleanDown", func(t *testing.T) {
		mig, err := newMigrator(databaseURL)
		require.NoError(t, err)
		require.NoError(t, mig.Steps(-1), "055 down with empty tables must succeed")
		closeMigrator(t, mig)

		// Re-apply for the next sub-tests.
		mig, err = newMigrator(databaseURL)
		require.NoError(t, err)
		require.NoError(t, mig.Steps(1), "055 up (re-apply)")
		closeMigrator(t, mig)
	})

	// Case 2: a phone_call row blocks the down migration.
	t.Run("DownRefusesPhoneCallRows", func(t *testing.T) {
		// phone_call.mac_host_id is nullable (ON DELETE SET NULL), and the
		// 055-down guard only counts phone_call rows — it does not inspect
		// mac_host_id. We leave MacHostID nil rather than seeding a host: the
		// SeedRevokedMacHost query RETURNs api_key_rotated_at, a column added
		// by migration 057, which does not exist on this clone rolled down to
		// 055. (TestMacHostMigrations seeds via SeedMacHost, which RETURNs only
		// id and is therefore schema-version-agnostic.)
		uniqueID := "mig-pc-call-" + suffix + "-2"
		answered := true
		_, err := phoneCallRepo.UpsertCall(ctx, repository.UpsertPhoneCallParams{
			CallUniqueID:    uniqueID,
			PeerHandle:      "+15551234567",
			PeerNormalized:  "+15551234567",
			Service:         repository.PhoneCallServiceVoice,
			Direction:       repository.PhoneCallDirectionInbound,
			Answered:        &answered,
			DurationSeconds: 1,
			StartedAt:       accelerated.GetCurrentTime().Truncate(time.Microsecond),
			MacHostID:       nil,
		})
		require.NoError(t, err)

		// Down must refuse.
		mig, err := newMigrator(databaseURL)
		require.NoError(t, err)
		downErr := mig.Steps(-1)
		require.Error(t, downErr, "055 down must fail while phone_call rows exist")
		assert.True(t,
			strings.Contains(downErr.Error(), "cannot drop phone_call") ||
				strings.Contains(downErr.Error(), "phone_call"),
			"error should mention phone_call guard, got: %v", downErr)
		require.NoError(t, mig.Force(55), "clear dirty migration flag")
		closeMigrator(t, mig)

		// Hard-delete the row, then down succeeds.
		require.NoError(t, phoneCallRepo.HardDeleteByUniqueID(ctx, uniqueID))
		mig, err = newMigrator(databaseURL)
		require.NoError(t, err)
		require.NoError(t, mig.Steps(-1), "055 down with no phone_call rows must succeed")
		closeMigrator(t, mig)

		// Re-apply for the next sub-test.
		mig, err = newMigrator(databaseURL)
		require.NoError(t, err)
		require.NoError(t, mig.Steps(1), "055 up (re-apply)")
		closeMigrator(t, mig)
	})

	// Case 3: an interaction with source='phone_calls' blocks the down
	// migration.
	t.Run("DownRefusesPhoneCallsInteraction", func(t *testing.T) {
		// Raw insert (the clone is positioned at v55, which predates the node
		// table (064) that ContactRepository.CreateContact now depends on via
		// contact_id_node_fk (077); the migration is the subject, so a
		// v55-shaped raw insert is the right scaffolding here).
		var contactID uuid.UUID
		err := database.Pool.QueryRow(ctx,
			`INSERT INTO contact (full_name) VALUES ($1) RETURNING id`, "Mig PC Contact "+suffix).Scan(&contactID)
		require.NoError(t, err)
		t.Cleanup(func() { _ = contactRepo.SoftDeleteContact(ctx, contactID) })

		ref := "mig-pc-ix-" + suffix
		// Raw insert (the clone is positioned at v55, which predates the
		// interaction.venue_id column the production CreateInteraction now writes;
		// the migration is the subject, so a v55-shaped raw insert is the right
		// scaffolding here).
		_, err = database.Pool.Exec(ctx,
			`INSERT INTO interaction (contact_id, source, source_ref, occurred_at, direction) VALUES ($1, $2, $3, $4, $5)`,
			contactID, "phone_calls", ref, accelerated.GetCurrentTime().Truncate(time.Microsecond), repository.InteractionDirectionInbound)
		require.NoError(t, err)

		// Down must refuse on the CHECK-revert guard.
		mig, err := newMigrator(databaseURL)
		require.NoError(t, err)
		downErr := mig.Steps(-1)
		require.Error(t, downErr, "055 down must fail while phone_calls interactions exist")
		assert.True(t,
			strings.Contains(downErr.Error(), "phone_calls") ||
				strings.Contains(downErr.Error(), "interaction.source"),
			"error should mention phone_calls/source guard, got: %v", downErr)
		require.NoError(t, mig.Force(55), "clear dirty migration flag")
		closeMigrator(t, mig)

		// Hard-delete (NOT soft-delete: the guard counts rows regardless
		// of deleted_at). Use the source-ref-prefix helper.
		require.NoError(t, interactionRepo.HardDeleteInteractionsBySourceRefPrefix(ctx, "phone_calls", "mig-pc-ix-%"))

		mig, err = newMigrator(databaseURL)
		require.NoError(t, err)
		require.NoError(t, mig.Steps(-1), "055 down with no phone_calls interactions must succeed")
		closeMigrator(t, mig)

		// Confirm the CHECK reverted: inserting a phone_calls
		// interaction must now fail (raw insert, same v55-shape rationale).
		ref2 := "mig-pc-ix-post-" + suffix
		_, err = database.Pool.Exec(ctx,
			`INSERT INTO interaction (contact_id, source, source_ref, occurred_at, direction) VALUES ($1, $2, $3, $4, $5)`,
			contactID, "phone_calls", ref2, accelerated.GetCurrentTime().Truncate(time.Microsecond), repository.InteractionDirectionInbound)
		require.Error(t, err, "phone_calls source must be rejected after 055 down")
		assert.True(t, strings.Contains(err.Error(), "interaction_source_check") ||
			strings.Contains(err.Error(), "check constraint"),
			"error should mention check constraint, got: %v", err)

		// Re-apply 055 so subsequent cleanup is consistent.
		mig, err = newMigrator(databaseURL)
		require.NoError(t, err)
		require.NoError(t, mig.Steps(1), "055 up (re-apply for cleanup)")
		closeMigrator(t, mig)
	})

	// Final sanity: 055 was re-applied above, so the phone_call table exists
	// and the lookup returns not-found rather than a missing-table error.
	_, err = phoneCallRepo.GetCallByUniqueID(ctx, "definitely-not-here-"+suffix)
	assert.True(t, errors.Is(err, db.ErrNotFound))
}
