// Package tests contains integration tests for contact_task functionality.
//
// These tests verify the contact_task table operations used by the
// Todoist cadence sync feature.
package tests

import (
	"context"
	"os"
	"testing"
	"time"

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
		})
		require.NoError(t, err)
		assert.Equal(t, "11111", task1.ExternalTaskID)

		// Upsert again (should update)
		task2, err := contactTaskRepo.UpsertContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contact.ID,
			Provider:       "todoist",
			Kind:           "cadence",
			ExternalTaskID: "22222",
		})
		require.NoError(t, err)
		assert.Equal(t, task1.ID, task2.ID)            // Same ID
		assert.Equal(t, "22222", task2.ExternalTaskID) // Updated external ID

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
		now := time.Now()
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
