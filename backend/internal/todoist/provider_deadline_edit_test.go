package todoist

import (
	"testing"
	"time"

	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression tests for the processItem deadline-edit branch.
//
// The deadline-edit branch in processItem (Todoist wins) is responsible for
// propagating a user's deadline edit on a managed Todoist task back into
// contact.contact_by. Two layers of correctness are pinned here:
//
//   1. Kind guard: the branch only fires when task.Lifecycle == contacttask.LifecycleCadenceDue
//      so follow-up tasks (which carry an unrelated grace-period deadline)
//      cannot regress contact_by.
//   2. Synced-deadline gate: a cadence task whose item.Deadline matches
//      MetadataKeySyncedDeadline is treated as a stale Todoist re-delivery
//      (CRM advanced contact_by but hasn't pushed yet) and skipped, so the
//      Todoist value cannot clobber a more-recent CRM value. A mismatch is
//      treated as a legitimate Todoist-side edit and clobbers as before.
//
// All tests use the shared dismissalTestEnv helpers from
// provider_dismissal_test.go. The strict countingRecorder is fine for every
// case because the deadline-edit branch never calls RecordInteraction.

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
		Kind:           contacttask.KindReachOut,
		Lifecycle:      contacttask.LifecycleFollowUpLoop,
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
	assert.Empty(t, env.bus.Published(), "deadline-edit path must not publish events")

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

// TestProcessItem_LegitimateTodoistEditStillClobbersContactBy is the
// regression guard for the legitimate cadence-edit path. The seeded
// synced_deadline ("2027-02-03") matches contact.ContactBy and DIFFERS from
// the incoming item.Deadline ("2026-02-24"), which is the "user actually
// edited the deadline in Todoist" precondition — Todoist wins. Without this
// test an over-eager fix could disable the deadline-edit branch entirely
// and break the user-visible flow where editing a cadence task's deadline
// in Todoist updates the CRM's contact_by on the next sync tick.
func TestProcessItem_LegitimateTodoistEditStillClobbersContactBy(t *testing.T) {
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
		Kind:           contacttask.KindReachOut,
		Lifecycle:      contacttask.LifecycleCadenceDue,
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
	assert.Empty(t, env.bus.Published())

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

// TestProcessItem_StaleTodoistDeadlineDoesNotClobberContactBy is the
// primary gate test. The Todoist Sync API can re-deliver a cadence task
// for unrelated reasons (e.g. a sibling task in the same project changed),
// carrying its old deadline. If CRM advanced contact_by between ticks but
// hasn't yet pushed the new deadline to Todoist, item.Deadline ==
// synced_deadline (both = the previous CRM value). The branch must NOT
// clobber contact_by back to that stale value.
//
// This is the regression test for the user-reported "Mark as Contacted
// reverts ~5s later" symptom on prod.
func TestProcessItem_StaleTodoistDeadlineDoesNotClobberContactBy(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	contact, _ := createDismissalContact(t, env, "StaleDeadline")

	// CRM advanced contact_by (2026-04-30) — simulates "Mark as Contacted"
	// on 2026-04-23 with weekly cadence. The Todoist task still carries
	// the old deadline (2026-04-19) and synced_deadline matches it
	// because we haven't pushed the new value yet.
	advancedContactBy := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	require.NoError(t, env.contactRepo.UpdateContactBy(env.ctx, contact.ID, advancedContactBy))

	externalID := "td-stale-" + uuid.New().String()[:8]
	staleDeadline := "2026-04-19"
	cadenceMetadata := map[string]any{
		MetadataKeySyncedDeadline:      staleDeadline,
		MetadataKeySyncedLastContacted: "2026-04-23T15:00:47Z",
	}
	task, err := env.contactTaskRepo.CreateContactTask(env.ctx, repository.CreateContactTaskRequest{
		ContactID:      contact.ID,
		Provider:       SourceName,
		Kind:           contacttask.KindReachOut,
		Lifecycle:      contacttask.LifecycleCadenceDue,
		ExternalTaskID: externalID,
		State:          string(repository.ContactTaskStateManaged),
		Metadata:       cadenceMetadata,
	})
	require.NoError(t, err)

	r := env.provider.processItem(env.ctx, SyncItem{
		ID:        externalID,
		IsDeleted: false,
		Labels:    []string{env.settings.LabelName},
		Deadline:  &SyncDate{Date: staleDeadline}, // matches synced_deadline
	}, env.settings, env.accountID)

	require.NoError(t, r.Err)
	assert.True(t, r.Processed)
	assert.Empty(t, r.Commands, "gate path emits no Todoist commands")
	assert.Empty(t, env.bus.Published(), "gate path must not publish events")

	// contact_by is still the advanced CRM value — gate held.
	reloaded, err := env.contactRepo.GetContact(env.ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, reloaded.ContactBy)
	assert.True(t, reloaded.ContactBy.Equal(advancedContactBy),
		"contact_by must NOT be reverted by stale Todoist deadline; want=%v got=%v",
		advancedContactBy, *reloaded.ContactBy)

	// Metadata unchanged.
	reloadedTask, err := env.contactTaskRepo.GetContactTask(env.ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, staleDeadline, reloadedTask.Metadata[MetadataKeySyncedDeadline],
		"synced_deadline must not be rewritten on the gate path")
	assert.Equal(t, "2026-04-23T15:00:47Z", reloadedTask.Metadata[MetadataKeySyncedLastContacted],
		"synced_last_contacted must be preserved")
}

// TestProcessItem_MissingSyncedDeadlineStillClobbers documents the legacy-
// task edge: when synced_deadline is absent (task created before #227), the
// gate cannot distinguish a stale re-delivery from a legitimate Todoist
// edit. We preserve pre-fix behavior — fire the clobber and backfill
// synced_deadline so future ticks have a populated gate. The accepted
// tradeoff: a legacy task whose CRM contact_by was recently advanced may
// still get clobbered on the first post-fix sync, but that's recoverable
// (user re-marks). The unacceptable alternative — skip + drop a legitimate
// Todoist edit — is permanent data loss because the incremental cursor
// advances past the unprocessed item.
func TestProcessItem_MissingSyncedDeadlineStillClobbers(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	contact, _ := createDismissalContact(t, env, "LegacyNoSynced")

	seededContactBy := time.Date(2027, 2, 3, 0, 0, 0, 0, time.UTC)
	require.NoError(t, env.contactRepo.UpdateContactBy(env.ctx, contact.ID, seededContactBy))

	externalID := "td-legacy-" + uuid.New().String()[:8]
	// No synced_deadline key — legacy task.
	task, err := env.contactTaskRepo.CreateContactTask(env.ctx, repository.CreateContactTaskRequest{
		ContactID:      contact.ID,
		Provider:       SourceName,
		Kind:           contacttask.KindReachOut,
		Lifecycle:      contacttask.LifecycleCadenceDue,
		ExternalTaskID: externalID,
		State:          string(repository.ContactTaskStateManaged),
		Metadata:       map[string]any{},
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

	// contact_by clobbered.
	reloaded, err := env.contactRepo.GetContact(env.ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, reloaded.ContactBy)
	expected := time.Date(2026, 2, 24, 0, 0, 0, 0, time.UTC)
	assert.True(t, reloaded.ContactBy.Equal(expected),
		"contact_by should be clobbered to the Todoist deadline on legacy tasks")

	// synced_deadline backfilled so the next tick's gate is populated.
	reloadedTask, err := env.contactTaskRepo.GetContactTask(env.ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, "2026-02-24", reloadedTask.Metadata[MetadataKeySyncedDeadline],
		"synced_deadline must be backfilled with the new Todoist deadline")
}

// TestHandleSkipTrigger_AdvancesContactByViaCadenceUpdater verifies that
// the skip-trigger code path now routes contact_by writes through
// CadenceUpdater.ApplyContactByOverride rather than the removed direct
// repository call. Asserts the fake's call counter incremented to prove
// the swap landed.
func TestHandleSkipTrigger_AdvancesContactByViaCadenceUpdater(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	contact, _ := createDismissalContact(t, env, "SkipViaCadence")

	cadenceExtID := "td-skip-cad-" + uuid.New().String()[:8]
	_, err := env.contactTaskRepo.CreateContactTask(env.ctx, repository.CreateContactTaskRequest{
		ContactID:      contact.ID,
		Provider:       SourceName,
		Kind:           contacttask.KindReachOut,
		Lifecycle:      contacttask.LifecycleCadenceDue,
		ExternalTaskID: cadenceExtID,
		State:          string(repository.ContactTaskStateManaged),
		Metadata:       map[string]any{},
	})
	require.NoError(t, err)

	beforeCalls := env.cadence.Calls()

	// Trigger the skip path via item.IsDeleted.
	item := SyncItem{
		ID:        cadenceExtID,
		IsDeleted: true,
		Labels:    []string{env.settings.LabelName},
		Deadline:  &SyncDate{Date: "2099-01-01"},
		UpdatedAt: "2026-04-15T12:00:00Z",
	}
	r := env.provider.processItem(env.ctx, item, env.settings, env.accountID)

	require.NoError(t, r.Err)
	assert.True(t, r.Processed)
	require.NotEmpty(t, r.Commands, "skip-trigger should return at least one command")

	// Fake's ApplyContactByOverride was invoked exactly once for this skip.
	afterCalls := env.cadence.Calls()
	assert.Equal(t, beforeCalls+1, afterCalls,
		"handleSkipTrigger must route contact_by through CadenceUpdater.ApplyContactByOverride")

	// contact_by advanced past its prior value.
	reloaded, err := env.contactRepo.GetContact(env.ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, reloaded.ContactBy)
	require.NotNil(t, contact.ContactBy)
	assert.True(t, reloaded.ContactBy.After(*contact.ContactBy),
		"contact_by should advance past its pre-skip value")
}

// TestProcessItem_StaleTodoistDeadline_DoubleDeliveryIsIdempotent pins the
// gate's idempotence invariant: a legitimate Todoist-wins edit fires once,
// backfills synced_deadline, and a second delivery of the same SyncItem is
// a no-op (the gate now sees synced_deadline == item.Deadline). Without
// this property the gate would silently re-clobber on every tick that
// re-delivers the item, defeating its point.
func TestProcessItem_StaleTodoistDeadline_DoubleDeliveryIsIdempotent(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	contact, _ := createDismissalContact(t, env, "DoubleDelivery")

	seededContactBy := time.Date(2027, 2, 3, 0, 0, 0, 0, time.UTC)
	require.NoError(t, env.contactRepo.UpdateContactBy(env.ctx, contact.ID, seededContactBy))

	externalID := "td-dbl-" + uuid.New().String()[:8]
	cadenceMetadata := map[string]any{
		MetadataKeySyncedDeadline:      "2027-02-03",
		MetadataKeySyncedLastContacted: "2026-02-03T00:00:00Z",
	}
	task, err := env.contactTaskRepo.CreateContactTask(env.ctx, repository.CreateContactTaskRequest{
		ContactID:      contact.ID,
		Provider:       SourceName,
		Kind:           contacttask.KindReachOut,
		Lifecycle:      contacttask.LifecycleCadenceDue,
		ExternalTaskID: externalID,
		State:          string(repository.ContactTaskStateManaged),
		Metadata:       cadenceMetadata,
	})
	require.NoError(t, err)

	syncItem := SyncItem{
		ID:        externalID,
		IsDeleted: false,
		Labels:    []string{env.settings.LabelName},
		Deadline:  &SyncDate{Date: "2026-02-24"}, // user-edited
	}

	// First call: legitimate Todoist edit fires the clobber + backfills.
	r1 := env.provider.processItem(env.ctx, syncItem, env.settings, env.accountID)
	require.NoError(t, r1.Err)
	assert.True(t, r1.Processed)

	afterFirst, err := env.contactRepo.GetContact(env.ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, afterFirst.ContactBy)
	expected := time.Date(2026, 2, 24, 0, 0, 0, 0, time.UTC)
	assert.True(t, afterFirst.ContactBy.Equal(expected),
		"first call should clobber contact_by to item.Deadline")

	taskAfterFirst, err := env.contactTaskRepo.GetContactTask(env.ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, "2026-02-24", taskAfterFirst.Metadata[MetadataKeySyncedDeadline],
		"synced_deadline must be backfilled to item.Deadline after first call")

	pubsAfterFirst := len(env.bus.Published())

	// Second call: same SyncItem; gate now sees synced_deadline == item.Deadline.
	r2 := env.provider.processItem(env.ctx, syncItem, env.settings, env.accountID)
	require.NoError(t, r2.Err)
	assert.True(t, r2.Processed)
	assert.Empty(t, r2.Commands, "second-call gate path must emit no commands")

	// contact_by + metadata unchanged from first-call end state.
	afterSecond, err := env.contactRepo.GetContact(env.ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, afterSecond.ContactBy)
	assert.True(t, afterFirst.ContactBy.Equal(*afterSecond.ContactBy),
		"second call must not change contact_by")

	taskAfterSecond, err := env.contactTaskRepo.GetContactTask(env.ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, taskAfterFirst.Metadata[MetadataKeySyncedDeadline],
		taskAfterSecond.Metadata[MetadataKeySyncedDeadline],
		"second call must not rewrite synced_deadline")

	// No additional events were published.
	assert.Equal(t, pubsAfterFirst, len(env.bus.Published()),
		"second call must not publish additional events")
}
