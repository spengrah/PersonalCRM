package todoist

import (
	"testing"
	"time"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSetSyncedLastOutreachAt_DoesNotTouchLastContacted is the helper-purity
// unit test for the P1 fix. setSyncedLastOutreachAt must write ONLY
// synced_last_outreach_at and leave any existing synced_last_contacted value
// byte-identical — refreshing it here would mask stale state and suppress a
// legitimate close+recreate in the deadlines-match drift path.
func TestSetSyncedLastOutreachAt_DoesNotTouchLastContacted(t *testing.T) {
	staleLC := "2023-01-01T00:00:00Z"
	metadata := map[string]any{
		MetadataKeySyncedLastContacted: staleLC,
		"sibling_key":                  "preserve-me",
	}

	// The contact's LastContacted is set to a DIFFERENT value than the stale
	// stored one: a regression that wrote contact.LastContacted into
	// synced_last_contacted would change the stored value, so this assertion
	// would catch it.
	contacted := time.Date(2024, 5, 1, 9, 0, 0, 0, time.UTC)
	outreach := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	contact := &repository.Contact{LastContacted: &contacted, LastOutreachAt: &outreach}

	got := setSyncedLastOutreachAt(metadata, contact)

	// synced_last_contacted must be untouched.
	assert.Equal(t, staleLC, got[MetadataKeySyncedLastContacted],
		"setSyncedLastOutreachAt must not modify synced_last_contacted")
	// synced_last_outreach_at must be written.
	assert.Equal(t, outreach.Format(time.RFC3339), got[MetadataKeySyncedLastOutreachAt],
		"setSyncedLastOutreachAt must write synced_last_outreach_at")
	// Sibling keys must survive (mutate-and-return, not replace).
	assert.Equal(t, "preserve-me", got["sibling_key"],
		"setSyncedLastOutreachAt must preserve sibling keys")
}

// TestSetSyncedLastOutreachAt_NilOutreach asserts the helper is a no-op for
// the outreach key when contact.LastOutreachAt is nil (and still preserves
// existing keys).
func TestSetSyncedLastOutreachAt_NilOutreach(t *testing.T) {
	metadata := map[string]any{MetadataKeySyncedLastContacted: "2023-01-01T00:00:00Z"}
	contacted := time.Date(2024, 5, 1, 9, 0, 0, 0, time.UTC)
	got := setSyncedLastOutreachAt(metadata, &repository.Contact{LastContacted: &contacted, LastOutreachAt: nil})
	_, has := got[MetadataKeySyncedLastOutreachAt]
	assert.False(t, has, "no synced_last_outreach_at should be written when LastOutreachAt is nil")
	assert.Equal(t, "2023-01-01T00:00:00Z", got[MetadataKeySyncedLastContacted],
		"setSyncedLastOutreachAt must not write synced_last_contacted even when contact has LastContacted")
}

// TestReconcileExistingTask_LOBackfillPreservesStaleLC is the behavioral guard
// for the P1 fix. Setup: a managed cadence task with a STALE stored
// synced_last_contacted, MISSING synced_last_outreach_at, and
// synced_deadline == currentDeadline. The contact has both LastContacted
// (advanced past the stale stored value) and LastOutreachAt set.
//
// reconcileExistingTask runs the synced_last_outreach_at-only backfill BEFORE
// wasContactedSinceSync. If that backfill regressed to also writing
// synced_last_contacted, it would refresh the stale value to the contact's
// current LastContacted, wasContactedSinceSync would return false, and NO
// close+recreate would be emitted. Asserting the close+recreate commands ARE
// emitted proves the LO-only backfill preserved the stale synced_last_contacted
// long enough for drift detection to fire.
//
// reconcileExistingTask is invoked directly (not via reconcileContactTasks) so
// the closeOnOutreach pre-gate backfill — which would otherwise consume the
// missing-synced_LO case first — does not pre-empt the path under test.
func TestReconcileExistingTask_LOBackfillPreservesStaleLC(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	contact, snap := createDismissalContact(t, env, "LOBackfillPreserveLC")
	require.NotNil(t, contact.LastContacted)
	require.NotNil(t, contact.LastOutreachAt, "contact must have last_outreach_at for the LO backfill")

	currentDeadline := contact.ContactBy.Format(DateFormat)

	// Stale synced_last_contacted: well before the contact's current
	// LastContacted, so wasContactedSinceSync (current > stored) can fire.
	staleLC := snap.LastContacted.AddDate(0, 0, -30)

	cadenceExtID := "td-cadence-lopreserve-" + uuid.New().String()[:8]
	task := createCadenceTask(t, env, contact.ID, cadenceExtID, map[string]any{
		MetadataKeySyncedDeadline:      currentDeadline,
		MetadataKeySyncedLastContacted: staleLC.Format(time.RFC3339),
		// deliberately omitted: MetadataKeySyncedLastOutreachAt (triggers the
		// LO-only backfill inside reconcileExistingTask).
	})

	commands := env.provider.reconcileExistingTask(env.ctx, task, contact, env.settings, currentDeadline, false)

	// The close+recreate pair must be emitted: close on the old external id,
	// plus a fresh item_add. If the LO backfill had clobbered synced_LC,
	// wasContactedSinceSync would have returned false and no commands fire.
	var closedIDs []string
	var addCount int
	for _, cmd := range commands {
		switch cmd.Type {
		case "item_close":
			if id, ok := cmd.Args["id"].(string); ok {
				closedIDs = append(closedIDs, id)
			}
		case "item_add":
			addCount++
		}
	}
	assert.Contains(t, closedIDs, cadenceExtID,
		"old cadence task must be closed (drift detected via preserved stale synced_last_contacted)")
	assert.Equal(t, 1, addCount,
		"a replacement cadence task must be created")
}
