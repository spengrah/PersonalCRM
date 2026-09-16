package todoist

import (
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func skipReconcileUTCDate(base time.Time, offset int) time.Time {
	return time.Date(base.Year(), base.Month(), base.Day()+offset, 0, 0, 0, 0, time.UTC)
}

func TestReconcile_AfterSkip_RedatesCadenceTaskToSkippedToDate(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := setupDismissalTest(t)
	defer cleanup()
	cadenceName := "monthly"
	contact, err := env.contactRepo.CreateContact(env.ctx, repository.CreateContactRequest{FullName: "Reconcile After Skip", Cadence: &cadenceName})
	require.NoError(t, err)
	now := accelerated.GetCurrentTime()
	today := cadence.Today(now)
	contactBy := skipReconcileUTCDate(today, 30)
	outreach := now.AddDate(0, 0, -2)
	response := now.AddDate(0, 0, -1)
	require.NoError(t, env.contactRepo.TestSeedContactCadenceFields(env.ctx, contact.ID, repository.TestCadenceSeed{ContactBy: &contactBy, LastOutreachAt: &outreach, LastResponseAt: &response}))
	followup := createFollowUpTask(t, env, contact.ID, "td-followup-"+uuid.NewString())
	_, err = env.contactTaskRepo.UpdateContactTaskState(env.ctx, followup.ID, repository.ContactTaskStateCompleted)
	require.NoError(t, err)
	oldExternal := "td-old-" + uuid.NewString()
	oldDate := skipReconcileUTCDate(today, -10)
	createCadenceTask(t, env, contact.ID, oldExternal, map[string]any{MetadataKeySyncedDeadline: oldDate.Format(DateFormat), MetadataKeySyncedContactBy: oldDate.Format(DateFormat)})
	commands := env.provider.reconcileContactTasks(env.ctx, nil, env.settings, env.accountID, false)
	adds := itemAddCommandsForContact(commands, contact.ID)
	require.Len(t, adds, 1)
	deadline := adds[0].Args["deadline"].(map[string]string)
	require.Equal(t, contactBy.Format(DateFormat), deadline["date"])
	var closed bool
	for _, command := range commands {
		if command.Type == "item_close" && command.Args["id"] == oldExternal {
			closed = true
		}
	}
	require.True(t, closed)
	env.provider.processTempIDMappings(env.ctx, map[string]string{adds[0].TempID: "td-new-" + uuid.NewString()})
	_, err = env.contactTaskRepo.GetContactTaskByContactFollowUpLive(env.ctx, contact.ID, SourceName)
	require.Error(t, err)
	second := env.provider.reconcileContactTasks(env.ctx, nil, env.settings, env.accountID, false)
	require.Empty(t, itemAddCommandsForContact(second, contact.ID))
}

func TestReconcile_AfterUndoPastDate_ClampsToToday(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	env, cleanup := setupDismissalTest(t)
	defer cleanup()
	cadenceName := "monthly"
	contact, err := env.contactRepo.CreateContact(env.ctx, repository.CreateContactRequest{FullName: "Reconcile After Undo", Cadence: &cadenceName})
	require.NoError(t, err)
	now := accelerated.GetCurrentTime()
	today := cadence.Today(now)
	contactBy := skipReconcileUTCDate(today, -10)
	outreach := now.AddDate(0, 0, -2)
	response := now.AddDate(0, 0, -1)
	require.NoError(t, env.contactRepo.TestSeedContactCadenceFields(env.ctx, contact.ID, repository.TestCadenceSeed{ContactBy: &contactBy, LastOutreachAt: &outreach, LastResponseAt: &response}))
	oldExternal := "td-old-" + uuid.NewString()
	futureDate := skipReconcileUTCDate(today, 30)
	createCadenceTask(t, env, contact.ID, oldExternal, map[string]any{MetadataKeySyncedDeadline: futureDate.Format(DateFormat), MetadataKeySyncedContactBy: futureDate.Format(DateFormat)})
	commands := env.provider.reconcileContactTasks(env.ctx, nil, env.settings, env.accountID, false)
	adds := itemAddCommandsForContact(commands, contact.ID)
	require.Len(t, adds, 1)
	deadline := adds[0].Args["deadline"].(map[string]string)
	require.Equal(t, today.Format(DateFormat), deadline["date"])
	updated, err := env.contactTaskRepo.GetContactTaskByContactCadenceDue(env.ctx, contact.ID, SourceName)
	require.NoError(t, err)
	require.Equal(t, contactBy.Format(DateFormat), updated.Metadata[MetadataKeySyncedContactBy])
}
