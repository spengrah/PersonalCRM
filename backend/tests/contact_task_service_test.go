package tests

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/todoist"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockTodoistClient implements todoist.Client for testing
type mockTodoistClient struct {
	quickAddFunc func(ctx context.Context, text string, note string) (*todoist.QuickAddTask, error)
	syncFunc     func(ctx context.Context, syncToken string, resourceTypes []string, commands []todoist.SyncCommand) (*todoist.SyncResponse, error)
	// Capture calls for assertions
	quickAddCalls []quickAddCall
	syncCalls     []syncCall
}

type quickAddCall struct {
	Text string
	Note string
}

type syncCall struct {
	SyncToken     string
	ResourceTypes []string
	Commands      []todoist.SyncCommand
}

func (m *mockTodoistClient) QuickAdd(ctx context.Context, text string, note string) (*todoist.QuickAddTask, error) {
	m.quickAddCalls = append(m.quickAddCalls, quickAddCall{Text: text, Note: note})
	if m.quickAddFunc != nil {
		return m.quickAddFunc(ctx, text, note)
	}
	// Generate unique task ID using UUID to avoid conflicts between test runs
	taskID := "test-task-" + uuid.New().String()[:8]
	return &todoist.QuickAddTask{
		ID:        taskID,
		ProjectID: "test-project-id",
		Content:   text,
	}, nil
}

func (m *mockTodoistClient) Sync(ctx context.Context, syncToken string, resourceTypes []string, commands []todoist.SyncCommand) (*todoist.SyncResponse, error) {
	m.syncCalls = append(m.syncCalls, syncCall{SyncToken: syncToken, ResourceTypes: resourceTypes, Commands: commands})
	if m.syncFunc != nil {
		return m.syncFunc(ctx, syncToken, resourceTypes, commands)
	}
	return &todoist.SyncResponse{SyncToken: "new-sync-token"}, nil
}

// setupServiceTest creates the service with necessary database setup
// Returns the service, contact, and cleanup function
func setupServiceTest(t *testing.T) (*service.ContactTaskService, *repository.Contact, func()) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	ctx := context.Background()
	cfg := config.TestConfig()
	cfg.Database.URL = databaseURL
	cfg.CORS.FrontendURL = "https://example.com"

	database, err := db.NewDatabase(ctx, cfg.Database)
	if err != nil {
		t.Skipf("Could not connect to database: %v", err)
	}

	contactRepo := repository.NewContactRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	syncRepo := repository.NewSyncRepository(database.Queries)

	// Create a test contact
	now := accelerated.GetCurrentTime()
	cadence := "weekly"
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName:      "Test Contact for Service",
		Cadence:       &cadence,
		LastContacted: &now,
	})
	require.NoError(t, err)

	// Clean up any existing test sync state
	_ = syncRepo.DeleteSyncStatesByAccountID(ctx, "test-account-123")

	// Create sync state with Todoist settings (simulates configured Todoist)
	metadata := map[string]any{
		"project_id":              "proj-123",
		"project_name":            "CRM",
		"label_id":                "label-456",
		"label_name":              "crm",
		"integration_instance_id": "instance-789",
	}
	syncState, err := syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
		Source:    todoist.SourceName,
		AccountID: strPtr("test-account-123"),
		Metadata:  metadata,
	})
	require.NoError(t, err)

	// Create the service (without real OAuth - we'll mock the Todoist client)
	svc := service.NewContactTaskServiceForTest(
		contactTaskRepo,
		contactRepo,
		syncRepo,
		cfg.CORS.FrontendURL,
	)

	cleanup := func() {
		_ = contactRepo.HardDeleteContact(ctx, contact.ID)
		_ = syncRepo.DeleteSyncState(ctx, syncState.ID)
		database.Close()
	}

	return svc, contact, cleanup
}

func TestContactTaskService_CreateManualTask_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	svc, contact, cleanup := setupServiceTest(t)
	defer cleanup()

	ctx := context.Background()

	t.Run("creates task with contact link in content", func(t *testing.T) {
		mock := &mockTodoistClient{}
		svc.SetTodoistClientFactory(func(accessToken string) todoist.Client {
			return mock
		})

		_, err := svc.CreateManualTask(ctx, service.CreateManualTaskRequest{
			ContactID: contact.ID,
			Kind:      contacttask.KindReachOut,
			Text:      "Follow up on proposal",
			Notes:     "",
		})
		require.NoError(t, err)

		// Verify QuickAdd was called with contact link prefix
		require.Len(t, mock.quickAddCalls, 1)
		quickAddText := mock.quickAddCalls[0].Text
		assert.Contains(t, quickAddText, "[Test Contact for Service]")
		assert.Contains(t, quickAddText, "https://example.com/contacts/"+contact.ID.String())
		assert.Contains(t, quickAddText, "Follow up on proposal")
	})

	t.Run("appends project name with # syntax", func(t *testing.T) {
		mock := &mockTodoistClient{}
		svc.SetTodoistClientFactory(func(accessToken string) todoist.Client {
			return mock
		})

		_, err := svc.CreateManualTask(ctx, service.CreateManualTaskRequest{
			ContactID: contact.ID,
			Kind:      contacttask.KindReachOut,
			Text:      "Call about project",
			Notes:     "",
		})
		require.NoError(t, err)

		require.Len(t, mock.quickAddCalls, 1)
		quickAddText := mock.quickAddCalls[0].Text
		assert.Contains(t, quickAddText, "#CRM")
	})

	t.Run("skips project if user specified # in text", func(t *testing.T) {
		mock := &mockTodoistClient{}
		svc.SetTodoistClientFactory(func(accessToken string) todoist.Client {
			return mock
		})

		_, err := svc.CreateManualTask(ctx, service.CreateManualTaskRequest{
			ContactID: contact.ID,
			Kind:      contacttask.KindReachOut,
			Text:      "Task #MyProject",
			Notes:     "",
		})
		require.NoError(t, err)

		require.Len(t, mock.quickAddCalls, 1)
		quickAddText := mock.quickAddCalls[0].Text
		assert.Contains(t, quickAddText, "#MyProject")
		assert.NotContains(t, quickAddText, "#CRM")
	})

	t.Run("appends label name with @ syntax", func(t *testing.T) {
		mock := &mockTodoistClient{}
		svc.SetTodoistClientFactory(func(accessToken string) todoist.Client {
			return mock
		})

		_, err := svc.CreateManualTask(ctx, service.CreateManualTaskRequest{
			ContactID: contact.ID,
			Kind:      contacttask.KindReachOut,
			Text:      "Send email",
			Notes:     "",
		})
		require.NoError(t, err)

		require.Len(t, mock.quickAddCalls, 1)
		quickAddText := mock.quickAddCalls[0].Text
		assert.Contains(t, quickAddText, "@crm")
	})

	t.Run("calls Sync API to update description with CRM marker", func(t *testing.T) {
		var capturedTaskID string
		mock := &mockTodoistClient{
			quickAddFunc: func(ctx context.Context, text string, note string) (*todoist.QuickAddTask, error) {
				capturedTaskID = "test-task-" + uuid.New().String()[:8]
				return &todoist.QuickAddTask{
					ID:        capturedTaskID,
					ProjectID: "test-project-id",
					Content:   text,
				}, nil
			},
		}
		svc.SetTodoistClientFactory(func(accessToken string) todoist.Client {
			return mock
		})

		_, err := svc.CreateManualTask(ctx, service.CreateManualTaskRequest{
			ContactID: contact.ID,
			Kind:      contacttask.KindReachOut,
			Text:      "Test task",
			Notes:     "Some notes",
		})
		require.NoError(t, err)

		// Verify Sync was called with item_update command
		require.Len(t, mock.syncCalls, 1)
		require.Len(t, mock.syncCalls[0].Commands, 1)

		cmd := mock.syncCalls[0].Commands[0]
		assert.Equal(t, "item_update", cmd.Type)
		assert.Equal(t, capturedTaskID, cmd.Args["id"])

		// Verify description contains CRM marker
		desc, ok := cmd.Args["description"].(string)
		require.True(t, ok)
		assert.Contains(t, desc, "Some notes")
		assert.Contains(t, desc, `"crm":true`)
		assert.Contains(t, desc, `"contact_id"`)
		assert.Contains(t, desc, `"kind":"action"`)
	})

	t.Run("deletes task if Sync update fails", func(t *testing.T) {
		var capturedTaskID string
		mock := &mockTodoistClient{
			quickAddFunc: func(ctx context.Context, text string, note string) (*todoist.QuickAddTask, error) {
				capturedTaskID = "test-task-" + uuid.New().String()[:8]
				return &todoist.QuickAddTask{
					ID:        capturedTaskID,
					ProjectID: "test-project-id",
					Content:   text,
				}, nil
			},
			syncFunc: func(ctx context.Context, syncToken string, resourceTypes []string, commands []todoist.SyncCommand) (*todoist.SyncResponse, error) {
				// First call (update) fails, second call (delete) succeeds
				if len(commands) > 0 && commands[0].Type == "item_update" {
					return nil, errors.New("sync failed")
				}
				return &todoist.SyncResponse{}, nil
			},
		}
		svc.SetTodoistClientFactory(func(accessToken string) todoist.Client {
			return mock
		})

		_, err := svc.CreateManualTask(ctx, service.CreateManualTaskRequest{
			ContactID: contact.ID,
			Kind:      contacttask.KindReachOut,
			Text:      "Task that will fail",
			Notes:     "",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "update task description")

		// Verify delete command was sent
		require.Len(t, mock.syncCalls, 2)
		deleteCmd := mock.syncCalls[1].Commands[0]
		assert.Equal(t, "item_delete", deleteCmd.Type)
		assert.Equal(t, capturedTaskID, deleteCmd.Args["id"])
	})

	t.Run("stores metadata from Todoist response", func(t *testing.T) {
		mock := &mockTodoistClient{
			quickAddFunc: func(ctx context.Context, text string, note string) (*todoist.QuickAddTask, error) {
				return &todoist.QuickAddTask{
					ID:        "task-with-due-" + contact.ID.String()[:8],
					ProjectID: "proj-abc",
					Content:   text,
					Due: &todoist.SyncDue{
						Date: "2025-12-31",
					},
				}, nil
			},
		}
		svc.SetTodoistClientFactory(func(accessToken string) todoist.Client {
			return mock
		})

		resp, err := svc.CreateManualTask(ctx, service.CreateManualTaskRequest{
			ContactID: contact.ID,
			Kind:      contacttask.KindReachOut,
			Text:      "Task with due date",
			Notes:     "",
		})
		require.NoError(t, err)

		assert.Contains(t, resp.ExternalTaskID, "task-with-due-")
		assert.Equal(t, "proj-abc", resp.ProjectID)
		require.NotNil(t, resp.DueDate)
		assert.Equal(t, "2025-12-31", *resp.DueDate)
	})
}

func TestContactTaskService_CRMMarkerFormat(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test")
	}

	svc, contact, cleanup := setupServiceTest(t)
	defer cleanup()

	ctx := context.Background()

	t.Run("CRM marker contains required fields", func(t *testing.T) {
		var capturedDesc string
		mock := &mockTodoistClient{
			syncFunc: func(ctx context.Context, syncToken string, resourceTypes []string, commands []todoist.SyncCommand) (*todoist.SyncResponse, error) {
				if len(commands) > 0 && commands[0].Type == "item_update" {
					capturedDesc = commands[0].Args["description"].(string)
				}
				return &todoist.SyncResponse{}, nil
			},
		}
		svc.SetTodoistClientFactory(func(accessToken string) todoist.Client {
			return mock
		})

		_, err := svc.CreateManualTask(ctx, service.CreateManualTaskRequest{
			ContactID: contact.ID,
			Kind:      contacttask.KindReachOut,
			Text:      "Marker test",
			Notes:     "Test notes",
		})
		require.NoError(t, err)

		// Parse the JSON marker from the description
		parts := strings.Split(capturedDesc, "---\n")
		require.Len(t, parts, 2, "description should have notes and marker separated by ---")

		assert.Contains(t, parts[0], "Test notes")

		var marker map[string]any
		err = json.Unmarshal([]byte(parts[1]), &marker)
		require.NoError(t, err)

		assert.Equal(t, true, marker["crm"])
		assert.Equal(t, contact.ID.String(), marker["contact_id"])
		assert.Equal(t, "action", marker["kind"])
		assert.Equal(t, "instance-789", marker["instance"])
	})
}

func strPtr(s string) *string {
	return &s
}
