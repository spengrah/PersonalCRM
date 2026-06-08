// Package tests contains integration tests for contact_task functionality.
//
// These tests verify the contact_task table operations used by the
// Todoist cadence sync feature.
package tests

import (
	"context"
	"os"
	"strings"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestContactTask_CRUD verifies basic CRUD operations for contact_task
func TestContactTask_CRUD(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	defer database.Close()

	contactRepo := repository.NewContactRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	gen, _ := migrationGenerator(t)

	// Seed a test contact via the synthetic factory (FK target for the tasks).
	contact, contactCleanup := seedMigrationContact(ctx, t, database, gen, factory.WithCadence("weekly"))
	defer contactCleanup()

	t.Run("create and retrieve contact task", func(t *testing.T) {
		// Create task
		task, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           contacttask.KindReachOut,
			Lifecycle:      contacttask.LifecycleCadenceDue,
			ExternalTaskID: "12345",
			State:          "managed",
			Metadata:       map[string]any{"test_key": "test_value"},
		})
		require.NoError(t, err)
		assert.Equal(t, contact.ID, task.ContactID)
		assert.Equal(t, "todoist", task.Provider)
		assert.Equal(t, contacttask.KindReachOut, task.Kind)
		assert.Equal(t, "12345", task.ExternalTaskID)
		assert.Equal(t, repository.ContactTaskStateManaged, task.State)

		// Retrieve by ID
		retrieved, err := contactTaskRepo.GetContactTask(ctx, task.ID)
		require.NoError(t, err)
		assert.Equal(t, task.ID, retrieved.ID)
		assert.Equal(t, "test_value", retrieved.Metadata["test_key"])

		// Retrieve by contact+provider+kind
		retrieved2, err := contactTaskRepo.GetContactTaskByContactCadenceDue(ctx, contact.ID, "todoist")
		require.NoError(t, err)
		assert.Equal(t, task.ID, retrieved2.ID)

		// Retrieve by external ID
		retrieved3, err := contactTaskRepo.GetContactTaskByExternalID(ctx, "todoist", "12345")
		require.NoError(t, err)
		assert.Equal(t, task.ID, retrieved3.ID)

		// Clean up
		err = contactTaskRepo.DeleteContactTask(ctx, task.ID)
		require.NoError(t, err)
	})

	t.Run("upsert contact task", func(t *testing.T) {
		// Create via upsert
		task1, err := contactTaskRepo.UpsertContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           contacttask.KindReachOut,
			Lifecycle:      contacttask.LifecycleCadenceDue,
			ExternalTaskID: "11111",
			State:          string(repository.ContactTaskStateManaged),
		})
		require.NoError(t, err)
		assert.Equal(t, "11111", task1.ExternalTaskID)
		assert.Equal(t, repository.ContactTaskStateManaged, task1.State)

		// Upsert again with same external_task_id (should update state)
		task2, err := contactTaskRepo.UpsertContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           contacttask.KindReachOut,
			Lifecycle:      contacttask.LifecycleCadenceDue,
			ExternalTaskID: "11111", // Same external ID
			State:          string(repository.ContactTaskStateUnmanaged),
		})
		require.NoError(t, err)
		assert.Equal(t, task1.ID, task2.ID)                                // Same ID (upsert matched)
		assert.Equal(t, "11111", task2.ExternalTaskID)                     // Same external ID (it's the key)
		assert.Equal(t, repository.ContactTaskStateUnmanaged, task2.State) // State was updated

		// Clean up
		err = contactTaskRepo.DeleteContactTask(ctx, task1.ID)
		require.NoError(t, err)
	})

	t.Run("update contact task state", func(t *testing.T) {
		// Create task
		task, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           contacttask.KindReachOut,
			Lifecycle:      contacttask.LifecycleCadenceDue,
			ExternalTaskID: "33333",
		})
		require.NoError(t, err)
		assert.Equal(t, repository.ContactTaskStateManaged, task.State)

		// Update state to unmanaged
		updated, err := contactTaskRepo.UpdateContactTaskState(ctx, task.ID, repository.ContactTaskStateUnmanaged)
		require.NoError(t, err)
		assert.Equal(t, repository.ContactTaskStateUnmanaged, updated.State)

		// Clean up
		err = contactTaskRepo.DeleteContactTask(ctx, task.ID)
		require.NoError(t, err)
	})

	t.Run("list contact tasks by provider", func(t *testing.T) {
		// Create a task
		task, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           contacttask.KindReachOut,
			Lifecycle:      contacttask.LifecycleCadenceDue,
			ExternalTaskID: "44444",
		})
		require.NoError(t, err)

		// List all tasks for provider
		tasks, err := contactTaskRepo.ListContactTasksByProvider(ctx, "todoist", nil)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, len(tasks), 1)

		// List managed tasks only
		managedState := "managed"
		managedTasks, err := contactTaskRepo.ListContactTasksByProvider(ctx, "todoist", &managedState)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, len(managedTasks), 1)

		// Clean up
		err = contactTaskRepo.DeleteContactTask(ctx, task.ID)
		require.NoError(t, err)
	})

	t.Run("list managed contact tasks with contact info", func(t *testing.T) {
		// Create a task
		task, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           contacttask.KindReachOut,
			Lifecycle:      contacttask.LifecycleCadenceDue,
			ExternalTaskID: "55555",
		})
		require.NoError(t, err)

		// List managed tasks with contact info
		tasksWithContact, err := contactTaskRepo.ListManagedContactTasks(ctx, "todoist")
		require.NoError(t, err)

		// Find our task
		var found *repository.ContactTaskWithContact
		for i, tc := range tasksWithContact {
			if tc.ID == task.ID {
				found = &tasksWithContact[i]
				break
			}
		}
		require.NotNil(t, found, "Task should be in list")
		assert.Equal(t, contact.FullName, found.FullName)
		assert.Equal(t, "weekly", *found.Cadence)

		// Clean up
		err = contactTaskRepo.DeleteContactTask(ctx, task.ID)
		require.NoError(t, err)
	})

	t.Run("delete by contact+provider+kind", func(t *testing.T) {
		// Create a task
		_, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           contacttask.KindReachOut,
			Lifecycle:      contacttask.LifecycleCadenceDue,
			ExternalTaskID: "66666",
		})
		require.NoError(t, err)

		// Delete by contact+provider+kind
		err = contactTaskRepo.DeleteContactTaskByContactCadenceDue(ctx, contact.ID, "todoist")
		require.NoError(t, err)

		// Verify deleted
		_, err = contactTaskRepo.GetContactTaskByContactCadenceDue(ctx, contact.ID, "todoist")
		assert.ErrorIs(t, err, db.ErrNotFound)
	})

	t.Run("cascade delete when contact is deleted", func(t *testing.T) {
		// Seed a fresh contact whose hard-delete must cascade to its task.
		tempContact, _ := seedMigrationContact(ctx, t, database, gen)

		// Create a task for this contact
		task, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      tempContact.ID,
			Provider:       "todoist",
			Kind:           contacttask.KindReachOut,
			Lifecycle:      contacttask.LifecycleCadenceDue,
			ExternalTaskID: "77777",
		})
		require.NoError(t, err)

		// Hard delete the contact (should cascade to contact_task)
		err = contactRepo.HardDeleteContact(ctx, tempContact.ID)
		require.NoError(t, err)

		// Verify task is also deleted (CASCADE)
		_, err = contactTaskRepo.GetContactTask(ctx, task.ID)
		assert.ErrorIs(t, err, db.ErrNotFound)
	})

	t.Run("unique constraint on contact+provider+kind", func(t *testing.T) {
		// Create first task
		_, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           contacttask.KindReachOut,
			Lifecycle:      contacttask.LifecycleCadenceDue,
			ExternalTaskID: "88888",
		})
		require.NoError(t, err)

		// Try to create duplicate (should fail)
		_, err = contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           contacttask.KindReachOut,
			Lifecycle:      contacttask.LifecycleCadenceDue,
			ExternalTaskID: "99999",
		})
		assert.Error(t, err) // Should fail due to unique constraint

		// Clean up
		err = contactTaskRepo.DeleteContactTaskByContactCadenceDue(ctx, contact.ID, "todoist")
		require.NoError(t, err)
	})
}

// TestContactTask_CountByProvider verifies counting tasks by provider and state
func TestContactTask_CountByProvider(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	defer database.Close()

	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	gen, _ := migrationGenerator(t)

	// Seed test contacts via the synthetic factory (FK targets only).
	var contacts []uuid.UUID
	for i := 0; i < 3; i++ {
		c, cleanup := seedMigrationContact(ctx, t, database, gen, factory.WithCadence("weekly"))
		defer cleanup()
		contacts = append(contacts, c.ID)
	}

	// Create tasks for each contact
	for i, contactID := range contacts {
		state := "managed"
		if i == 2 {
			state = "unmanaged"
		}
		_, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contactID,
			Provider:       "todoist",
			Kind:           contacttask.KindReachOut,
			Lifecycle:      contacttask.LifecycleCadenceDue,
			ExternalTaskID: uuid.New().String(),
			State:          state,
		})
		require.NoError(t, err)
	}
	defer func() {
		_ = contactTaskRepo.DeleteContactTasksByProvider(ctx, "todoist")
	}()

	// Count managed tasks
	managedCount, err := contactTaskRepo.CountContactTasksByProvider(ctx, "todoist", "managed")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, managedCount, int64(2))

	// Count unmanaged tasks
	unmanagedCount, err := contactTaskRepo.CountContactTasksByProvider(ctx, "todoist", "unmanaged")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, unmanagedCount, int64(1))
}

// TestContactTask_SyncedDeadlineMetadata verifies the synced_deadline metadata
// behavior used for detecting when contact_by drifts from the Todoist task deadline.
func TestContactTask_SyncedDeadlineMetadata(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	defer database.Close()

	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	gen, _ := migrationGenerator(t)

	// Seed a test contact via the synthetic factory (FK target for the tasks).
	contact, contactCleanup := seedMigrationContact(ctx, t, database, gen, factory.WithCadence("weekly"))
	defer contactCleanup()

	t.Run("create task with synced_deadline metadata", func(t *testing.T) {
		// Create task with synced_deadline in metadata (simulating what reconciliation does)
		task, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           contacttask.KindReachOut,
			Lifecycle:      contacttask.LifecycleCadenceDue,
			ExternalTaskID: "temp-uuid-123",
			State:          "managed",
			Metadata: map[string]any{
				"pending_temp_id": "temp-uuid-123",
				"synced_deadline": "2026-02-15",
			},
		})
		require.NoError(t, err)
		assert.Equal(t, "temp-uuid-123", task.Metadata["pending_temp_id"])
		assert.Equal(t, "2026-02-15", task.Metadata["synced_deadline"])

		// Update metadata to simulate temp_id resolution (clear pending, keep synced_deadline)
		metadata := task.Metadata
		delete(metadata, "pending_temp_id")
		updated, err := contactTaskRepo.UpdateContactTaskMetadata(ctx, task.ID, metadata)
		require.NoError(t, err)
		assert.Nil(t, updated.Metadata["pending_temp_id"])
		assert.Equal(t, "2026-02-15", updated.Metadata["synced_deadline"])

		// Update synced_deadline when deadline changes (simulating reconciliation update)
		metadata["synced_deadline"] = "2026-03-15"
		metadata["pending_temp_id"] = "new-temp-uuid"
		updated2, err := contactTaskRepo.UpdateContactTaskMetadata(ctx, task.ID, metadata)
		require.NoError(t, err)
		assert.Equal(t, "2026-03-15", updated2.Metadata["synced_deadline"])
		assert.Equal(t, "new-temp-uuid", updated2.Metadata["pending_temp_id"])

		// Clean up
		err = contactTaskRepo.DeleteContactTask(ctx, task.ID)
		require.NoError(t, err)
	})

	t.Run("backfill synced_deadline for existing task", func(t *testing.T) {
		// Create task without synced_deadline (simulating pre-existing task)
		task, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           contacttask.KindReachOut,
			Lifecycle:      contacttask.LifecycleCadenceDue,
			ExternalTaskID: "12345678",
			State:          "managed",
			Metadata:       map[string]any{}, // No synced_deadline
		})
		require.NoError(t, err)
		assert.Nil(t, task.Metadata["synced_deadline"])

		// Backfill synced_deadline (simulating what reconciliation does)
		metadata := task.Metadata
		if metadata == nil {
			metadata = make(map[string]any)
		}
		metadata["synced_deadline"] = "2026-02-15"
		updated, err := contactTaskRepo.UpdateContactTaskMetadata(ctx, task.ID, metadata)
		require.NoError(t, err)
		assert.Equal(t, "2026-02-15", updated.Metadata["synced_deadline"])

		// Clean up
		err = contactTaskRepo.DeleteContactTask(ctx, task.ID)
		require.NoError(t, err)
	})
}

// TestContactTask_ActionTasks verifies action task operations
func TestContactTask_ActionTasks(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL

	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	defer database.Close()

	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	gen, _ := migrationGenerator(t)

	// Seed a test contact via the synthetic factory (FK target for the tasks).
	contact, contactCleanup := seedMigrationContact(ctx, t, database, gen)
	defer contactCleanup()

	t.Run("multiple action tasks per contact allowed", func(t *testing.T) {
		// Create first action task
		task1, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           contacttask.KindAction,
			Lifecycle:      contacttask.LifecycleManual,
			ExternalTaskID: "action-task-1",
			State:          "managed",
			Metadata: map[string]any{
				"content":  "Follow up about surgery",
				"due_date": "2026-02-10",
			},
		})
		require.NoError(t, err)
		assert.Equal(t, "action", task1.Kind)

		// Create second action task - should NOT fail (unlike cadence tasks)
		task2, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           contacttask.KindAction,
			Lifecycle:      contacttask.LifecycleManual,
			ExternalTaskID: "action-task-2",
			State:          "managed",
			Metadata: map[string]any{
				"content":  "Send contract",
				"due_date": "2026-02-15",
			},
		})
		require.NoError(t, err)
		assert.Equal(t, "action", task2.Kind)
		assert.NotEqual(t, task1.ID, task2.ID)

		// Create third action task
		task3, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           contacttask.KindAction,
			Lifecycle:      contacttask.LifecycleManual,
			ExternalTaskID: "action-task-3",
			State:          "managed",
			Metadata: map[string]any{
				"content": "No due date task",
			},
		})
		require.NoError(t, err)

		// Clean up
		err = contactTaskRepo.DeleteContactTask(ctx, task1.ID)
		require.NoError(t, err)
		err = contactTaskRepo.DeleteContactTask(ctx, task2.ID)
		require.NoError(t, err)
		err = contactTaskRepo.DeleteContactTask(ctx, task3.ID)
		require.NoError(t, err)
	})

	t.Run("list tasks with filters", func(t *testing.T) {
		// Create mix of tasks
		actionManaged, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           contacttask.KindAction,
			Lifecycle:      contacttask.LifecycleManual,
			ExternalTaskID: "action-managed",
			State:          "managed",
		})
		require.NoError(t, err)

		actionCompleted, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           contacttask.KindAction,
			Lifecycle:      contacttask.LifecycleManual,
			ExternalTaskID: "action-completed",
			State:          "completed",
		})
		require.NoError(t, err)

		cadenceManaged, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           contacttask.KindReachOut,
			Lifecycle:      contacttask.LifecycleCadenceDue,
			ExternalTaskID: "cadence-managed",
			State:          "managed",
		})
		require.NoError(t, err)

		// List all tasks for contact
		allTasks, err := contactTaskRepo.ListContactTasksByContact(ctx, contact.ID)
		require.NoError(t, err)
		assert.Len(t, allTasks, 3)

		// Filter by state=managed
		managed := "managed"
		managedTasks, err := contactTaskRepo.ListContactTasksFiltered(ctx, contact.ID, &managed, nil, nil)
		require.NoError(t, err)
		assert.Len(t, managedTasks, 2) // action-managed and cadence-managed

		// Filter by state=completed
		completed := "completed"
		completedTasks, err := contactTaskRepo.ListContactTasksFiltered(ctx, contact.ID, &completed, nil, nil)
		require.NoError(t, err)
		assert.Len(t, completedTasks, 1)
		assert.Equal(t, "action-completed", completedTasks[0].ExternalTaskID)

		// Filter by kind=action
		action := "action"
		actionTasks, err := contactTaskRepo.ListContactTasksFiltered(ctx, contact.ID, nil, &action, nil)
		require.NoError(t, err)
		assert.Len(t, actionTasks, 2) // action-managed and action-completed

		// Filter by kind=action AND state=managed
		actionManagedTasks, err := contactTaskRepo.ListContactTasksFiltered(ctx, contact.ID, &managed, &action, nil)
		require.NoError(t, err)
		assert.Len(t, actionManagedTasks, 1)
		assert.Equal(t, "action-managed", actionManagedTasks[0].ExternalTaskID)

		// Clean up
		err = contactTaskRepo.DeleteContactTask(ctx, actionManaged.ID)
		require.NoError(t, err)
		err = contactTaskRepo.DeleteContactTask(ctx, actionCompleted.ID)
		require.NoError(t, err)
		err = contactTaskRepo.DeleteContactTask(ctx, cadenceManaged.ID)
		require.NoError(t, err)
	})

	t.Run("completed state transition", func(t *testing.T) {
		// Create managed action task
		task, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           contacttask.KindAction,
			Lifecycle:      contacttask.LifecycleManual,
			ExternalTaskID: "task-to-complete",
			State:          "managed",
		})
		require.NoError(t, err)
		assert.Equal(t, repository.ContactTaskStateManaged, task.State)

		// Transition to completed
		updated, err := contactTaskRepo.UpdateContactTaskState(ctx, task.ID, repository.ContactTaskStateCompleted)
		require.NoError(t, err)
		assert.Equal(t, repository.ContactTaskStateCompleted, updated.State)

		// Verify retrieval shows completed state
		retrieved, err := contactTaskRepo.GetContactTask(ctx, task.ID)
		require.NoError(t, err)
		assert.Equal(t, repository.ContactTaskStateCompleted, retrieved.State)

		// Clean up
		err = contactTaskRepo.DeleteContactTask(ctx, task.ID)
		require.NoError(t, err)
	})

	t.Run("action task metadata schema", func(t *testing.T) {
		// Create action task with full metadata
		task, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           contacttask.KindAction,
			Lifecycle:      contacttask.LifecycleManual,
			ExternalTaskID: "action-with-metadata",
			State:          "managed",
			Metadata: map[string]any{
				"content":    "Follow up about surgery",
				"due_date":   "2026-02-10",
				"project_id": "123456",
			},
		})
		require.NoError(t, err)

		// Verify metadata structure
		assert.Equal(t, "Follow up about surgery", task.Metadata["content"])
		assert.Equal(t, "2026-02-10", task.Metadata["due_date"])
		assert.Equal(t, "123456", task.Metadata["project_id"])

		// Update metadata (e.g., when Todoist task is edited)
		newMetadata := map[string]any{
			"content":    "Follow up about knee surgery",
			"due_date":   "2026-02-12",
			"project_id": "123456",
		}
		updated, err := contactTaskRepo.UpdateContactTaskMetadata(ctx, task.ID, newMetadata)
		require.NoError(t, err)
		assert.Equal(t, "Follow up about knee surgery", updated.Metadata["content"])
		assert.Equal(t, "2026-02-12", updated.Metadata["due_date"])

		// Clean up
		err = contactTaskRepo.DeleteContactTask(ctx, task.ID)
		require.NoError(t, err)
	})
}

// TestContactTask_Migration046_CheckConstraints verifies the post-migration
// CHECK constraints reject every invalid (kind, lifecycle, kind enum,
// lifecycle enum) combination at insert time. This is the migration
// regression-guard: if a code path ever passes an invalid pair through
// CreateContactTaskRequest, the DB rejects with the named CHECK
// constraint and the error propagates out cleanly. We assert via the
// pgconn.PgError ConstraintName so a future schema change that drops
// or renames a CHECK is caught loudly rather than silently letting
// invalid rows land.
func TestContactTask_Migration046_CheckConstraints(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	defer database.Close()

	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	gen, ns := migrationGenerator(t)

	contact, contactCleanup := seedMigrationContact(ctx, t, database, gen, factory.WithCadence("weekly"))
	t.Cleanup(contactCleanup)

	// Composite CHECK rejects every invalid (kind, lifecycle) pair.
	// Only kind=reach_out participates in all three lifecycles; every
	// other kind requires lifecycle=manual.
	invalidPairs := []struct {
		name      string
		kind      string
		lifecycle string
	}{
		{"send_cadence_due", "send", "cadence_due"},
		{"send_followup_loop", "send", "followup_loop"},
		{"reminder_cadence_due", "reminder", "cadence_due"},
		{"reminder_followup_loop", "reminder", "followup_loop"},
		{"meet_cadence_due", "meet", "cadence_due"},
		{"meet_followup_loop", "meet", "followup_loop"},
		{"action_cadence_due", "action", "cadence_due"},
		{"action_followup_loop", "action", "followup_loop"},
	}
	for _, c := range invalidPairs {
		t.Run("composite_check_rejects_"+c.name, func(t *testing.T) {
			_, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
				ContactID:      contact.ID,
				Provider:       "todoist",
				Kind:           c.kind,
				Lifecycle:      c.lifecycle,
				ExternalTaskID: "check-pair-" + c.name + "-" + ns,
				State:          "managed",
			})
			require.Error(t, err, "(%s, %s) MUST be rejected by composite CHECK", c.kind, c.lifecycle)
			require.Contains(t, err.Error(), "contact_task_kind_lifecycle_check",
				"error must name the composite CHECK constraint")
		})
	}

	// Kind CHECK rejects unknown kinds.
	t.Run("kind_check_rejects_unknown", func(t *testing.T) {
		_, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           "bogus_kind",
			Lifecycle:      "manual",
			ExternalTaskID: "check-bogus-kind-" + ns,
			State:          "managed",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "contact_task_kind_check",
			"error must name the kind CHECK constraint")
	})

	// Lifecycle CHECK rejects unknown lifecycles. Either the lifecycle
	// CHECK or the composite CHECK can fire first (Postgres picks the
	// constraint that fails first; for an unknown lifecycle value with
	// a known kind, both reject because the unknown value is also not
	// in any composite pair). Accepting either constraint name keeps
	// this test robust against Postgres's evaluation order.
	t.Run("lifecycle_check_rejects_unknown", func(t *testing.T) {
		_, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           "reach_out",
			Lifecycle:      "bogus_lifecycle",
			ExternalTaskID: "check-bogus-lc-" + ns,
			State:          "managed",
		})
		require.Error(t, err)
		errMsg := err.Error()
		require.True(t,
			strings.Contains(errMsg, "contact_task_lifecycle_check") ||
				strings.Contains(errMsg, "contact_task_kind_lifecycle_check"),
			"error must name a lifecycle-related CHECK constraint, got %q", errMsg)
	})

	// Pre-migration kind values (cadence / follow_up) are rejected by
	// the post-migration kind CHECK.
	for _, legacyKind := range []string{"cadence", "follow_up"} {
		t.Run("kind_check_rejects_legacy_"+legacyKind, func(t *testing.T) {
			_, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
				ContactID:      contact.ID,
				Provider:       "todoist",
				Kind:           legacyKind,
				Lifecycle:      "manual",
				ExternalTaskID: "check-legacy-" + legacyKind + "-" + ns,
				State:          "managed",
			})
			require.Error(t, err, "legacy kind=%q MUST be rejected post-046", legacyKind)
			require.Contains(t, err.Error(), "contact_task_kind_check",
				"error must name the kind CHECK constraint")
		})
	}
}

// TestContactTask_Migration046_PartialUniqueIndexes verifies the
// lifecycle-keyed partial unique indexes installed by migration 046:
//   - unique_contact_provider_cadence: (contact_id, provider) WHERE lifecycle='cadence_due'
//   - idx_contact_task_followup_unique_live: (contact_id, provider) WHERE lifecycle='followup_loop' AND state IN live
//
// These guarantee the GetContactTaskByContactCadenceDue and
// GetContactTaskByContactFollowUpLive single-row lookups never get a
// multi-row hit. lifecycle=manual has NO uniqueness — multiple rows
// per (contact_id, provider) are allowed (intentional design choice).
func TestContactTask_Migration046_PartialUniqueIndexes(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}
	defer database.Close()

	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	gen, ns := migrationGenerator(t)

	t.Run("cadence_due_unique_per_contact_provider", func(t *testing.T) {
		contact, contactCleanup := seedMigrationContact(ctx, t, database, gen, factory.WithCadence("weekly"))
		t.Cleanup(contactCleanup)

		_, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           contacttask.KindReachOut,
			Lifecycle:      contacttask.LifecycleCadenceDue,
			ExternalTaskID: "cadence-1-" + ns,
			State:          "managed",
		})
		require.NoError(t, err)

		// Second cadence_due row for same (contact, provider) must fail.
		_, err = contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           contacttask.KindReachOut,
			Lifecycle:      contacttask.LifecycleCadenceDue,
			ExternalTaskID: "cadence-2-" + ns,
			State:          "managed",
		})
		require.Error(t, err, "second cadence_due row for same (contact, provider) must violate unique index")
	})

	t.Run("followup_loop_live_unique_per_contact_provider", func(t *testing.T) {
		contact, contactCleanup := seedMigrationContact(ctx, t, database, gen, factory.WithCadence("weekly"))
		t.Cleanup(contactCleanup)

		_, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           contacttask.KindReachOut,
			Lifecycle:      contacttask.LifecycleFollowUpLoop,
			ExternalTaskID: "followup-1-" + ns,
			State:          "managed",
		})
		require.NoError(t, err)

		// Second live followup_loop row for same (contact, provider) must fail.
		_, err = contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           contacttask.KindReachOut,
			Lifecycle:      contacttask.LifecycleFollowUpLoop,
			ExternalTaskID: "followup-2-" + ns,
			State:          "managed",
		})
		require.Error(t, err, "second live followup_loop row for same (contact, provider) must violate unique index")
	})

	t.Run("manual_lifecycle_has_no_uniqueness", func(t *testing.T) {
		// Two reach_out manual tasks for the same contact/provider are
		// fine — manual lifecycle is intentionally unconstrained.
		contact, contactCleanup := seedMigrationContact(ctx, t, database, gen, factory.WithCadence("weekly"))
		t.Cleanup(contactCleanup)

		_, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           contacttask.KindReachOut,
			Lifecycle:      contacttask.LifecycleManual,
			ExternalTaskID: "manual-1-" + ns,
			State:          "managed",
		})
		require.NoError(t, err)

		_, err = contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           contacttask.KindReachOut,
			Lifecycle:      contacttask.LifecycleManual,
			ExternalTaskID: "manual-2-" + ns,
			State:          "managed",
		})
		require.NoError(t, err, "manual lifecycle has no uniqueness — both inserts must succeed")
	})
}
