package tests

import (
	"context"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupDirectionTestDeps(t *testing.T) (*service.ContactService, *repository.ContactRepository, *repository.ContactTaskRepository, func()) {
	t.Helper()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	// Migrations are applied once by TestMain.
	ctx := context.Background()
	dbConfig := config.DatabaseConfig{
		URL:               databaseURL,
		MaxConns:          8, // mirrors the lowered TestConfig() ceiling for parallel tests
		MinConns:          1,
		MaxConnIdleTime:   config.DefaultDBMaxConnIdleTime,
		MaxConnLifetime:   config.DefaultDBMaxConnLifetime,
		HealthCheckPeriod: config.DefaultDBHealthCheckPeriod,
	}
	database, err := db.NewDatabase(ctx, dbConfig)
	require.NoError(t, err)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactMethodRepo := repository.NewContactMethodRepository(database.Queries)
	interactionRepo := repository.NewInteractionRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	contactService := service.NewContactService(database, contactRepo, contactMethodRepo, interactionRepo, contactTaskRepo, nil, nil)
	wireCadenceUpdaterForTest(t, database, contactService)

	cleanup := func() {
		database.Close()
	}

	return contactService, contactRepo, contactTaskRepo, cleanup
}

func TestRecordInteraction_DirectionOutbound(t *testing.T) {
	contactService, contactRepo, _, cleanup := setupDirectionTestDeps(t)
	defer cleanup()
	ctx := context.Background()

	// Create a contact with cadence and a known last_contacted time
	cadence := "monthly"
	initialTime := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName:      "Direction Test Outbound",
		Cadence:       &cadence,
		LastContacted: &initialTime,
	})
	require.NoError(t, err)

	// Record an outbound interaction
	outboundTime := time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC)
	sourceRef := "test-outbound-" + contact.ID.String()
	interaction, err := contactService.RecordInteraction(ctx, repository.RecordInteractionRequest{
		ContactID:  contact.ID,
		Source:     repository.InteractionSourceTodoist,
		SourceRef:  &sourceRef,
		OccurredAt: outboundTime,
		Direction:  repository.InteractionDirectionOutbound,
	})
	require.NoError(t, err)
	assert.Equal(t, repository.InteractionDirectionOutbound, interaction.Direction)

	// Verify: last_contacted should NOT have changed (outbound doesn't reset cadence clock)
	updated, err := contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.NotNil(t, updated.LastContacted)
	assert.Equal(t, initialTime.UTC(), updated.LastContacted.UTC(), "outbound should not change last_contacted")

	// Verify: last_outreach_at should be updated
	assert.NotNil(t, updated.LastOutreachAt)
	assert.Equal(t, outboundTime.UTC(), updated.LastOutreachAt.UTC())

	// Verify: last_response_at and last_interaction_at should NOT be set by outbound
	// (New contacts created after migration 031 won't have backfilled values)
	assert.Nil(t, updated.LastResponseAt, "outbound should not set last_response_at")
	assert.Nil(t, updated.LastInteractionAt, "outbound should not set last_interaction_at")
}

func TestRecordInteraction_DirectionMutual(t *testing.T) {
	contactService, contactRepo, _, cleanup := setupDirectionTestDeps(t)
	defer cleanup()
	ctx := context.Background()

	cadence := "monthly"
	initialTime := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName:      "Direction Test Mutual",
		Cadence:       &cadence,
		LastContacted: &initialTime,
	})
	require.NoError(t, err)

	// Record a mutual interaction
	mutualTime := time.Date(2026, 3, 20, 10, 0, 0, 0, time.UTC)
	sourceRef := "test-mutual-" + contact.ID.String()
	interaction, err := contactService.RecordInteraction(ctx, repository.RecordInteractionRequest{
		ContactID:  contact.ID,
		Source:     repository.InteractionSourceManual,
		SourceRef:  &sourceRef,
		OccurredAt: mutualTime,
		Direction:  repository.InteractionDirectionMutual,
	})
	require.NoError(t, err)
	assert.Equal(t, repository.InteractionDirectionMutual, interaction.Direction)

	// Verify: all fields should be updated
	updated, err := contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.NotNil(t, updated.LastContacted)
	assert.Equal(t, mutualTime.UTC(), updated.LastContacted.UTC(), "mutual should update last_contacted")
	assert.NotNil(t, updated.LastOutreachAt)
	assert.Equal(t, mutualTime.UTC(), updated.LastOutreachAt.UTC())
	assert.NotNil(t, updated.LastResponseAt)
	assert.Equal(t, mutualTime.UTC(), updated.LastResponseAt.UTC())
	assert.NotNil(t, updated.LastInteractionAt)
	assert.Equal(t, mutualTime.UTC(), updated.LastInteractionAt.UTC())
	// contact_by should be recalculated
	assert.NotNil(t, updated.ContactBy)
}

func TestRecordInteraction_DirectionInbound(t *testing.T) {
	contactService, contactRepo, _, cleanup := setupDirectionTestDeps(t)
	defer cleanup()
	ctx := context.Background()

	cadence := "monthly"
	initialTime := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName:      "Direction Test Inbound",
		Cadence:       &cadence,
		LastContacted: &initialTime,
	})
	require.NoError(t, err)

	// Record an inbound interaction
	inboundTime := time.Date(2026, 3, 25, 10, 0, 0, 0, time.UTC)
	sourceRef := "test-inbound-" + contact.ID.String()
	interaction, err := contactService.RecordInteraction(ctx, repository.RecordInteractionRequest{
		ContactID:  contact.ID,
		Source:     repository.InteractionSourceTodoist,
		SourceRef:  &sourceRef,
		OccurredAt: inboundTime,
		Direction:  repository.InteractionDirectionInbound,
	})
	require.NoError(t, err)
	assert.Equal(t, repository.InteractionDirectionInbound, interaction.Direction)

	// Verify: last_contacted should be updated (inbound resets cadence clock)
	updated, err := contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	assert.NotNil(t, updated.LastContacted)
	assert.Equal(t, inboundTime.UTC(), updated.LastContacted.UTC(), "inbound should update last_contacted")
	// last_outreach_at should NOT be set by inbound
	assert.Nil(t, updated.LastOutreachAt, "inbound should not set last_outreach_at")
	// last_response_at should be updated
	assert.NotNil(t, updated.LastResponseAt)
	assert.Equal(t, inboundTime.UTC(), updated.LastResponseAt.UTC())
}

func TestRecordInteraction_EmptyDirectionDefaultsMutual(t *testing.T) {
	contactService, contactRepo, _, cleanup := setupDirectionTestDeps(t)
	defer cleanup()
	ctx := context.Background()

	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Direction Test Default",
	})
	require.NoError(t, err)

	// Record interaction with no direction (backward compat)
	sourceRef := "test-default-" + contact.ID.String()
	interaction, err := contactService.RecordInteraction(ctx, repository.RecordInteractionRequest{
		ContactID:  contact.ID,
		Source:     repository.InteractionSourceManual,
		SourceRef:  &sourceRef,
		OccurredAt: time.Date(2026, 4, 5, 10, 0, 0, 0, time.UTC),
		// Direction intentionally empty
	})
	require.NoError(t, err)
	assert.Equal(t, repository.InteractionDirectionMutual, interaction.Direction, "empty direction should default to mutual")
}

func TestRecordInteraction_ForwardOnlyGuard_ContactByNotRegressed(t *testing.T) {
	contactService, contactRepo, _, cleanup := setupDirectionTestDeps(t)
	defer cleanup()
	ctx := context.Background()

	cadence := "monthly"
	// Start with a recent last_contacted
	recentTime := time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC)
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName:      "Forward Only Guard Test",
		Cadence:       &cadence,
		LastContacted: &recentTime,
	})
	require.NoError(t, err)

	// Record a mutual interaction to set contact_by
	sourceRef := "test-recent-" + contact.ID.String()
	_, err = contactService.RecordInteraction(ctx, repository.RecordInteractionRequest{
		ContactID:  contact.ID,
		Source:     repository.InteractionSourceManual,
		SourceRef:  &sourceRef,
		OccurredAt: recentTime,
		Direction:  repository.InteractionDirectionMutual,
	})
	require.NoError(t, err)

	// Get the contact_by that was set
	contactAfterRecent, err := contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, contactAfterRecent.ContactBy)
	recentContactBy := *contactAfterRecent.ContactBy

	// Now record a late-arriving automated inbound event with an OLDER timestamp
	oldTime := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	oldSourceRef := "test-old-" + contact.ID.String()
	_, err = contactService.RecordInteraction(ctx, repository.RecordInteractionRequest{
		ContactID:  contact.ID,
		Source:     repository.InteractionSourceTodoist, // automated source
		SourceRef:  &oldSourceRef,
		OccurredAt: oldTime,
		Direction:  repository.InteractionDirectionInbound,
	})
	require.NoError(t, err)

	// Verify: contact_by should NOT have regressed
	contactAfterOld, err := contactRepo.GetContact(ctx, contact.ID)
	require.NoError(t, err)
	require.NotNil(t, contactAfterOld.ContactBy)
	assert.False(t, contactAfterOld.ContactBy.Before(recentContactBy),
		"contact_by should not regress from late-arriving automated event (got %v, want >= %v)",
		*contactAfterOld.ContactBy, recentContactBy)
}

func TestHasPendingFollowUp(t *testing.T) {
	contactService, contactRepo, contactTaskRepo, cleanup := setupDirectionTestDeps(t)
	defer cleanup()
	ctx := context.Background()

	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Follow-Up Pending Test",
	})
	require.NoError(t, err)

	// Initially no pending follow-up
	hasPending, err := contactService.HasPendingFollowUp(ctx, contact.ID)
	require.NoError(t, err)
	assert.False(t, hasPending)

	// Create a managed follow-up task
	_, err = contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
		ContactID:      contact.ID,
		Provider:       "todoist",
		Kind:           contacttask.KindReachOut,
		Lifecycle:      contacttask.LifecycleFollowUpLoop,
		ExternalTaskID: "test-followup-" + contact.ID.String(),
		State:          "managed",
	})
	require.NoError(t, err)

	// Now should have pending follow-up
	hasPending, err = contactService.HasPendingFollowUp(ctx, contact.ID)
	require.NoError(t, err)
	assert.True(t, hasPending)

	// Complete the follow-up
	_, err = contactTaskRepo.CompleteFollowUpForContact(ctx, contact.ID)
	require.NoError(t, err)

	// No longer pending
	hasPending, err = contactService.HasPendingFollowUp(ctx, contact.ID)
	require.NoError(t, err)
	assert.False(t, hasPending)
}

func TestFollowupFilter(t *testing.T) {
	_, contactRepo, contactTaskRepo, cleanup := setupDirectionTestDeps(t)
	defer cleanup()
	ctx := context.Background()

	// Create two contacts
	contactWithFollowup, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Has Followup Filter Test",
	})
	require.NoError(t, err)

	contactWithoutFollowup, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "No Followup Filter Test",
	})
	require.NoError(t, err)

	// Give one a pending follow-up
	_, err = contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
		ContactID:      contactWithFollowup.ID,
		Provider:       "todoist",
		Kind:           contacttask.KindReachOut,
		Lifecycle:      contacttask.LifecycleFollowUpLoop,
		ExternalTaskID: "test-filter-" + contactWithFollowup.ID.String(),
		State:          "managed",
	})
	require.NoError(t, err)

	// Filter: has_followup
	contactsWithFollowup, err := contactRepo.ListContacts(ctx, repository.ListContactsParams{
		Limit:          100,
		FollowupFilter: "has_followup",
	})
	require.NoError(t, err)
	foundWith := false
	foundWithout := false
	for _, c := range contactsWithFollowup {
		if c.ID == contactWithFollowup.ID {
			foundWith = true
		}
		if c.ID == contactWithoutFollowup.ID {
			foundWithout = true
		}
	}
	assert.True(t, foundWith, "contact with follow-up should appear in has_followup filter")
	assert.False(t, foundWithout, "contact without follow-up should NOT appear in has_followup filter")

	// Filter: no_followup
	contactsWithout, err := contactRepo.ListContacts(ctx, repository.ListContactsParams{
		Limit:          100,
		FollowupFilter: "no_followup",
	})
	require.NoError(t, err)
	foundWith = false
	foundWithout = false
	for _, c := range contactsWithout {
		if c.ID == contactWithFollowup.ID {
			foundWith = true
		}
		if c.ID == contactWithoutFollowup.ID {
			foundWithout = true
		}
	}
	assert.False(t, foundWith, "contact with follow-up should NOT appear in no_followup filter")
	assert.True(t, foundWithout, "contact without follow-up should appear in no_followup filter")
}

func TestCompletedCadenceTask_CanBeReplacedByNewOne(t *testing.T) {
	_, contactRepo, contactTaskRepo, cleanup := setupDirectionTestDeps(t)
	defer cleanup()
	ctx := context.Background()

	cadence := "monthly"
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Cadence Replacement Test",
		Cadence:  &cadence,
	})
	require.NoError(t, err)

	// Simulate the handleTaskCompletion flow: create a cadence task and mark it completed
	originalTask, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
		ContactID:      contact.ID,
		Provider:       "todoist",
		Kind:           contacttask.KindReachOut,
		Lifecycle:      contacttask.LifecycleCadenceDue,
		ExternalTaskID: "original-cadence-task",
		State:          "managed",
	})
	require.NoError(t, err)

	_, err = contactTaskRepo.UpdateContactTaskState(ctx, originalTask.ID, repository.ContactTaskStateCompleted)
	require.NoError(t, err)

	// Verify: GetContactTaskByContact finds the completed task
	found, err := contactTaskRepo.GetContactTaskByContactCadenceDue(ctx, contact.ID, "todoist")
	require.NoError(t, err)
	assert.Equal(t, repository.ContactTaskStateCompleted, found.State)

	// Simulate what reconcileContactTasks now does: delete the completed task
	err = contactTaskRepo.DeleteContactTask(ctx, found.ID)
	require.NoError(t, err)

	// Verify: can create a new cadence task (no unique constraint violation)
	newTask, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
		ContactID:      contact.ID,
		Provider:       "todoist",
		Kind:           contacttask.KindReachOut,
		Lifecycle:      contacttask.LifecycleCadenceDue,
		ExternalTaskID: "replacement-cadence-task",
		State:          "managed",
	})
	require.NoError(t, err)
	assert.Equal(t, repository.ContactTaskStateManaged, newTask.State)
	assert.Equal(t, "replacement-cadence-task", newTask.ExternalTaskID)

	// Verify: GetContactTaskByContact now returns the new managed task
	current, err := contactTaskRepo.GetContactTaskByContactCadenceDue(ctx, contact.ID, "todoist")
	require.NoError(t, err)
	assert.Equal(t, newTask.ID, current.ID)
	assert.Equal(t, repository.ContactTaskStateManaged, current.State)
}
