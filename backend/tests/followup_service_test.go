package tests

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/todoist"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// followUpMockClient implements todoist.Client for testing follow-up service
type followUpMockClient struct {
	syncFunc  func(ctx context.Context, syncToken string, resourceTypes []string, commands []todoist.SyncCommand) (*todoist.SyncResponse, error)
	syncCalls [][]todoist.SyncCommand
}

func (m *followUpMockClient) QuickAdd(_ context.Context, _ string, _ string) (*todoist.QuickAddTask, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *followUpMockClient) Sync(ctx context.Context, syncToken string, resourceTypes []string, commands []todoist.SyncCommand) (*todoist.SyncResponse, error) {
	m.syncCalls = append(m.syncCalls, commands)
	if m.syncFunc != nil {
		return m.syncFunc(ctx, syncToken, resourceTypes, commands)
	}
	return &todoist.SyncResponse{SyncToken: "test"}, nil
}

var fakeSettings = &todoist.Settings{
	ProjectID:             "test-project",
	ProjectName:           "CRM",
	LabelID:               "test-label-id",
	LabelName:             "crm",
	IntegrationInstanceID: "test-instance",
}

// fakeSettingsFunc returns a todoistSettingsFunc that always succeeds
func fakeSettingsFunc() func(ctx context.Context) (*todoist.Settings, string, error) {
	return func(_ context.Context) (*todoist.Settings, string, error) {
		return fakeSettings, "fake-access-token", nil
	}
}

func setupFollowUpTestDeps(t *testing.T) (*repository.ContactRepository, *repository.ContactTaskRepository, *config.Config, func()) {
	t.Helper()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	migrationsPath := getMigrationsPath()
	if err := db.RunMigrations(databaseURL, migrationsPath); err != nil {
		t.Fatalf("Failed to run migrations: %v", err)
	}

	ctx := context.Background()
	dbConfig := config.DatabaseConfig{
		URL:               databaseURL,
		MaxConns:          config.DefaultDBMaxConns,
		MinConns:          config.DefaultDBMinConns,
		MaxConnIdleTime:   config.DefaultDBMaxConnIdleTime,
		MaxConnLifetime:   config.DefaultDBMaxConnLifetime,
		HealthCheckPeriod: config.DefaultDBHealthCheckPeriod,
	}
	database, err := db.NewDatabase(ctx, dbConfig)
	require.NoError(t, err)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	cfg := config.TestConfig()

	cleanup := func() {
		database.Close()
	}

	return contactRepo, contactTaskRepo, cfg, cleanup
}

// newTestFollowUpService creates a FollowUpService wired to the given mock client and fake settings.
// Bypasses OAuth entirely by injecting both the client factory and settings function.
func newTestFollowUpService(contactTaskRepo *repository.ContactTaskRepository, contactRepo *repository.ContactRepository, cfg *config.Config, mock *followUpMockClient) *service.FollowUpService {
	svc := service.NewFollowUpService(contactTaskRepo, contactRepo, nil, nil, cfg)
	svc.SetTodoistClientFactory(func(_ string) todoist.Client { return mock })
	svc.SetTodoistSettingsFunc(fakeSettingsFunc())
	return svc
}

func TestCreateFollowUp_SuccessfulCreation(t *testing.T) {
	contactRepo, contactTaskRepo, cfg, cleanup := setupFollowUpTestDeps(t)
	defer cleanup()
	ctx := context.Background()

	cadence := "monthly"
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Create Followup OK " + uuid.New().String()[:8],
		Cadence:  &cadence,
	})
	require.NoError(t, err)

	// Mock returns a valid TempIDMap
	mock := &followUpMockClient{
		syncFunc: func(_ context.Context, _ string, _ []string, commands []todoist.SyncCommand) (*todoist.SyncResponse, error) {
			tempIDMap := make(map[string]string)
			for _, cmd := range commands {
				if cmd.Type == "item_add" {
					tempIDMap[cmd.TempID] = "real-todoist-id-" + cmd.TempID[:8]
				}
			}
			return &todoist.SyncResponse{SyncToken: "test", TempIDMap: tempIDMap}, nil
		},
	}
	svc := newTestFollowUpService(contactTaskRepo, contactRepo, cfg, mock)

	err = svc.CreateOrRefreshFollowUp(ctx, *contact, time.Date(2026, 4, 5, 10, 0, 0, 0, time.UTC))
	require.NoError(t, err)

	// Verify follow-up task was created
	task, err := contactTaskRepo.FindPendingFollowUp(ctx, contact.ID)
	require.NoError(t, err)
	assert.Equal(t, "follow_up", task.Kind)
	assert.Equal(t, repository.ContactTaskStateManaged, task.State)
	assert.Contains(t, task.ExternalTaskID, "real-todoist-id-", "should have real ID from TempIDMap")
	assert.NotEmpty(t, task.Metadata["due_date"])
	assert.NotEmpty(t, task.Metadata["content"])

	// Verify Todoist API was called with item_add
	require.Len(t, mock.syncCalls, 1)
	require.Len(t, mock.syncCalls[0], 1)
	assert.Equal(t, "item_add", mock.syncCalls[0][0].Type)
}

func TestCreateFollowUp_FailsWhenTempIDMissing(t *testing.T) {
	contactRepo, contactTaskRepo, cfg, cleanup := setupFollowUpTestDeps(t)
	defer cleanup()
	ctx := context.Background()

	cadence := "monthly"
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "TempID Missing " + uuid.New().String()[:8],
		Cadence:  &cadence,
	})
	require.NoError(t, err)

	// Mock returns empty TempIDMap — simulates command failure on Todoist side
	mock := &followUpMockClient{
		syncFunc: func(_ context.Context, _ string, _ []string, _ []todoist.SyncCommand) (*todoist.SyncResponse, error) {
			return &todoist.SyncResponse{SyncToken: "test", TempIDMap: map[string]string{}}, nil
		},
	}
	svc := newTestFollowUpService(contactTaskRepo, contactRepo, cfg, mock)

	err = svc.CreateOrRefreshFollowUp(ctx, *contact, time.Date(2026, 4, 5, 10, 0, 0, 0, time.UTC))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "todoist did not return task ID")

	// Verify no follow-up task was created locally
	_, err = contactTaskRepo.FindPendingFollowUp(ctx, contact.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestCompleteFollowUp_SetsRetryFlag_OnTodoistCloseFailure(t *testing.T) {
	contactRepo, contactTaskRepo, cfg, cleanup := setupFollowUpTestDeps(t)
	defer cleanup()
	ctx := context.Background()

	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Close Fail Retry " + uuid.New().String()[:8],
	})
	require.NoError(t, err)

	// Create a managed follow-up task
	externalID := "test-close-fail-" + uuid.New().String()[:8]
	task, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
		ContactID:      contact.ID,
		Provider:       "todoist",
		Kind:           "follow_up",
		ExternalTaskID: externalID,
		State:          "managed",
		Metadata:       map[string]any{"due_date": "2026-04-10"},
	})
	require.NoError(t, err)

	// Mock Todoist client that fails on close
	mock := &followUpMockClient{
		syncFunc: func(_ context.Context, _ string, _ []string, _ []todoist.SyncCommand) (*todoist.SyncResponse, error) {
			return nil, fmt.Errorf("todoist API timeout")
		},
	}
	svc := newTestFollowUpService(contactTaskRepo, contactRepo, cfg, mock)

	err = svc.CompleteFollowUp(ctx, contact.ID)
	require.NoError(t, err, "CompleteFollowUp should succeed even when Todoist close fails")

	// Verify: task should be completed locally
	_, err = contactTaskRepo.FindPendingFollowUp(ctx, contact.ID)
	assert.ErrorIs(t, err, db.ErrNotFound, "should have no pending follow-up")

	// Verify: completed task should have todoist_close_pending flag
	completedTask, err := contactTaskRepo.GetContactTask(ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.ContactTaskStateCompleted, completedTask.State)
	assert.Equal(t, true, completedTask.Metadata["todoist_close_pending"],
		"should set todoist_close_pending flag for retry")

	// Verify the Todoist close was attempted
	require.Len(t, mock.syncCalls, 1)
	require.Len(t, mock.syncCalls[0], 1)
	assert.Equal(t, "item_close", mock.syncCalls[0][0].Type)
}

func TestCompleteFollowUp_NoRetryFlag_OnSuccess(t *testing.T) {
	contactRepo, contactTaskRepo, cfg, cleanup := setupFollowUpTestDeps(t)
	defer cleanup()
	ctx := context.Background()

	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Close OK " + uuid.New().String()[:8],
	})
	require.NoError(t, err)

	externalID := "test-close-ok-" + uuid.New().String()[:8]
	task, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
		ContactID:      contact.ID,
		Provider:       "todoist",
		Kind:           "follow_up",
		ExternalTaskID: externalID,
		State:          "managed",
		Metadata:       map[string]any{"due_date": "2026-04-10"},
	})
	require.NoError(t, err)

	// Mock Todoist client that succeeds
	mock := &followUpMockClient{}
	svc := newTestFollowUpService(contactTaskRepo, contactRepo, cfg, mock)

	err = svc.CompleteFollowUp(ctx, contact.ID)
	require.NoError(t, err)

	// Verify: task completed, no retry flag
	completedTask, err := contactTaskRepo.GetContactTask(ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.ContactTaskStateCompleted, completedTask.State)
	assert.Nil(t, completedTask.Metadata["todoist_close_pending"],
		"should NOT set todoist_close_pending on successful close")
}

func TestRetryPendingCloses_ClearsFlag_OnSuccess(t *testing.T) {
	contactRepo, contactTaskRepo, cfg, cleanup := setupFollowUpTestDeps(t)
	defer cleanup()
	ctx := context.Background()

	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Retry Clear " + uuid.New().String()[:8],
	})
	require.NoError(t, err)

	// Create a completed follow-up with todoist_close_pending
	externalID := "test-retry-" + uuid.New().String()[:8]
	task, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
		ContactID:      contact.ID,
		Provider:       "todoist",
		Kind:           "follow_up",
		ExternalTaskID: externalID,
		State:          "completed",
		Metadata:       map[string]any{"todoist_close_pending": true, "due_date": "2026-04-10"},
	})
	require.NoError(t, err)

	// Mock Todoist client that succeeds on retry
	mock := &followUpMockClient{}
	svc := newTestFollowUpService(contactTaskRepo, contactRepo, cfg, mock)

	svc.RetryPendingCloses(ctx)

	// Verify: flag should be cleared on our task
	updatedTask, err := contactTaskRepo.GetContactTask(ctx, task.ID)
	require.NoError(t, err)
	assert.Nil(t, updatedTask.Metadata["todoist_close_pending"],
		"todoist_close_pending should be cleared after successful retry")

	// Verify Todoist close was called at least once (may include other tests' stale tasks)
	require.GreaterOrEqual(t, len(mock.syncCalls), 1, "should have called Todoist at least once")
	// Find our specific close call
	found := false
	for _, calls := range mock.syncCalls {
		for _, cmd := range calls {
			if cmd.Type == "item_close" {
				if id, ok := cmd.Args["id"].(string); ok && id == externalID {
					found = true
				}
			}
		}
	}
	assert.True(t, found, "should have called item_close for our specific task")
}

func TestRetryPendingCloses_KeepsFlag_OnFailure(t *testing.T) {
	contactRepo, contactTaskRepo, cfg, cleanup := setupFollowUpTestDeps(t)
	defer cleanup()
	ctx := context.Background()

	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Retry Fail " + uuid.New().String()[:8],
	})
	require.NoError(t, err)

	externalID := "test-retry-fail-" + uuid.New().String()[:8]
	task, err := contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
		ContactID:      contact.ID,
		Provider:       "todoist",
		Kind:           "follow_up",
		ExternalTaskID: externalID,
		State:          "completed",
		Metadata:       map[string]any{"todoist_close_pending": true},
	})
	require.NoError(t, err)

	// Mock Todoist client that still fails
	mock := &followUpMockClient{
		syncFunc: func(_ context.Context, _ string, _ []string, _ []todoist.SyncCommand) (*todoist.SyncResponse, error) {
			return nil, fmt.Errorf("still failing")
		},
	}
	svc := newTestFollowUpService(contactTaskRepo, contactRepo, cfg, mock)

	svc.RetryPendingCloses(ctx)

	// Verify: flag should still be set
	updatedTask, err := contactTaskRepo.GetContactTask(ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, true, updatedTask.Metadata["todoist_close_pending"],
		"flag should persist when retry fails")
}

func TestCompleteFollowUp_NoFollowUpExists(t *testing.T) {
	contactRepo, contactTaskRepo, cfg, cleanup := setupFollowUpTestDeps(t)
	defer cleanup()
	ctx := context.Background()

	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "No Followup " + uuid.New().String()[:8],
	})
	require.NoError(t, err)

	mock := &followUpMockClient{}
	svc := newTestFollowUpService(contactTaskRepo, contactRepo, cfg, mock)

	err = svc.CompleteFollowUp(ctx, contact.ID)
	assert.NoError(t, err, "should succeed when no follow-up exists")
	assert.Empty(t, mock.syncCalls, "should not call Todoist when no follow-up exists")
}

func TestCreateOrRefreshFollowUp_NoCadence_Skips(t *testing.T) {
	contactRepo, contactTaskRepo, cfg, cleanup := setupFollowUpTestDeps(t)
	defer cleanup()
	ctx := context.Background()

	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "No Cadence " + uuid.New().String()[:8],
	})
	require.NoError(t, err)

	mock := &followUpMockClient{}
	svc := newTestFollowUpService(contactTaskRepo, contactRepo, cfg, mock)

	err = svc.CreateOrRefreshFollowUp(ctx, *contact, time.Date(2026, 4, 5, 10, 0, 0, 0, time.UTC))
	assert.NoError(t, err)
	assert.Empty(t, mock.syncCalls, "should not call Todoist when contact has no cadence")

	_, err = contactTaskRepo.FindPendingFollowUp(ctx, contact.ID)
	assert.ErrorIs(t, err, db.ErrNotFound)
}

func TestCreateOrRefreshFollowUp_RefreshExisting(t *testing.T) {
	contactRepo, contactTaskRepo, cfg, cleanup := setupFollowUpTestDeps(t)
	defer cleanup()
	ctx := context.Background()

	cadence := "monthly"
	contact, err := contactRepo.CreateContact(ctx, repository.CreateContactRequest{
		FullName: "Refresh Followup " + uuid.New().String()[:8],
		Cadence:  &cadence,
	})
	require.NoError(t, err)

	// Create an existing pending follow-up
	externalID := "test-refresh-" + uuid.New().String()[:8]
	_, err = contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
		ContactID:      contact.ID,
		Provider:       "todoist",
		Kind:           "follow_up",
		ExternalTaskID: externalID,
		State:          "managed",
		Metadata:       map[string]any{"due_date": "2026-04-10"},
	})
	require.NoError(t, err)

	mock := &followUpMockClient{}
	svc := newTestFollowUpService(contactTaskRepo, contactRepo, cfg, mock)

	// Second outbound should refresh, not create a new one
	err = svc.CreateOrRefreshFollowUp(ctx, *contact, time.Date(2026, 4, 12, 10, 0, 0, 0, time.UTC))
	require.NoError(t, err)

	// Verify: Todoist was called with item_update (not item_add)
	require.Len(t, mock.syncCalls, 1)
	require.Len(t, mock.syncCalls[0], 1)
	assert.Equal(t, "item_update", mock.syncCalls[0][0].Type, "should update existing task, not create new one")

	// Verify: due_date was updated in metadata
	task, err := contactTaskRepo.FindPendingFollowUp(ctx, contact.ID)
	require.NoError(t, err)
	assert.NotEqual(t, "2026-04-10", task.Metadata["due_date"], "due_date should be updated")
}
