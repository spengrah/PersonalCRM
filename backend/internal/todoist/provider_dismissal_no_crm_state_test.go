package todoist

import (
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCadenceTaskDeadline(t *testing.T) {
	today := time.Date(2026, time.September, 15, 0, 0, 0, 0, time.UTC)
	yesterday := time.Date(2026, time.September, 14, 0, 0, 0, 0, time.UTC)
	tomorrow := time.Date(2026, time.September, 16, 0, 0, 0, 0, time.UTC)

	assert.Equal(t, "2026-09-15", cadenceTaskDeadline(yesterday, today))
	assert.Equal(t, "2026-09-15", cadenceTaskDeadline(today, today))
	assert.Equal(t, "2026-09-16", cadenceTaskDeadline(tomorrow, today))

	originalLocal := time.Local
	negativeOffset, err := time.LoadLocation("America/Chicago")
	require.NoError(t, err)
	time.Local = negativeOffset
	t.Cleanup(func() { time.Local = originalLocal })
	todayLocal := time.Date(2026, time.September, 15, 0, 0, 0, 0, time.Local)
	contactByTomorrowUTC := time.Date(2026, time.September, 16, 0, 0, 0, 0, time.UTC)
	assert.Equal(t, "2026-09-16", cadenceTaskDeadline(contactByTomorrowUTC, todayLocal))
}

func TestFollowUpDismissal_LeavesAwaitingReplyAndCreatesNoCadenceTask(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	cadenceName := "monthly"
	contact, err := env.contactRepo.CreateContact(env.ctx, repository.CreateContactRequest{
		FullName: "Awaiting Reply Dismissal " + uuid.NewString(),
		Cadence:  &cadenceName,
	})
	require.NoError(t, err)

	now := accelerated.GetCurrentTime().UTC().Truncate(time.Second)
	lastContacted := now.AddDate(0, 0, -40)
	lastInteractionAt := lastContacted
	lastResponseAt := lastContacted
	lastOutreachAt := now.Add(-24 * time.Hour)
	contactBy := now.AddDate(0, 0, -10)
	awaitingReplyUntil := cadence.AwaitingReplyUntil(lastOutreachAt, 7)
	require.NoError(t, env.contactRepo.TestSeedContactCadenceFields(env.ctx, contact.ID, repository.TestCadenceSeed{
		LastContacted:      &lastContacted,
		LastInteractionAt:  &lastInteractionAt,
		LastResponseAt:     &lastResponseAt,
		LastOutreachAt:     &lastOutreachAt,
		ContactBy:          &contactBy,
		AwaitingReplyUntil: &awaitingReplyUntil,
	}))

	reloaded, err := env.contactRepo.GetContact(env.ctx, contact.ID)
	require.NoError(t, err)
	snapshot := dateSnapshot{
		LastContacted:      copyTimePtr(reloaded.LastContacted),
		LastInteractionAt:  copyTimePtr(reloaded.LastInteractionAt),
		LastResponseAt:     copyTimePtr(reloaded.LastResponseAt),
		LastOutreachAt:     copyTimePtr(reloaded.LastOutreachAt),
		ContactBy:          copyTimePtr(reloaded.ContactBy),
		AwaitingReplyUntil: copyTimePtr(reloaded.AwaitingReplyUntil),
	}

	externalID := "awaiting-dismissal-" + uuid.NewString()
	task := createFollowUpTask(t, env, contact.ID, externalID)
	r := env.provider.processItem(env.ctx, SyncItem{
		ID:        externalID,
		IsDeleted: true,
		Labels:    []string{env.settings.LabelName},
		Deadline:  &SyncDate{Date: "2099-01-01"},
	}, env.settings, env.accountID)
	require.NoError(t, r.Err)

	commands := env.provider.reconcileContactTasks(env.ctx, nil, env.settings, env.accountID, false)
	for _, command := range commands {
		if command.Type != "item_add" {
			continue
		}
		description, _ := command.Args["description"].(string)
		if strings.Contains(description, contact.ID.String()) {
			assert.Failf(t, "unexpected cadence task for awaiting contact", "deadline argument: %v", command.Args["deadline"])
		}
	}
	_, lookupErr := env.contactTaskRepo.GetContactTaskByContactCadenceDue(env.ctx, contact.ID, SourceName)
	assert.ErrorIs(t, lookupErr, db.ErrNotFound)
	assertDatesUnchanged(t, env, contact.ID, snapshot)

	dismissed, err := env.contactTaskRepo.GetContactTask(env.ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.ContactTaskStateDismissed, dismissed.State)
}

// spec: CAD-042.lapsed-window-creates-only-without-live-followup
// spec: CAD-042.resurrected-deadline-today-or-later
func TestReconcile_LapsedWindow_CreatesClampedTaskOnlyWithoutLiveFollowUp(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	cadenceName := "monthly"
	contact, err := env.contactRepo.CreateContact(env.ctx, repository.CreateContactRequest{
		FullName: "Lapsed Awaiting Reply " + uuid.NewString(),
		Cadence:  &cadenceName,
	})
	require.NoError(t, err)
	now := accelerated.GetCurrentTime().UTC().Truncate(time.Second)
	lastContacted := now.AddDate(0, 0, -40)
	lastInteractionAt := lastContacted
	lastResponseAt := lastContacted
	lastOutreachAt := now.AddDate(0, 0, -8)
	contactBy := now.AddDate(0, 0, -10)
	awaitingReplyUntil := cadence.AwaitingReplyUntil(now, -1)
	require.NoError(t, env.contactRepo.TestSeedContactCadenceFields(env.ctx, contact.ID, repository.TestCadenceSeed{
		LastContacted:      &lastContacted,
		LastInteractionAt:  &lastInteractionAt,
		LastResponseAt:     &lastResponseAt,
		LastOutreachAt:     &lastOutreachAt,
		ContactBy:          &contactBy,
		AwaitingReplyUntil: &awaitingReplyUntil,
	}))

	externalID := "lapsed-followup-" + uuid.NewString()
	createFollowUpTask(t, env, contact.ID, externalID)
	first := env.provider.reconcileContactTasks(env.ctx, nil, env.settings, env.accountID, false)
	assert.Empty(t, itemAddCommandsForContact(first, contact.ID))

	dismissal := env.provider.processItem(env.ctx, SyncItem{
		ID:        externalID,
		IsDeleted: true,
		Labels:    []string{env.settings.LabelName},
		Deadline:  &SyncDate{Date: "2099-01-01"},
	}, env.settings, env.accountID)
	require.NoError(t, dismissal.Err)

	today := cadence.Today(now).Format(DateFormat)
	contactByString := cadence.CalendarDate(contactBy).Format(DateFormat)
	second := env.provider.reconcileContactTasks(env.ctx, nil, env.settings, env.accountID, false)
	adds := itemAddCommandsForContact(second, contact.ID)
	// spec: CAD-042.lapsed-window-creates-only-without-live-followup
	require.Len(t, adds, 1)
	deadline, ok := adds[0].Args["deadline"].(map[string]string)
	require.True(t, ok, "item_add carries a deadline argument")
	// spec: CAD-042.resurrected-deadline-today-or-later
	assert.Equal(t, today, deadline["date"])

	cadenceTask, err := env.contactTaskRepo.GetContactTaskByContactCadenceDue(env.ctx, contact.ID, SourceName)
	require.NoError(t, err)
	assert.Equal(t, today, cadenceTask.Metadata[MetadataKeySyncedDeadline])
	assert.Equal(t, contactByString, cadenceTask.Metadata["synced_contact_by"])

	third := env.provider.reconcileContactTasks(env.ctx, nil, env.settings, env.accountID, false)
	assert.Empty(t, itemAddCommandsForContact(third, contact.ID), "pending temp ID prevents daily replacement churn")
	for _, command := range third {
		if command.Type == "item_close" && command.Args["id"] == cadenceTask.ExternalTaskID {
			assert.Fail(t, "unexpected close command for the contact's cadence task")
		}
	}
}

func TestHandleSkipTrigger_ReplacementSurvivesNextReconcile(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	contact := createSkipReconcileContact(t, env)
	require.False(t, cadence.IsAwaitingReply(
		contact.LastOutreachAt,
		contact.LastResponseAt,
		contact.AwaitingReplyUntil,
		accelerated.GetCurrentTime(),
	), "the closed reply window must let reconciliation reach the contact_by drift check")
	require.Nil(t, contact.AwaitingReplyUntil, "the response ended the reply window")
	require.NotNil(t, contact.LastOutreachAt)
	require.NotNil(t, contact.LastResponseAt)
	require.True(t, contact.LastResponseAt.After(*contact.LastOutreachAt), "the response must be strictly later than outreach")
	require.NotNil(t, contact.ContactBy)
	require.True(t, cadence.CalendarDate(*contact.ContactBy).After(cadence.CalendarDate(accelerated.GetCurrentTime())), "the fixture starts with a future contact_by")
	externalID := "skip-replacement-" + uuid.NewString()
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

	r := env.provider.handleSkipTrigger(env.ctx, SyncItem{ID: externalID, UpdatedAt: "2099-01-01T00:00:00Z"}, task, contact, env.settings, env.accountID)
	require.NoError(t, r.Err)
	require.Len(t, r.Commands, 1)
	require.Equal(t, "item_add", r.Commands[0].Type)
	deadline, ok := r.Commands[0].Args["deadline"].(map[string]string)
	require.True(t, ok)
	skippedTo := deadline["date"]

	rolledBack := env.provider.processTempIDMappings(env.ctx, map[string]string{r.Commands[0].TempID: "real-skip-replacement-id"})
	assert.False(t, rolledBack)

	commands := env.provider.reconcileContactTasks(env.ctx, nil, env.settings, env.accountID, false)
	assert.Empty(t, itemAddCommandsForContact(commands, contact.ID))
	updated, err := env.contactTaskRepo.GetContactTask(env.ctx, task.ID)
	require.NoError(t, err)
	require.Equal(t, "real-skip-replacement-id", updated.ExternalTaskID)
	assert.Equal(t, skippedTo, updated.Metadata[MetadataKeySyncedContactBy])
	for _, command := range commands {
		if command.Args["id"] != updated.ExternalTaskID {
			continue
		}
		assert.NotContains(t, []string{"item_close", "item_complete", "item_delete"}, command.Type,
			"reconciliation must not mutate the replacement task for this contact")
	}
}

func createSkipReconcileContact(t *testing.T, env *dismissalTestEnv) *repository.Contact {
	t.Helper()

	cadenceName := "monthly"
	contact, err := env.contactRepo.CreateContact(env.ctx, repository.CreateContactRequest{
		FullName: "Skip Reconcile " + uuid.NewString(),
		Cadence:  &cadenceName,
	})
	require.NoError(t, err)

	now := accelerated.GetCurrentTime().UTC().Truncate(time.Second)
	lastResponseAt := now.AddDate(0, 0, -2)
	lastOutreachAt := now.AddDate(0, 0, -3)
	lastContacted := lastResponseAt
	lastInteractionAt := lastResponseAt
	contactBy := cadence.Today(now).AddDate(0, 0, 20)
	require.NoError(t, env.contactRepo.TestSeedContactCadenceFields(env.ctx, contact.ID, repository.TestCadenceSeed{
		LastContacted:     &lastContacted,
		LastInteractionAt: &lastInteractionAt,
		LastResponseAt:    &lastResponseAt,
		LastOutreachAt:    &lastOutreachAt,
		ContactBy:         &contactBy,
	}))

	reloaded, err := env.contactRepo.GetContact(env.ctx, contact.ID)
	require.NoError(t, err)
	return reloaded
}

func itemAddCommandsForContact(commands []SyncCommand, contactID uuid.UUID) []SyncCommand {
	var matching []SyncCommand
	for _, command := range commands {
		if command.Type != "item_add" {
			continue
		}
		description, _ := command.Args["description"].(string)
		if strings.Contains(description, contactID.String()) {
			matching = append(matching, command)
		}
	}
	return matching
}
