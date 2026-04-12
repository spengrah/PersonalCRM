package todoist

import (
	"testing"
	"time"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression tests for the processItem deadline-edit branch
// (fix/followup-deadline-regression).
//
// The deadline-edit branch in processItem (Todoist wins) is responsible for
// propagating a user's deadline edit on a managed Todoist task back into
// contact.contact_by. Before this fix it ran for any task kind, including
// follow-up tasks whose deadlines are computed on a completely different
// basis (last_outreach_at + watchdog_days). When the Telegram Phase 4
// backfill replayed historical outbound messages, it created back-dated
// follow-up tasks whose grace-period deadlines were then written into
// contact_by, regressing the cadence state for 12 contacts on prod.
//
// The fix is a single guard: the branch only fires when
// task.Kind == TaskKindCadence. These two tests pin both halves of the
// guard so a future "simplification" cannot revert it without an explicit
// signal:
//
//   - TestProcessItem_FollowUpDeadlineDoesNotOverwriteContactBy
//     Negative path: a follow-up task with a diverging deadline must NOT
//     mutate contact_by and must NOT add a synced_deadline key to the
//     follow-up's metadata.
//
//   - TestProcessItem_CadenceDeadlineOverwritesContactBy
//     Positive path: a cadence task with a diverging deadline MUST update
//     contact_by AND update its own metadata.synced_deadline (without
//     wiping other metadata keys).
//
// Both tests use the shared dismissalTestEnv helpers from
// provider_dismissal_test.go. The strict countingRecorder is fine for both
// cases because the deadline-edit branch never calls RecordInteraction.

// TestProcessItem_FollowUpDeadlineDoesNotOverwriteContactBy verifies that a
// follow-up task arriving via processItem with a deadline that differs from
// contact_by is left alone — the deadline-edit branch must short-circuit on
// task.Kind != cadence.
func TestProcessItem_FollowUpDeadlineDoesNotOverwriteContactBy(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	contact, _ := createDismissalContact(t, env, "FollowUpDeadline")

	// Override contact_by to a specific known value so the assertion can be exact.
	// 2027-02-03 is well in the future, far enough from any natural cadence
	// computation that an accidental write would be obvious.
	seededContactBy := time.Date(2027, 2, 3, 0, 0, 0, 0, time.UTC)
	require.NoError(t, env.contactRepo.UpdateContactBy(env.ctx, contact.ID, seededContactBy))

	externalID := "td-fu-deadline-" + uuid.New().String()[:8]
	followupMetadata := map[string]any{
		"due_date": "2026-02-24",
		"content":  "Follow up: test",
	}
	task, err := env.contactTaskRepo.CreateContactTask(env.ctx, repository.CreateContactTaskRequest{
		ContactID:      contact.ID,
		Provider:       SourceName,
		Kind:           TaskKindFollowUp,
		ExternalTaskID: externalID,
		State:          string(repository.ContactTaskStateManaged),
		Metadata:       followupMetadata,
	})
	require.NoError(t, err)

	// item.Deadline (2026-02-24) differs from contact.ContactBy (2027-02-03).
	// Without the guard, the branch would fire UpdateContactBy(2026-02-24)
	// and clobber the cadence state.
	r := env.provider.processItem(env.ctx, SyncItem{
		ID:        externalID,
		IsDeleted: false,
		Labels:    []string{env.settings.LabelName},
		Deadline:  &SyncDate{Date: "2026-02-24"},
	}, env.settings, env.accountID)

	require.NoError(t, r.Err)
	assert.True(t, r.Processed, "the follow-up task was handled, just without the deadline-edit side effect")
	assert.Empty(t, r.Commands, "deadline-edit path emits no Todoist commands")
	assert.Equal(t, 0, env.recorder.count, "deadline-edit path must not record interactions")

	// contact.ContactBy is unchanged.
	reloaded, err := env.contactRepo.GetContact(env.ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, reloaded.ContactBy, "contact_by must remain set")
	assert.True(t, reloaded.ContactBy.Equal(seededContactBy),
		"contact_by must NOT be regressed by a follow-up deadline; want=%v got=%v",
		seededContactBy, *reloaded.ContactBy)

	// The follow-up task's metadata must be unchanged. Specifically it must NOT
	// have gained a synced_deadline key — the buggy code path unconditionally
	// mutates task.Metadata[MetadataKeySyncedDeadline] regardless of kind.
	reloadedTask, err := env.contactTaskRepo.GetContactTask(env.ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, "2026-02-24", reloadedTask.Metadata["due_date"], "due_date must be preserved")
	assert.Equal(t, "Follow up: test", reloadedTask.Metadata["content"], "content must be preserved")
	_, hasSynced := reloadedTask.Metadata[MetadataKeySyncedDeadline]
	assert.False(t, hasSynced, "follow-up metadata must NOT gain a synced_deadline key")
}

// TestProcessItem_CadenceDeadlineOverwritesContactBy is the regression guard
// for the legitimate cadence-edit path. Without this test, an over-eager fix
// could disable the entire deadline-edit branch and break the user-visible
// flow where editing a cadence task's deadline in Todoist updates the CRM's
// contact_by on the next sync tick.
func TestProcessItem_CadenceDeadlineOverwritesContactBy(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	contact, _ := createDismissalContact(t, env, "CadenceDeadline")

	seededContactBy := time.Date(2027, 2, 3, 0, 0, 0, 0, time.UTC)
	require.NoError(t, env.contactRepo.UpdateContactBy(env.ctx, contact.ID, seededContactBy))

	externalID := "td-cad-deadline-" + uuid.New().String()[:8]
	cadenceMetadata := map[string]any{
		MetadataKeySyncedDeadline:      "2027-02-03",
		MetadataKeySyncedLastContacted: "2026-02-03T00:00:00Z",
	}
	task, err := env.contactTaskRepo.CreateContactTask(env.ctx, repository.CreateContactTaskRequest{
		ContactID:      contact.ID,
		Provider:       SourceName,
		Kind:           TaskKindCadence,
		ExternalTaskID: externalID,
		State:          string(repository.ContactTaskStateManaged),
		Metadata:       cadenceMetadata,
	})
	require.NoError(t, err)

	r := env.provider.processItem(env.ctx, SyncItem{
		ID:        externalID,
		IsDeleted: false,
		Labels:    []string{env.settings.LabelName},
		Deadline:  &SyncDate{Date: "2026-02-24"},
	}, env.settings, env.accountID)

	require.NoError(t, r.Err)
	assert.True(t, r.Processed)
	assert.Empty(t, r.Commands)
	assert.Equal(t, 0, env.recorder.count)

	// contact.ContactBy is updated to the Todoist deadline.
	reloaded, err := env.contactRepo.GetContact(env.ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, reloaded.ContactBy)
	expectedContactBy := time.Date(2026, 2, 24, 0, 0, 0, 0, time.UTC)
	assert.True(t, reloaded.ContactBy.Equal(expectedContactBy),
		"contact_by must be updated to the Todoist deadline; want=%v got=%v",
		expectedContactBy, *reloaded.ContactBy)

	// synced_deadline is updated to the new Todoist deadline; other metadata
	// keys (synced_last_contacted) survive the partial update.
	reloadedTask, err := env.contactTaskRepo.GetContactTask(env.ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, "2026-02-24", reloadedTask.Metadata[MetadataKeySyncedDeadline],
		"cadence synced_deadline must be updated to the new Todoist deadline")
	assert.Equal(t, "2026-02-03T00:00:00Z", reloadedTask.Metadata[MetadataKeySyncedLastContacted],
		"synced_last_contacted must be preserved across the metadata update")
}
