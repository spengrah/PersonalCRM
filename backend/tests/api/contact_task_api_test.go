package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"personal-crm/backend/internal/api"
	"personal-crm/backend/internal/api/handlers"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/db"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/service"
	"personal-crm/backend/internal/todoist"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeContactTaskTodoistClient implements todoist.Client. It records call
// counts so tests can prove which endpoints do (and do NOT) touch the remote
// task provider, and returns a deterministic per-call task ID.
type fakeContactTaskTodoistClient struct {
	mu            sync.Mutex
	quickAddCalls int
	syncCalls     int
	lastTaskID    string
}

func (f *fakeContactTaskTodoistClient) QuickAdd(_ context.Context, text string, _ string) (*todoist.QuickAddTask, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quickAddCalls++
	f.lastTaskID = "ct-api-task-" + uuid.NewString()
	return &todoist.QuickAddTask{
		ID:        f.lastTaskID,
		ProjectID: "ct-api-project",
		Content:   text,
	}, nil
}

func (f *fakeContactTaskTodoistClient) Sync(_ context.Context, _ string, _ []string, _ []todoist.SyncCommand) (*todoist.SyncResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.syncCalls++
	return &todoist.SyncResponse{SyncToken: "ct-api-sync-token"}, nil
}

func (f *fakeContactTaskTodoistClient) callCounts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.quickAddCalls, f.syncCalls
}

func (f *fakeContactTaskTodoistClient) lastCreatedTaskID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastTaskID
}

type contactTaskAPIHarness struct {
	router          *gin.Engine
	contactRepo     *repository.ContactRepository
	contactTaskRepo *repository.ContactTaskRepository
	fakeTodoist     *fakeContactTaskTodoistClient
}

// setupContactTaskAPIRouter wires the contact-task route surface through the
// PRODUCTION registration seam (handlers.RegisterContactTaskRoutes) on top of
// the real service/repository/DB chain, with only the outbound Todoist client
// faked.
func setupContactTaskAPIRouter(t *testing.T) (*contactTaskAPIHarness, func()) {
	t.Helper()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	// Migrations are applied once by TestMain.

	ctx := context.Background()
	// MaxConns/MinConns mirror config.TestConfig() (8/1) to cap the per-pool
	// connection ceiling under parallel execution.
	dbConfig := config.DatabaseConfig{
		URL:               databaseURL,
		MaxConns:          8,
		MinConns:          1,
		MaxConnIdleTime:   config.DefaultDBMaxConnIdleTime,
		MaxConnLifetime:   config.DefaultDBMaxConnLifetime,
		HealthCheckPeriod: config.DefaultDBHealthCheckPeriod,
	}
	database, err := db.NewDatabase(ctx, dbConfig)
	require.NoError(t, err)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	syncRepo := repository.NewSyncRepository(database.Queries)

	// Seed a labeled Todoist sync state under a per-run account so
	// CreateManualTask's test-mode settings lookup succeeds. The test-mode
	// lookup takes the FIRST todoist row from ListSyncStates (ordered by
	// account_id), and sibling suites seed label-less todoist states under
	// letter-prefixed accounts ("test-account-", "todoist-acct-",
	// "watchdog-"), so a digit prefix keeps this labeled state first.
	accountID := "00-ct-api-account-" + uuid.NewString()[:8]
	syncState, err := syncRepo.CreateSyncState(ctx, repository.CreateSyncStateRequest{
		Source:    todoist.SourceName,
		AccountID: stringPtr(accountID),
		Metadata: map[string]any{
			todoist.MetadataKeyProjectID:           "ct-api-proj",
			todoist.MetadataKeyProjectName:         "CRM",
			todoist.MetadataKeyLabelID:             "ct-api-label",
			todoist.MetadataKeyLabelName:           "crm",
			todoist.MetadataKeyIntegrationInstance: "ct-api-instance",
		},
	})
	require.NoError(t, err)

	contactTaskService := service.NewContactTaskServiceForTest(contactTaskRepo, contactRepo, syncRepo, "http://localhost:3000")
	fakeTodoist := &fakeContactTaskTodoistClient{}
	contactTaskService.SetTodoistClientFactory(func(string) todoist.Client { return fakeTodoist })

	contactTaskHandler := handlers.NewContactTaskHandler(contactTaskService)

	router := gin.New()
	router.Use(api.RequestIDMiddleware())
	v1 := router.Group("/api/v1")
	handlers.RegisterContactTaskRoutes(v1, contactTaskHandler)

	cleanup := func() {
		_ = syncRepo.DeleteSyncState(ctx, syncState.ID)
		database.Close()
	}

	return &contactTaskAPIHarness{
		router:          router,
		contactRepo:     contactRepo,
		contactTaskRepo: contactTaskRepo,
		fakeTodoist:     fakeTodoist,
	}, cleanup
}

// createContactTaskTestContact seeds a contact through the production
// repository and registers a hard-delete cleanup (FK cascade removes its
// contact_task rows).
func createContactTaskTestContact(t *testing.T, h *contactTaskAPIHarness, name string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	contact, err := h.contactRepo.CreateContact(ctx, repository.CreateContactRequest{FullName: name})
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = h.contactRepo.HardDeleteContact(ctx, contact.ID)
	})
	return contact.ID
}

func decodeContactTaskEnvelope(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var envelope map[string]any
	require.NoError(t, json.Unmarshal(body, &envelope))
	return envelope
}

func TestContactTaskAPI_ListFilters(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()

	h, cleanup := setupContactTaskAPIRouter(t)
	defer cleanup()
	ctx := context.Background()

	suffix := uuid.NewString()[:8]
	contactID := createContactTaskTestContact(t, h, "Task State Filter API Test "+suffix)

	// Seed one manual-lifecycle task per state in the closed set. Manual
	// lifecycle carries no per-contact partial unique index, so all five
	// states can coexist on one contact.
	stateClosedSet := []string{"managed", "completed", "unmanaged", "dismissed", "pending_remote_create"}
	externalIDByState := make(map[string]string, len(stateClosedSet))
	for _, state := range stateClosedSet {
		externalID := "ct-api-state-" + state + "-" + suffix
		externalIDByState[state] = externalID
		_, err := h.contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contactID,
			Provider:       todoist.SourceName,
			Kind:           contacttask.KindReachOut,
			Lifecycle:      contacttask.LifecycleManual,
			ExternalTaskID: externalID,
			State:          state,
		})
		require.NoError(t, err)
	}

	t.Run("StateFilterEachClosedSetMember", func(t *testing.T) {
		// spec: CAD-032[0]
		for _, state := range stateClosedSet {
			req, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID.String()+"/tasks?state="+state, nil)
			w := httptest.NewRecorder()
			h.router.ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code, "state=%s should be accepted", state)

			envelope := decodeContactTaskEnvelope(t, w.Body.Bytes())
			items, ok := envelope["data"].([]any)
			require.True(t, ok, "data should be an array for state=%s", state)
			require.Len(t, items, 1, "exactly the one %s task should match", state)
			task, ok := items[0].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, state, task["state"])
			assert.Equal(t, externalIDByState[state], task["external_task_id"])
		}
	})

	t.Run("UnfilteredListReturnsAllStates", func(t *testing.T) {
		// spec: CAD-032[0]
		req, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID.String()+"/tasks", nil)
		w := httptest.NewRecorder()
		h.router.ServeHTTP(w, req)
		require.Equal(t, http.StatusOK, w.Code)

		envelope := decodeContactTaskEnvelope(t, w.Body.Bytes())
		items, ok := envelope["data"].([]any)
		require.True(t, ok, "data should be an array")
		require.Len(t, items, len(stateClosedSet))
		seen := make(map[string]bool)
		for _, item := range items {
			task, ok := item.(map[string]any)
			require.True(t, ok)
			seen[task["state"].(string)] = true
		}
		for _, state := range stateClosedSet {
			assert.True(t, seen[state], "unfiltered list should include the %s task", state)
		}
	})

	t.Run("InvalidStateRejected", func(t *testing.T) {
		// spec: CAD-032[0]
		req, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID.String()+"/tasks?state=archived", nil)
		w := httptest.NewRecorder()
		h.router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("InvalidLifecycleRejected", func(t *testing.T) {
		// spec: CAD-032[0]
		req, _ := http.NewRequest("GET", "/api/v1/contacts/"+contactID.String()+"/tasks?lifecycle=recurring", nil)
		w := httptest.NewRecorder()
		h.router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusBadRequest, w.Code)
	})

	t.Run("UnknownContactNotFound", func(t *testing.T) {
		// spec: CAD-032[0]
		req, _ := http.NewRequest("GET", "/api/v1/contacts/"+uuid.NewString()+"/tasks", nil)
		w := httptest.NewRecorder()
		h.router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusNotFound, w.Code)
	})
}

func postManualTask(t *testing.T, h *contactTaskAPIHarness, contactID uuid.UUID, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	require.NoError(t, err)
	req, _ := http.NewRequest("POST", "/api/v1/contacts/"+contactID.String()+"/tasks", bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.router.ServeHTTP(w, req)
	return w
}

func TestContactTaskAPI_CreateManualTask(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()

	h, cleanup := setupContactTaskAPIRouter(t)
	defer cleanup()

	contactID := createContactTaskTestContact(t, h, "Task Create API Test "+uuid.NewString()[:8])

	t.Run("ValidCreateReturns201WithCreatedTask", func(t *testing.T) {
		// spec: CAD-032[1]
		w := postManualTask(t, h, contactID, map[string]any{
			"kind": "reach_out",
			"text": "Follow up about the conference",
		})
		require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())

		envelope := decodeContactTaskEnvelope(t, w.Body.Bytes())
		data, ok := envelope["data"].(map[string]any)
		require.True(t, ok, "response should carry the created task under the data key")

		// Literal wire keys of the created task.
		taskID, ok := data["id"].(string)
		require.True(t, ok, "created task should expose an id key")
		_, err := uuid.Parse(taskID)
		require.NoError(t, err, "id should be a UUID")
		assert.Equal(t, contactID.String(), data["contact_id"])
		assert.Equal(t, "reach_out", data["kind"])
		assert.Equal(t, "manual", data["lifecycle"])
		assert.Equal(t, "managed", data["state"])
		assert.Equal(t, h.fakeTodoist.lastCreatedTaskID(), data["external_task_id"])
		require.Contains(t, data, "created_at")
		content, ok := data["content"].(string)
		require.True(t, ok, "created task should expose the content key")
		assert.Contains(t, content, "Follow up about the conference")
	})

	t.Run("PickableKindSendAccepted", func(t *testing.T) {
		// spec: CAD-032[1]
		w := postManualTask(t, h, contactID, map[string]any{
			"kind": "send",
			"text": "Send the signed contract",
		})
		require.Equal(t, http.StatusCreated, w.Code, "body: %s", w.Body.String())
		envelope := decodeContactTaskEnvelope(t, w.Body.Bytes())
		data, ok := envelope["data"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "send", data["kind"])
	})

	t.Run("NonPickableKindRejected", func(t *testing.T) {
		// spec: CAD-032[1]
		// action and meet are valid listing-filter kinds but NOT user-pickable
		// for manual creation.
		for _, kind := range []string{"action", "meet"} {
			w := postManualTask(t, h, contactID, map[string]any{
				"kind": kind,
				"text": "should be rejected",
			})
			assert.Equal(t, http.StatusBadRequest, w.Code, "kind=%s must not be user-pickable", kind)
		}
	})

	t.Run("TextLengthBoundary", func(t *testing.T) {
		// spec: CAD-032[1]
		w := postManualTask(t, h, contactID, map[string]any{
			"kind": "reminder",
			"text": strings.Repeat("a", 1000),
		})
		assert.Equal(t, http.StatusCreated, w.Code, "text of exactly 1000 chars should be accepted")

		w = postManualTask(t, h, contactID, map[string]any{
			"kind": "reminder",
			"text": strings.Repeat("a", 1001),
		})
		assert.Equal(t, http.StatusBadRequest, w.Code, "text of 1001 chars should be rejected")
	})

	t.Run("NotesLengthBoundary", func(t *testing.T) {
		// spec: CAD-032[1]
		w := postManualTask(t, h, contactID, map[string]any{
			"kind":  "reminder",
			"text":  "boundary notes task",
			"notes": strings.Repeat("n", 5000),
		})
		assert.Equal(t, http.StatusCreated, w.Code, "notes of exactly 5000 chars should be accepted")

		w = postManualTask(t, h, contactID, map[string]any{
			"kind":  "reminder",
			"text":  "boundary notes task overflow",
			"notes": strings.Repeat("n", 5001),
		})
		assert.Equal(t, http.StatusBadRequest, w.Code, "notes of 5001 chars should be rejected")
	})

	t.Run("MissingTextRejected", func(t *testing.T) {
		// spec: CAD-032[1]
		w := postManualTask(t, h, contactID, map[string]any{
			"kind": "reach_out",
		})
		assert.Equal(t, http.StatusBadRequest, w.Code, "text is required")
	})
}

func TestContactTaskAPI_DeleteTaskLink(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}
	t.Parallel()

	h, cleanup := setupContactTaskAPIRouter(t)
	defer cleanup()
	ctx := context.Background()

	suffix := uuid.NewString()[:8]
	contactID := createContactTaskTestContact(t, h, "Task Unlink API Test "+suffix)

	t.Run("UnlinkRemovesOnlyCRMLink", func(t *testing.T) {
		// spec: CAD-032[2]
		task, err := h.contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      contactID,
			Provider:       todoist.SourceName,
			Kind:           contacttask.KindReachOut,
			Lifecycle:      contacttask.LifecycleManual,
			ExternalTaskID: "ct-api-unlink-" + suffix,
			State:          "managed",
		})
		require.NoError(t, err)

		req, _ := http.NewRequest("DELETE", "/api/v1/contacts/"+contactID.String()+"/tasks/"+task.ID.String(), nil)
		w := httptest.NewRecorder()
		h.router.ServeHTTP(w, req)
		require.Equal(t, http.StatusNoContent, w.Code)
		assert.Zero(t, w.Body.Len(), "204 response should carry no content")

		// The CRM link row is gone...
		_, err = h.contactTaskRepo.GetContactTask(ctx, task.ID)
		assert.True(t, errors.Is(err, db.ErrNotFound), "CRM link should be removed, got err=%v", err)

		// ...but ONLY the link: the contact is untouched and no remote
		// provider mutation happened (the Todoist client was never called).
		_, err = h.contactRepo.GetContact(ctx, contactID)
		assert.NoError(t, err, "contact should be untouched by unlink")
		quickAdds, syncs := h.fakeTodoist.callCounts()
		assert.Zero(t, quickAdds, "unlink must not create remote tasks")
		assert.Zero(t, syncs, "unlink must not mutate the remote task")
	})

	t.Run("TaskOfDifferentContactNotFound", func(t *testing.T) {
		// spec: CAD-032[2]
		otherContactID := createContactTaskTestContact(t, h, "Task Unlink Other Contact "+suffix)
		otherTask, err := h.contactTaskRepo.CreateContactTask(ctx, repository.CreateContactTaskRequest{
			ContactID:      otherContactID,
			Provider:       todoist.SourceName,
			Kind:           contacttask.KindReachOut,
			Lifecycle:      contacttask.LifecycleManual,
			ExternalTaskID: "ct-api-unlink-other-" + suffix,
			State:          "managed",
		})
		require.NoError(t, err)

		req, _ := http.NewRequest("DELETE", "/api/v1/contacts/"+contactID.String()+"/tasks/"+otherTask.ID.String(), nil)
		w := httptest.NewRecorder()
		h.router.ServeHTTP(w, req)
		assert.Equal(t, http.StatusNotFound, w.Code, "task belonging to a different contact should be not-found")

		// The mismatched delete must not remove the other contact's link.
		fetched, err := h.contactTaskRepo.GetContactTask(ctx, otherTask.ID)
		require.NoError(t, err, "other contact's task link should survive the mismatched delete")
		assert.Equal(t, otherContactID, fetched.ContactID)
	})
}
