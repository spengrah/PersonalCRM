// Package tests contains integration tests for contact_task functionality.
//
// These tests verify the contact_task table operations used by the
// Todoist cadence sync feature.
package tests

import (
	"context"
	"os"
	"testing"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"

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

	// Create a test contact
	now := accelerated.GetCurrentTime()
	cadence := "weekly"
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName:      "Test Contact for Tasks",
		Cadence:       &cadence,
		LastContacted: &now,
	})
	require.NoError(t, err)
	defer func() {
		_ = contactRepo.HardDeleteContact(ctx, contact.ID)
	}()

	t.Run("create and retrieve contact task", func(t *testing.T) {
		// Create task
		task, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           "cadence",
			ExternalTaskID: "12345",
			State:          "managed",
			Metadata:       map[string]any{"test_key": "test_value"},
		})
		require.NoError(t, err)
		assert.Equal(t, contact.ID, task.ContactID)
		assert.Equal(t, "todoist", task.Provider)
		assert.Equal(t, "cadence", task.Kind)
		assert.Equal(t, "12345", task.ExternalTaskID)
		assert.Equal(t, repository.ContactTaskStateManaged, task.State)

		// Retrieve by ID
		retrieved, err := contactTaskRepo.GetContactTask(ctx, task.ID)
		require.NoError(t, err)
		assert.Equal(t, task.ID, retrieved.ID)
		assert.Equal(t, "test_value", retrieved.Metadata["test_key"])

		// Retrieve by contact+provider+kind
		retrieved2, err := contactTaskRepo.GetContactTaskByContact(ctx, contact.ID, "todoist", "cadence")
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
			Kind:           "cadence",
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
			Kind:           "cadence",
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
			Kind:           "cadence",
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
			Kind:           "cadence",
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
			Kind:           "cadence",
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
		assert.Equal(t, cadence, *found.Cadence)

		// Clean up
		err = contactTaskRepo.DeleteContactTask(ctx, task.ID)
		require.NoError(t, err)
	})

	t.Run("delete by contact+provider+kind", func(t *testing.T) {
		// Create a task
		_, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           "cadence",
			ExternalTaskID: "66666",
		})
		require.NoError(t, err)

		// Delete by contact+provider+kind
		err = contactTaskRepo.DeleteContactTaskByContact(ctx, contact.ID, "todoist", "cadence")
		require.NoError(t, err)

		// Verify deleted
		_, err = contactTaskRepo.GetContactTaskByContact(ctx, contact.ID, "todoist", "cadence")
		assert.ErrorIs(t, err, db.ErrNotFound)
	})

	t.Run("cascade delete when contact is deleted", func(t *testing.T) {
		// Create a new contact
		tempContact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName: "Temp Contact for Cascade Test",
		})
		require.NoError(t, err)

		// Create a task for this contact
		task, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      tempContact.ID,
			Provider:       "todoist",
			Kind:           "cadence",
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
			Kind:           "cadence",
			ExternalTaskID: "88888",
		})
		require.NoError(t, err)

		// Try to create duplicate (should fail)
		_, err = contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           "cadence",
			ExternalTaskID: "99999",
		})
		assert.Error(t, err) // Should fail due to unique constraint

		// Clean up
		err = contactTaskRepo.DeleteContactTaskByContact(ctx, contact.ID, "todoist", "cadence")
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

	contactRepo := repository.NewContactRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)

	// Create test contacts
	var contacts []uuid.UUID
	for i := 0; i < 3; i++ {
		now := accelerated.GetCurrentTime()
		cadence := "weekly"
		c, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
			FullName:      "Test Contact " + uuid.New().String()[:8],
			Cadence:       &cadence,
			LastContacted: &now,
		})
		require.NoError(t, err)
		contacts = append(contacts, c.ID)
	}
	defer func() {
		for _, id := range contacts {
			_ = contactRepo.HardDeleteContact(ctx, id)
		}
	}()

	// Create tasks for each contact
	for i, contactID := range contacts {
		state := "managed"
		if i == 2 {
			state = "unmanaged"
		}
		_, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contactID,
			Provider:       "todoist",
			Kind:           "cadence",
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

	contactRepo := repository.NewContactRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)

	// Create a test contact
	now := accelerated.GetCurrentTime()
	cadence := "weekly"
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName:      "Test Contact for SyncedDeadline",
		Cadence:       &cadence,
		LastContacted: &now,
	})
	require.NoError(t, err)
	defer func() {
		_ = contactRepo.HardDeleteContact(ctx, contact.ID)
	}()

	t.Run("create task with synced_deadline metadata", func(t *testing.T) {
		// Create task with synced_deadline in metadata (simulating what reconciliation does)
		task, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           "cadence",
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
			Kind:           "cadence",
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

	contactRepo := repository.NewContactRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)

	// Create a test contact
	now := accelerated.GetCurrentTime()
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName:      "Test Contact for Action Tasks",
		LastContacted: &now,
	})
	require.NoError(t, err)
	defer func() {
		_ = contactRepo.HardDeleteContact(ctx, contact.ID)
	}()

	t.Run("multiple action tasks per contact allowed", func(t *testing.T) {
		// Create first action task
		task1, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           "action",
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
			Kind:           "action",
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
			Kind:           "action",
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
			Kind:           "action",
			ExternalTaskID: "action-managed",
			State:          "managed",
		})
		require.NoError(t, err)

		actionCompleted, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           "action",
			ExternalTaskID: "action-completed",
			State:          "completed",
		})
		require.NoError(t, err)

		cadenceManaged, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           "cadence",
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
		managedTasks, err := contactTaskRepo.ListContactTasksFiltered(ctx, contact.ID, &managed, nil)
		require.NoError(t, err)
		assert.Len(t, managedTasks, 2) // action-managed and cadence-managed

		// Filter by state=completed
		completed := "completed"
		completedTasks, err := contactTaskRepo.ListContactTasksFiltered(ctx, contact.ID, &completed, nil)
		require.NoError(t, err)
		assert.Len(t, completedTasks, 1)
		assert.Equal(t, "action-completed", completedTasks[0].ExternalTaskID)

		// Filter by kind=action
		action := "action"
		actionTasks, err := contactTaskRepo.ListContactTasksFiltered(ctx, contact.ID, nil, &action)
		require.NoError(t, err)
		assert.Len(t, actionTasks, 2) // action-managed and action-completed

		// Filter by kind=action AND state=managed
		actionManagedTasks, err := contactTaskRepo.ListContactTasksFiltered(ctx, contact.ID, &managed, &action)
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
			Kind:           "action",
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
			Kind:           "action",
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
