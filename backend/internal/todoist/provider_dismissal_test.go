package todoist

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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

// ============================================================================
// Test helpers
// ============================================================================

// countingRecorder is a test double for the interactionRecorder interface that
// fails the test immediately if RecordInteraction is called. Used to assert the
// dismissal path never records an interaction.
type countingRecorder struct {
	t     *testing.T
	count int
}

func (r *countingRecorder) RecordInteraction(_ context.Context, _ repository.RecordInteractionRequest) (*repository.Interaction, error) {
	r.t.Helper()
	r.count++
	r.t.Errorf("unexpected RecordInteraction call during dismissal test (count=%d)", r.count)
	return &repository.Interaction{ID: uuid.New()}, nil
}

type dismissalTestEnv struct {
	ctx             context.Context
	provider        *CadenceSyncProvider
	contactRepo     *repository.ContactRepository
	contactTaskRepo *repository.ContactTaskRepository
	recorder        *countingRecorder
	settings        Settings
	accountID       string
}

// setupDismissalTest builds a CadenceSyncProvider wired to a real DB and a test
// interactionRecorder that asserts it is never called. Skips if DATABASE_URL is
// unset, matching the pattern in backend/tests/followup_service_test.go.
func setupDismissalTest(t *testing.T) (*dismissalTestEnv, func()) {
	t.Helper()

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set, skipping integration test")
	}

	if err := db.RunMigrations(databaseURL, migrationsPathForTest()); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	ctx := context.Background()
	database, err := db.NewDatabase(ctx, config.DatabaseConfig{
		URL:               databaseURL,
		MaxConns:          config.DefaultDBMaxConns,
		MinConns:          config.DefaultDBMinConns,
		MaxConnIdleTime:   config.DefaultDBMaxConnIdleTime,
		MaxConnLifetime:   config.DefaultDBMaxConnLifetime,
		HealthCheckPeriod: config.DefaultDBHealthCheckPeriod,
	})
	require.NoError(t, err)

	contactRepo := repository.NewContactRepository(database.Queries)
	contactTaskRepo := repository.NewContactTaskRepository(database.Queries)
	cfg := config.TestConfig()
	recorder := &countingRecorder{t: t}

	provider := NewCadenceSyncProvider(
		nil, // oauthService: not consulted on the dismissal path
		contactTaskRepo,
		contactRepo,
		nil, // syncRepo: not consulted on the dismissal path
		cfg,
		recorder,
	)

	env := &dismissalTestEnv{
		ctx:             ctx,
		provider:        provider,
		contactRepo:     contactRepo,
		contactTaskRepo: contactTaskRepo,
		recorder:        recorder,
		settings: Settings{
			ProjectID:             "test-project",
			ProjectName:           "CRM",
			LabelID:               "test-label-id",
			LabelName:             "crm",
			IntegrationInstanceID: "test-instance",
		},
		accountID: "test-account",
	}

	return env, func() { database.Close() }
}

// migrationsPathForTest returns the absolute path to backend/migrations from the
// location of this test file. Same-package test, so we can't reuse
// tests/integration_test.go's getMigrationsPath helper.
func migrationsPathForTest() string {
	if path := os.Getenv("MIGRATIONS_PATH"); path != "" && filepath.IsAbs(path) {
		return path
	}
	_, filename, _, _ := runtime.Caller(0)
	testDir := filepath.Dir(filename) // backend/internal/todoist
	return filepath.Join(testDir, "..", "..", "migrations")
}

// createDismissalContact creates a contact with cadence set, a full set of
// populated date fields, and returns both the contact and a snapshot of the
// date fields so tests can assert they remain unchanged after dismissal.
type dateSnapshot struct {
	LastContacted     *time.Time
	LastInteractionAt *time.Time
	LastResponseAt    *time.Time
	LastOutreachAt    *time.Time
	ContactBy         *time.Time
}

func createDismissalContact(t *testing.T, env *dismissalTestEnv, nameSuffix string) (*repository.Contact, dateSnapshot) {
	t.Helper()

	cadence := "monthly"
	contact, err := env.contactRepo.CreateContact(env.ctx, repository.CreateContactRequest{
		FullName: "Dismissal " + nameSuffix + " " + uuid.New().String()[:8],
		Cadence:  &cadence,
	})
	require.NoError(t, err)

	// Seed all contact date fields so tests can assert they remain unchanged.
	// Use a mutual update to populate last_contacted, last_interaction_at,
	// last_response_at, contact_by in one call.
	now := accelerated.GetCurrentTime().UTC().Truncate(time.Second)
	mutualAt := now.AddDate(0, 0, -5)
	contactBy := mutualAt.AddDate(0, 1, 0)
	require.NoError(t, env.contactRepo.UpdateContactMutualFields(env.ctx, contact.ID, mutualAt, &contactBy, true))

	// Seed last_outreach_at separately — outbound updates only that field.
	outreachAt := now.AddDate(0, 0, -1)
	require.NoError(t, env.contactRepo.UpdateContactOutreachAt(env.ctx, contact.ID, outreachAt, true))

	// Reload to capture persisted values.
	reloaded, err := env.contactRepo.GetContact(env.ctx, contact.ID)
	require.NoError(t, err)

	snapshot := dateSnapshot{
		LastContacted:     copyTimePtr(reloaded.LastContacted),
		LastInteractionAt: copyTimePtr(reloaded.LastInteractionAt),
		LastResponseAt:    copyTimePtr(reloaded.LastResponseAt),
		LastOutreachAt:    copyTimePtr(reloaded.LastOutreachAt),
		ContactBy:         copyTimePtr(reloaded.ContactBy),
	}

	return reloaded, snapshot
}

func copyTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := *t
	return &v
}

// createFollowUpTask inserts a managed follow_up row for the given contact with
// the given external task ID. Returns the created task.
func createFollowUpTask(t *testing.T, env *dismissalTestEnv, contactID uuid.UUID, externalID string) *repository.ContactTask {
	t.Helper()
	task, err := env.contactTaskRepo.CreateContactTask(env.ctx, repository.CreateContactTaskRequest{
		ContactID:      contactID,
		Provider:       SourceName,
		Kind:           TaskKindFollowUp,
		ExternalTaskID: externalID,
		State:          string(repository.ContactTaskStateManaged),
		Metadata:       map[string]any{"due_date": "2099-01-01"},
	})
	require.NoError(t, err)
	return task
}

// assertDatesUnchanged reloads the contact and asserts every date field still
// matches the snapshot. Uses time.Equal to compare pointer values correctly.
func assertDatesUnchanged(t *testing.T, env *dismissalTestEnv, contactID uuid.UUID, snap dateSnapshot) {
	t.Helper()
	c, err := env.contactRepo.GetContact(env.ctx, contactID)
	require.NoError(t, err)
	assertTimePtrEqual(t, snap.LastContacted, c.LastContacted, "last_contacted")
	assertTimePtrEqual(t, snap.LastInteractionAt, c.LastInteractionAt, "last_interaction_at")
	assertTimePtrEqual(t, snap.LastResponseAt, c.LastResponseAt, "last_response_at")
	assertTimePtrEqual(t, snap.LastOutreachAt, c.LastOutreachAt, "last_outreach_at")
	assertTimePtrEqual(t, snap.ContactBy, c.ContactBy, "contact_by")
}

func assertTimePtrEqual(t *testing.T, want, got *time.Time, field string) {
	t.Helper()
	if want == nil && got == nil {
		return
	}
	if want == nil || got == nil {
		t.Errorf("%s: want=%v got=%v", field, want, got)
		return
	}
	if !want.Equal(*got) {
		t.Errorf("%s: want=%v got=%v", field, *want, *got)
	}
}

// ============================================================================
// Tests
// ============================================================================

// Case 1: dismissal via IsDeleted — no command emitted, state transitions, no
// interactions recorded, and every contact date field stays unchanged.
func TestFollowUpDismissal_IsDeleted(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	contact, snap := createDismissalContact(t, env, "IsDeleted")
	externalID := "td-task-" + uuid.New().String()[:8]
	task := createFollowUpTask(t, env, contact.ID, externalID)

	ok, commands := env.provider.processItem(env.ctx, SyncItem{
		ID:        externalID,
		IsDeleted: true,
		Labels:    []string{env.settings.LabelName},
		Deadline:  &SyncDate{Date: "2099-01-01"},
	}, env.settings, env.accountID)

	assert.True(t, ok, "processItem should return true (task was handled)")
	assert.Empty(t, commands, "no Todoist commands should be emitted when item is already deleted")
	assert.Equal(t, 0, env.recorder.count, "RecordInteraction must not be called during dismissal")

	assertDismissedAndInvariants(t, env, contact.ID, task.ID, snap)
}

// Case 2: dismissal via label removed — exactly one ItemClose command is
// emitted, targeting the follow-up's external task ID.
func TestFollowUpDismissal_LabelRemoved(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	contact, snap := createDismissalContact(t, env, "LabelRemoved")
	externalID := "td-task-" + uuid.New().String()[:8]
	task := createFollowUpTask(t, env, contact.ID, externalID)

	ok, commands := env.provider.processItem(env.ctx, SyncItem{
		ID:        externalID,
		IsDeleted: false,
		Labels:    []string{}, // CRM label removed
		Deadline:  &SyncDate{Date: "2099-01-01"},
	}, env.settings, env.accountID)

	assert.True(t, ok)
	require.Len(t, commands, 1, "exactly one ItemClose command expected")
	assert.Equal(t, "item_close", commands[0].Type)
	assert.Equal(t, externalID, commands[0].Args["id"])
	assert.Equal(t, 0, env.recorder.count)

	assertDismissedAndInvariants(t, env, contact.ID, task.ID, snap)
}

// Case 3: dismissal via deadline removed — same as case 2 but triggered by a
// nil Deadline (CRM label still present).
func TestFollowUpDismissal_DeadlineRemoved(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	contact, snap := createDismissalContact(t, env, "DeadlineRemoved")
	externalID := "td-task-" + uuid.New().String()[:8]
	task := createFollowUpTask(t, env, contact.ID, externalID)

	ok, commands := env.provider.processItem(env.ctx, SyncItem{
		ID:        externalID,
		IsDeleted: false,
		Labels:    []string{env.settings.LabelName},
		Deadline:  nil,
	}, env.settings, env.accountID)

	assert.True(t, ok)
	require.Len(t, commands, 1)
	assert.Equal(t, "item_close", commands[0].Type)
	assert.Equal(t, externalID, commands[0].Args["id"])
	assert.Equal(t, 0, env.recorder.count)

	assertDismissedAndInvariants(t, env, contact.ID, task.ID, snap)
}

// Case 4: dismissing a follow-up must not touch a coexisting managed cadence
// task for the same contact.
func TestFollowUpDismissal_CadenceTaskUnaffected(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	contact, _ := createDismissalContact(t, env, "CadenceUnaffected")

	cadenceExtID := "td-cadence-" + uuid.New().String()[:8]
	cadenceTask, err := env.contactTaskRepo.CreateContactTask(env.ctx, repository.CreateContactTaskRequest{
		ContactID:      contact.ID,
		Provider:       SourceName,
		Kind:           TaskKindCadence,
		ExternalTaskID: cadenceExtID,
		State:          string(repository.ContactTaskStateManaged),
		Metadata:       map[string]any{"synced_deadline": "2099-01-01"},
	})
	require.NoError(t, err)

	followupExtID := "td-followup-" + uuid.New().String()[:8]
	_ = createFollowUpTask(t, env, contact.ID, followupExtID)

	_, _ = env.provider.processItem(env.ctx, SyncItem{
		ID:        followupExtID,
		IsDeleted: true,
		Labels:    []string{env.settings.LabelName},
		Deadline:  &SyncDate{Date: "2099-01-01"},
	}, env.settings, env.accountID)

	reloaded, err := env.contactTaskRepo.GetContactTask(env.ctx, cadenceTask.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.ContactTaskStateManaged, reloaded.State, "cadence task must remain managed")
	assert.Equal(t, cadenceExtID, reloaded.ExternalTaskID, "cadence external ID must be unchanged")
}

// Case 5: after dismissal, a subsequent processItem call on the same external
// task ID must skip the row via the state != managed early-return, not
// re-emitting any commands.
func TestFollowUpDismissal_SubsequentProcessItemSkipsDismissedRow(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	contact, _ := createDismissalContact(t, env, "Subsequent")
	externalID := "td-task-" + uuid.New().String()[:8]
	_ = createFollowUpTask(t, env, contact.ID, externalID)

	// First dismissal.
	_, _ = env.provider.processItem(env.ctx, SyncItem{
		ID:        externalID,
		IsDeleted: false,
		Labels:    []string{env.settings.LabelName},
		Deadline:  nil,
	}, env.settings, env.accountID)

	// Second call — simulates the next sync tick seeing the same task.
	ok, commands := env.provider.processItem(env.ctx, SyncItem{
		ID:        externalID,
		IsDeleted: false,
		Labels:    []string{env.settings.LabelName},
		Deadline:  nil,
	}, env.settings, env.accountID)

	assert.False(t, ok, "subsequent processItem should report the row was skipped")
	assert.Empty(t, commands, "no commands should be emitted on subsequent calls")
	assert.Equal(t, 0, env.recorder.count)
}

// Case 6: FindPendingFollowUp must ignore dismissed rows. Regression guardrail
// for the existing SQL (state='managed' filter) against the new state constant.
func TestFollowUpDismissal_FindPendingFollowUpIgnoresDismissed(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	contact, _ := createDismissalContact(t, env, "FindPending")
	task := createFollowUpTask(t, env, contact.ID, "td-"+uuid.New().String()[:8])

	// Transition directly to dismissed via the repository.
	_, err := env.contactTaskRepo.UpdateContactTaskState(env.ctx, task.ID, repository.ContactTaskStateDismissed)
	require.NoError(t, err)

	_, err = env.contactTaskRepo.FindPendingFollowUp(env.ctx, contact.ID)
	assert.True(t, errors.Is(err, db.ErrNotFound), "FindPendingFollowUp must treat dismissed as absent")
}

// Case 7: has_followup / no_followup contact filters must ignore dismissed rows.
func TestFollowUpDismissal_ContactFollowUpFiltersIgnoreDismissed(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	contact, _ := createDismissalContact(t, env, "Filters")
	task := createFollowUpTask(t, env, contact.ID, "td-"+uuid.New().String()[:8])
	_, err := env.contactTaskRepo.UpdateContactTaskState(env.ctx, task.ID, repository.ContactTaskStateDismissed)
	require.NoError(t, err)

	// Helper to check whether this contact is in a filter result.
	contactInFilter := func(filter string) bool {
		contacts, err := env.contactRepo.ListContacts(env.ctx, repository.ListContactsParams{
			Limit:          1000,
			FollowupFilter: filter,
		})
		require.NoError(t, err)
		for _, c := range contacts {
			if c.ID == contact.ID {
				return true
			}
		}
		return false
	}

	assert.False(t, contactInFilter("has_followup"), "dismissed row must not count as pending follow-up")
	assert.True(t, contactInFilter("no_followup"), "contact with only a dismissed follow-up should appear in no_followup")
}

// Case 8a: dispatch for cadence is unchanged — a cadence task hit with a skip
// trigger still routes through handleSkipTrigger (observable via a new
// item_add command being returned).
func TestFollowUpDismissal_CadenceDispatchUnchanged(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	contact, _ := createDismissalContact(t, env, "CadenceDispatch")

	cadenceExtID := "td-cadence-" + uuid.New().String()[:8]
	_, err := env.contactTaskRepo.CreateContactTask(env.ctx, repository.CreateContactTaskRequest{
		ContactID:      contact.ID,
		Provider:       SourceName,
		Kind:           TaskKindCadence,
		ExternalTaskID: cadenceExtID,
		State:          string(repository.ContactTaskStateManaged),
		Metadata:       map[string]any{"synced_deadline": "2099-01-01"},
	})
	require.NoError(t, err)

	ok, commands := env.provider.processItem(env.ctx, SyncItem{
		ID:        cadenceExtID,
		IsDeleted: true,
		Labels:    []string{env.settings.LabelName},
		Deadline:  &SyncDate{Date: "2099-01-01"},
	}, env.settings, env.accountID)

	assert.True(t, ok)
	require.NotEmpty(t, commands, "handleSkipTrigger should return an item_add command for the replacement cadence task")

	sawItemAdd := false
	for _, cmd := range commands {
		if cmd.Type == "item_add" {
			sawItemAdd = true
		}
	}
	assert.True(t, sawItemAdd, "expected at least one item_add command from cadence skip-trigger")
}

// Case 8b: dispatch for action is unchanged — an action task being deleted
// transitions the local row to unmanaged (via handleActionTaskTriggers).
func TestFollowUpDismissal_ActionDispatchUnchanged(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	contact, _ := createDismissalContact(t, env, "ActionDispatch")

	actionExtID := "td-action-" + uuid.New().String()[:8]
	action, err := env.contactTaskRepo.CreateContactTask(env.ctx, repository.CreateContactTaskRequest{
		ContactID:      contact.ID,
		Provider:       SourceName,
		Kind:           TaskKindAction,
		ExternalTaskID: actionExtID,
		State:          string(repository.ContactTaskStateManaged),
		Metadata:       map[string]any{},
	})
	require.NoError(t, err)

	ok, _ := env.provider.processItem(env.ctx, SyncItem{
		ID:        actionExtID,
		IsDeleted: true,
		Labels:    []string{env.settings.LabelName},
		Deadline:  &SyncDate{Date: "2099-01-01"},
	}, env.settings, env.accountID)

	assert.True(t, ok)

	reloaded, err := env.contactTaskRepo.GetContactTask(env.ctx, action.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.ContactTaskStateUnmanaged, reloaded.State, "action task must transition to unmanaged on delete")
}

// ============================================================================
// Shared assertion helpers
// ============================================================================

// assertDismissedAndInvariants rolls up the post-dismissal checks that all
// three trigger-variant cases share: state transitions, no new interaction, no
// new follow-up, HasPendingFollowUp false, and every date field unchanged.
func assertDismissedAndInvariants(t *testing.T, env *dismissalTestEnv, contactID uuid.UUID, taskID uuid.UUID, snap dateSnapshot) {
	t.Helper()

	reloaded, err := env.contactTaskRepo.GetContactTask(env.ctx, taskID)
	require.NoError(t, err)
	assert.Equal(t, repository.ContactTaskStateDismissed, reloaded.State, "follow-up must be dismissed")

	// No pending follow-up remains.
	_, err = env.contactTaskRepo.FindPendingFollowUp(env.ctx, contactID)
	assert.True(t, errors.Is(err, db.ErrNotFound), "no pending follow-up should exist after dismissal")

	// No new follow-up rows were created (count stays at 1 — the dismissed one).
	tasks, err := env.contactTaskRepo.ListContactTasksByContact(env.ctx, contactID)
	require.NoError(t, err)
	followupCount := 0
	for _, tk := range tasks {
		if tk.Kind == TaskKindFollowUp {
			followupCount++
		}
	}
	assert.Equal(t, 1, followupCount, "no new follow-up rows should have been created")

	// All contact date fields unchanged.
	assertDatesUnchanged(t, env, contactID, snap)
}
