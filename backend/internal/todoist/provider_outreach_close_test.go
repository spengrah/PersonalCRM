package todoist

import (
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression tests for outreach-triggered cadence task closure.
//
// When the user sends a message via a non-Todoist source (e.g., Telegram),
// the CRM records an outbound interaction and updates last_outreach_at. The
// reconciler should detect this and close the existing cadence task (outreach
// reminder) in Todoist.
//
// These tests use the shared dismissalTestEnv harness from
// provider_dismissal_test.go. The strict countingRecorder is fine because
// outreach detection never calls RecordInteraction.

// createCadenceTask creates a managed cadence task for the given contact with
// the specified metadata. Returns the created task.
func createCadenceTask(t *testing.T, env *dismissalTestEnv, contactID uuid.UUID, externalID string, metadata map[string]any) *repository.ContactTask {
	t.Helper()
	task, err := env.contactTaskRepo.CreateContactTask(env.ctx, repository.CreateContactTaskRequest{
		ContactID:      contactID,
		Provider:       SourceName,
		Kind:           contacttask.KindReachOut,
		Lifecycle:      contacttask.LifecycleCadenceDue,
		ExternalTaskID: externalID,
		State:          string(repository.ContactTaskStateManaged),
		Metadata:       metadata,
	})
	require.NoError(t, err)
	return task
}

// TestReconcile_CloseCadenceTaskOnOutreachWithPendingFollowUp is the primary
// regression test for the root cause. When a Telegram outbound message updates
// last_outreach_at AND creates a follow-up task, the reconciler must still
// close the cadence task despite the follow-up gate.
func TestReconcile_CloseCadenceTaskOnOutreachWithPendingFollowUp(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	contact, snap := createDismissalContact(t, env, "OutreachClose")

	// Seed a managed cadence task with synced_last_outreach_at in the past.
	oldOutreach := snap.LastOutreachAt.Add(-24 * time.Hour)
	cadenceExtID := "td-cadence-" + uuid.New().String()[:8]
	cadenceTask := createCadenceTask(t, env, contact.ID, cadenceExtID, map[string]any{
		MetadataKeySyncedDeadline:       contact.ContactBy.Format(DateFormat),
		MetadataKeySyncedLastContacted:  snap.LastContacted.Format(time.RFC3339),
		MetadataKeySyncedLastOutreachAt: oldOutreach.Format(time.RFC3339),
	})

	// Create a pending follow-up (simulating the Telegram outbound flow).
	followUpExtID := "td-followup-" + uuid.New().String()[:8]
	followUp := createFollowUpTask(t, env, contact.ID, followUpExtID)

	// Advance last_outreach_at (simulating a new Telegram outbound).
	newOutreach := accelerated.GetCurrentTime().UTC().Truncate(time.Second)
	require.NoError(t, env.contactRepo.UpdateContactOutreachAt(env.ctx, contact.ID, newOutreach, true))

	// Run reconciler.
	commands := env.provider.reconcileContactTasks(env.ctx, nil, env.settings, env.accountID, false)

	// Assert: item_close command for the cadence task.
	var closedIDs []string
	for _, cmd := range commands {
		if cmd.Type == "item_close" {
			closedIDs = append(closedIDs, cmd.Args["id"].(string))
		}
	}
	assert.Contains(t, closedIDs, cadenceExtID, "cadence task must be closed in Todoist")

	// Assert: no new cadence task row created for this contact (the completed one
	// still exists but no replacement was created).
	_, lookupErr := env.contactTaskRepo.GetContactTaskByContactCadenceDue(env.ctx, contact.ID, SourceName)
	require.NoError(t, lookupErr, "completed cadence task row should still exist")

	// Assert: cadence task state is completed.
	reloadedCadence, err := env.contactTaskRepo.GetContactTask(env.ctx, cadenceTask.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.ContactTaskStateCompleted, reloadedCadence.State)

	// Assert: contact_by is unchanged.
	reloadedContact, err := env.contactRepo.GetContact(env.ctx, contact.ID)
	require.NoError(t, err)
	assert.True(t, reloadedContact.ContactBy.Equal(*snap.ContactBy),
		"contact_by must not change; want=%v got=%v", *snap.ContactBy, *reloadedContact.ContactBy)

	// Assert: follow-up task is unchanged.
	reloadedFollowUp, err := env.contactTaskRepo.GetContactTask(env.ctx, followUp.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.ContactTaskStateManaged, reloadedFollowUp.State,
		"follow-up task must remain managed")

	// Assert: no events published (outreach detection path).
	assert.Empty(t, env.bus.Published(), "outreach detection must not publish events")
}

// TestReconcile_CadenceTaskUnchangedWhenNoNewOutreach is the negative case:
// last_outreach_at equals synced_last_outreach_at, so no outreach is detected.
func TestReconcile_CadenceTaskUnchangedWhenNoNewOutreach(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	contact, snap := createDismissalContact(t, env, "NoOutreach")

	// Seed cadence task with synced_last_outreach_at matching the contact's current value.
	cadenceExtID := "td-cadence-noout-" + uuid.New().String()[:8]
	createCadenceTask(t, env, contact.ID, cadenceExtID, map[string]any{
		MetadataKeySyncedDeadline:       contact.ContactBy.Format(DateFormat),
		MetadataKeySyncedLastContacted:  snap.LastContacted.Format(time.RFC3339),
		MetadataKeySyncedLastOutreachAt: snap.LastOutreachAt.Format(time.RFC3339),
	})

	// No change to last_outreach_at — reconciler should not close the task.
	commands := env.provider.reconcileContactTasks(env.ctx, nil, env.settings, env.accountID, false)

	// Assert: no item_close command for the cadence task.
	for _, cmd := range commands {
		if cmd.Type == "item_close" {
			if id, ok := cmd.Args["id"].(string); ok {
				assert.NotEqual(t, cadenceExtID, id,
					"cadence task must NOT be closed when no new outreach detected")
			}
		}
	}
}

// TestReconcile_OutreachClosesWithoutPendingFollowUp verifies the cadence task
// closes even when no follow-up task exists. On the next tick, the cadence task
// should be recreated (since there's no follow-up to gate recreation).
func TestReconcile_OutreachClosesWithoutPendingFollowUp(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	contact, snap := createDismissalContact(t, env, "OutreachNoFollowUp")

	// Seed cadence task with old synced_last_outreach_at.
	oldOutreach := snap.LastOutreachAt.Add(-24 * time.Hour)
	cadenceExtID := "td-cadence-nofu-" + uuid.New().String()[:8]
	createCadenceTask(t, env, contact.ID, cadenceExtID, map[string]any{
		MetadataKeySyncedDeadline:       contact.ContactBy.Format(DateFormat),
		MetadataKeySyncedLastContacted:  snap.LastContacted.Format(time.RFC3339),
		MetadataKeySyncedLastOutreachAt: oldOutreach.Format(time.RFC3339),
	})

	// No follow-up task exists.

	// Advance last_outreach_at.
	newOutreach := accelerated.GetCurrentTime().UTC().Truncate(time.Second)
	require.NoError(t, env.contactRepo.UpdateContactOutreachAt(env.ctx, contact.ID, newOutreach, true))

	// First reconcile: cadence task should be closed.
	commands := env.provider.reconcileContactTasks(env.ctx, nil, env.settings, env.accountID, false)

	var closedIDs []string
	for _, cmd := range commands {
		if cmd.Type == "item_close" {
			closedIDs = append(closedIDs, cmd.Args["id"].(string))
		}
	}
	assert.Contains(t, closedIDs, cadenceExtID, "cadence task must be closed on outreach")

	// Second reconcile: completed task is cleaned up, new cadence task created
	// (no follow-up gate blocks recreation).
	commands2 := env.provider.reconcileContactTasks(env.ctx, nil, env.settings, env.accountID, false)

	var addCount int
	for _, cmd := range commands2 {
		if cmd.Type == "item_add" {
			addCount++
		}
	}
	assert.GreaterOrEqual(t, addCount, 1, "new cadence task should be recreated on the next tick")
}

// TestReconcile_LegacyTaskBackfillsWithoutClosing is a migration test for the
// pre-gate backfill path. Legacy tasks without synced_last_outreach_at should
// get the key backfilled on the first cycle without closing, then detect
// outreach on the second cycle.
func TestReconcile_LegacyTaskBackfillsWithoutClosing(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	contact, snap := createDismissalContact(t, env, "LegacyBackfill")

	// Seed cadence task WITHOUT synced_last_outreach_at (legacy task).
	cadenceExtID := "td-cadence-legacy-" + uuid.New().String()[:8]
	cadenceTask := createCadenceTask(t, env, contact.ID, cadenceExtID, map[string]any{
		MetadataKeySyncedDeadline:      contact.ContactBy.Format(DateFormat),
		MetadataKeySyncedLastContacted: snap.LastContacted.Format(time.RFC3339),
		// deliberately omitted: MetadataKeySyncedLastOutreachAt
	})

	// Create a pending follow-up so the follow-up gate would block
	// reconcileExistingTask (verifying the pre-gate backfill runs).
	followUpExtID := "td-followup-legacy-" + uuid.New().String()[:8]
	createFollowUpTask(t, env, contact.ID, followUpExtID)

	// First reconcile: should backfill synced_last_outreach_at without closing.
	commands := env.provider.reconcileContactTasks(env.ctx, nil, env.settings, env.accountID, false)

	// Assert: no item_close for the cadence task.
	for _, cmd := range commands {
		if cmd.Type == "item_close" {
			if id, ok := cmd.Args["id"].(string); ok {
				assert.NotEqual(t, cadenceExtID, id,
					"cadence task must NOT be closed during backfill cycle")
			}
		}
	}

	// Assert: metadata now has synced_last_outreach_at.
	reloaded, err := env.contactTaskRepo.GetContactTask(env.ctx, cadenceTask.ID)
	require.NoError(t, err)
	syncedLO, ok := reloaded.Metadata[MetadataKeySyncedLastOutreachAt].(string)
	require.True(t, ok, "synced_last_outreach_at must be backfilled")
	assert.Equal(t, snap.LastOutreachAt.Format(time.RFC3339), syncedLO,
		"backfilled value must match contact's current last_outreach_at")

	// Second cycle: advance last_outreach_at past the backfilled value.
	newOutreach := accelerated.GetCurrentTime().UTC().Truncate(time.Second)
	require.NoError(t, env.contactRepo.UpdateContactOutreachAt(env.ctx, contact.ID, newOutreach, true))

	commands2 := env.provider.reconcileContactTasks(env.ctx, nil, env.settings, env.accountID, false)

	var closedIDs []string
	for _, cmd := range commands2 {
		if cmd.Type == "item_close" {
			closedIDs = append(closedIDs, cmd.Args["id"].(string))
		}
	}
	assert.Contains(t, closedIDs, cadenceExtID, "cadence task must be closed on second cycle after backfill")
}

// TestReconcile_OutreachDetectionStateFailureSkipsClose verifies that if the
// state transition to 'completed' fails, no item_close command is emitted.
// Uses a cancelled context to deterministically fail the DB call (same pattern
// as TestHandleFollowUpDismissal_StateUpdateErrorPropagates).
func TestReconcile_OutreachDetectionStateFailureSkipsClose(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	contact, snap := createDismissalContact(t, env, "StateFailure")

	// Seed cadence task with old synced_last_outreach_at so outreach IS detected.
	oldOutreach := snap.LastOutreachAt.Add(-24 * time.Hour)
	cadenceExtID := "td-cadence-fail-" + uuid.New().String()[:8]
	cadenceTask := createCadenceTask(t, env, contact.ID, cadenceExtID, map[string]any{
		MetadataKeySyncedDeadline:       contact.ContactBy.Format(DateFormat),
		MetadataKeySyncedLastContacted:  snap.LastContacted.Format(time.RFC3339),
		MetadataKeySyncedLastOutreachAt: oldOutreach.Format(time.RFC3339),
	})

	// Advance last_outreach_at so wasReachedOutSinceSync returns true.
	newOutreach := accelerated.GetCurrentTime().UTC().Truncate(time.Second)
	require.NoError(t, env.contactRepo.UpdateContactOutreachAt(env.ctx, contact.ID, newOutreach, true))

	// Reload contact for closeOnOutreach.
	reloadedContact, err := env.contactRepo.GetContact(env.ctx, contact.ID)
	require.NoError(t, err)

	// Call closeOnOutreach with a cancelled context to force state update failure.
	badCtx := cancelledContext()
	commands, handled := env.provider.closeOnOutreach(badCtx, cadenceTask, reloadedContact)

	// Assert: not handled and no commands returned (safe failure — don't close remote).
	assert.False(t, handled, "outreach must not be handled when state update fails")
	assert.Nil(t, commands, "no commands must be returned when state update fails")

	// Assert: cadence task remains managed (state update failed).
	reloadedTask, err := env.contactTaskRepo.GetContactTask(env.ctx, cadenceTask.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.ContactTaskStateManaged, reloadedTask.State,
		"cadence task must remain managed when state update fails")

	// Assert: no events published (outreach detection path).
	assert.Empty(t, env.bus.Published(), "outreach detection must not publish events")
}

// TestCloseOnOutreach_PendingTempID verifies that when outreach is detected but
// the cadence task has a pending temp ID (no real Todoist ID yet), closeOnOutreach
// still returns handled=true (so the caller skips further processing) but emits
// no item_close command (can't close what Todoist hasn't confirmed yet). The DB
// row must still transition to completed.
func TestCloseOnOutreach_PendingTempID(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	contact, snap := createDismissalContact(t, env, "PendingTemp")

	// Seed cadence task with a pending temp ID (simulating a task whose Todoist
	// ID hasn't been resolved yet) and old synced_last_outreach_at.
	oldOutreach := snap.LastOutreachAt.Add(-24 * time.Hour)
	tempID := "tmp-" + uuid.New().String()[:8]
	cadenceTask := createCadenceTask(t, env, contact.ID, tempID, map[string]any{
		MetadataKeySyncedDeadline:       contact.ContactBy.Format(DateFormat),
		MetadataKeySyncedLastContacted:  snap.LastContacted.Format(time.RFC3339),
		MetadataKeySyncedLastOutreachAt: oldOutreach.Format(time.RFC3339),
		MetadataKeyPendingTempID:        tempID, // marks this as a pending temp ID
	})

	// Advance last_outreach_at so outreach is detected.
	newOutreach := accelerated.GetCurrentTime().UTC().Truncate(time.Second)
	require.NoError(t, env.contactRepo.UpdateContactOutreachAt(env.ctx, contact.ID, newOutreach, true))

	// Reload contact.
	reloadedContact, err := env.contactRepo.GetContact(env.ctx, contact.ID)
	require.NoError(t, err)

	commands, handled := env.provider.closeOnOutreach(env.ctx, cadenceTask, reloadedContact)

	// Assert: handled=true (outreach was detected and processed).
	assert.True(t, handled, "outreach must be handled even with pending temp ID")

	// Assert: no item_close command (can't close a pending temp task in Todoist).
	assert.Empty(t, commands, "no item_close command should be emitted for pending temp ID tasks")

	// Assert: DB row transitioned to completed.
	reloadedTask, err := env.contactTaskRepo.GetContactTask(env.ctx, cadenceTask.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.ContactTaskStateCompleted, reloadedTask.State,
		"cadence task must be marked completed even with pending temp ID")
}

// TestCloseOnOutreach_NilLastOutreachAt verifies that closeOnOutreach returns
// nil when the contact has no last_outreach_at (wasReachedOutSinceSync returns
// false).
func TestCloseOnOutreach_NilLastOutreachAt(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	cadence := "monthly"
	contact, err := env.contactRepo.CreateContact(env.ctx, repository.CreateContactRequest{
		FullName: "NoOutreach " + uuid.New().String()[:8],
		Cadence:  &cadence,
	})
	require.NoError(t, err)

	cadenceExtID := "td-cadence-nil-" + uuid.New().String()[:8]
	task := createCadenceTask(t, env, contact.ID, cadenceExtID, map[string]any{
		MetadataKeySyncedDeadline: "2099-01-01",
	})

	// contact.LastOutreachAt is nil — wasReachedOutSinceSync should return false.
	commands, handled := env.provider.closeOnOutreach(env.ctx, task, contact)
	assert.False(t, handled, "closeOnOutreach must not handle when LastOutreachAt is nil")
	assert.Nil(t, commands, "closeOnOutreach must return nil when LastOutreachAt is nil")

	// Verify task is still managed.
	_, findErr := env.contactTaskRepo.GetContactTaskByContactCadenceDue(env.ctx, contact.ID, SourceName)
	require.NoError(t, findErr, "cadence task must still exist as managed")

	// Clean up — soft-delete the contact so it doesn't pollute other tests.
	require.NoError(t, env.contactRepo.SoftDeleteContact(env.ctx, contact.ID))
	_ = env.contactTaskRepo.DeleteContactTask(env.ctx, task.ID)
}
