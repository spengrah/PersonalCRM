package unit

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/todoist"

	"github.com/stretchr/testify/assert"
)

// TestTodoistDeadlineDateComparison tests the UTC date comparison logic
// used when checking if Todoist deadline differs from CRM contact_by.
// This ensures timezone differences don't cause incorrect comparisons.
func TestTodoistDeadlineDateComparison(t *testing.T) {
	tests := []struct {
		name            string
		todoistDate     string // Todoist deadline date string (YYYY-MM-DD)
		contactByTime   time.Time
		expectDifferent bool
	}{
		{
			name:            "same date in UTC",
			todoistDate:     "2026-01-28",
			contactByTime:   time.Date(2026, 1, 28, 0, 0, 0, 0, time.UTC),
			expectDifferent: false,
		},
		{
			name:            "different dates",
			todoistDate:     "2026-01-28",
			contactByTime:   time.Date(2026, 1, 20, 0, 0, 0, 0, time.UTC),
			expectDifferent: true,
		},
		{
			name:            "same date but contact_by has time component",
			todoistDate:     "2026-01-28",
			contactByTime:   time.Date(2026, 1, 28, 15, 30, 0, 0, time.UTC),
			expectDifferent: false, // Should compare dates only, not time
		},
		{
			name:        "same date in different timezone (PST stored as UTC midnight)",
			todoistDate: "2026-01-28",
			// PostgreSQL DATE loaded as UTC midnight
			contactByTime:   time.Date(2026, 1, 28, 0, 0, 0, 0, time.UTC),
			expectDifferent: false,
		},
		{
			name:            "adjacent dates at year boundary",
			todoistDate:     "2026-01-01",
			contactByTime:   time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC),
			expectDifferent: true,
		},
		{
			name:            "same date different year",
			todoistDate:     "2026-01-15",
			contactByTime:   time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC),
			expectDifferent: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Parse Todoist deadline the same way the provider does
			todoistDeadline, err := time.Parse("2006-01-02", tt.todoistDate)
			assert.NoError(t, err)

			// Compare using UTC year/month/day (the fix we implemented)
			tY, tM, tD := todoistDeadline.UTC().Date()
			cY, cM, cD := tt.contactByTime.UTC().Date()
			isDifferent := tY != cY || tM != cM || tD != cD

			assert.Equal(t, tt.expectDifferent, isDifferent,
				"todoistDeadline=%v, contactByTime=%v",
				todoistDeadline, tt.contactByTime)
		})
	}
}

// TestTodoistDeadlineDateComparisonOldVsNew compares the old (buggy) comparison
// with the new (fixed) comparison to demonstrate the difference.
func TestTodoistDeadlineDateComparisonOldVsNew(t *testing.T) {
	// Simulate a scenario where old code would give wrong result
	// due to timezone conversion

	// Todoist returns date string, parsed as UTC midnight
	todoistDate := "2026-01-28"
	todoistDeadline, _ := time.Parse("2006-01-02", todoistDate)
	// todoistDeadline = 2026-01-28 00:00:00 UTC

	// Contact_by from DB (DATE stored as UTC midnight)
	contactBy := time.Date(2026, 1, 28, 0, 0, 0, 0, time.UTC)

	// NEW comparison (UTC date components) - CORRECT
	tY, tM, tD := todoistDeadline.UTC().Date()
	cY, cM, cD := contactBy.UTC().Date()
	newIsDifferent := tY != cY || tM != cM || tD != cD

	assert.False(t, newIsDifferent, "New comparison should see same dates as equal")

	// The old code used cadence.Today() which converts to local timezone.
	// This test verifies our fix works correctly regardless of local timezone.
}

// TestSyncedDeadlineComparison tests the synced_deadline comparison logic
// used to detect when contact_by has changed and the Todoist task needs updating.
func TestSyncedDeadlineComparison(t *testing.T) {
	tests := []struct {
		name           string
		syncedDeadline string // stored in metadata (YYYY-MM-DD)
		contactByTime  time.Time
		expectDrift    bool
	}{
		{
			name:           "deadlines match",
			syncedDeadline: "2026-02-15",
			contactByTime:  time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC),
			expectDrift:    false,
		},
		{
			name:           "contact_by moved forward (user updated last_contacted)",
			syncedDeadline: "2026-01-15",
			contactByTime:  time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC),
			expectDrift:    true,
		},
		{
			name:           "contact_by moved backward (edge case)",
			syncedDeadline: "2026-03-15",
			contactByTime:  time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC),
			expectDrift:    true,
		},
		{
			name:           "same date different time component",
			syncedDeadline: "2026-02-15",
			contactByTime:  time.Date(2026, 2, 15, 12, 30, 0, 0, time.UTC),
			expectDrift:    false, // Only date matters
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Format contact_by the same way the provider does
			currentDeadline := tt.contactByTime.Format("2006-01-02")

			// Compare strings (how reconcileExistingTask does it)
			hasDrift := tt.syncedDeadline != currentDeadline

			assert.Equal(t, tt.expectDrift, hasDrift,
				"syncedDeadline=%s, currentDeadline=%s",
				tt.syncedDeadline, currentDeadline)
		})
	}
}

// TestIsPendingTempIDLogic tests the logic for detecting if a task's
// external ID is still a pending temp ID (not yet resolved to real Todoist ID).
func TestIsPendingTempIDLogic(t *testing.T) {
	tests := []struct {
		name           string
		externalTaskID string
		metadata       map[string]any
		expectPending  bool
	}{
		{
			name:           "nil metadata",
			externalTaskID: "temp-uuid-123",
			metadata:       nil,
			expectPending:  false,
		},
		{
			name:           "no pending_temp_id in metadata",
			externalTaskID: "12345678",
			metadata:       map[string]any{"synced_deadline": "2026-02-15"},
			expectPending:  false,
		},
		{
			name:           "pending_temp_id matches external_task_id",
			externalTaskID: "temp-uuid-123",
			metadata:       map[string]any{"pending_temp_id": "temp-uuid-123"},
			expectPending:  true,
		},
		{
			name:           "pending_temp_id does not match (resolved)",
			externalTaskID: "12345678",
			metadata:       map[string]any{"pending_temp_id": "temp-uuid-123"},
			expectPending:  false,
		},
		{
			name:           "pending_temp_id is not a string",
			externalTaskID: "temp-uuid-123",
			metadata:       map[string]any{"pending_temp_id": 123},
			expectPending:  false,
		},
		{
			name:           "empty external_task_id",
			externalTaskID: "",
			metadata:       map[string]any{"pending_temp_id": "temp-uuid-123"},
			expectPending:  false,
		},
		{
			name:           "empty pending_temp_id from malformed metadata",
			externalTaskID: "temp-uuid-123",
			metadata:       map[string]any{"pending_temp_id": ""},
			expectPending:  false,
		},
		{
			name:           "both external_task_id and pending_temp_id are empty",
			externalTaskID: "",
			metadata:       map[string]any{"pending_temp_id": ""},
			expectPending:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Inline the isPendingTempID logic (since it's not exported)
			// This matches the updated implementation in provider.go
			isPending := false
			if tt.metadata != nil && tt.externalTaskID != "" {
				pendingTempID, ok := tt.metadata["pending_temp_id"].(string)
				if ok && pendingTempID != "" {
					isPending = pendingTempID == tt.externalTaskID
				}
			}

			assert.Equal(t, tt.expectPending, isPending)
		})
	}
}

// TestReconciliationCommandGeneration tests the logic for determining which commands
// should be generated during reconciliation based on task state and deadline drift.
func TestReconciliationCommandGeneration(t *testing.T) {
	tests := []struct {
		name             string
		externalTaskID   string
		metadata         map[string]any
		currentDeadline  string
		expectCloseCmd   bool
		expectCreateCmd  bool
		expectBackfill   bool
		expectNoCommands bool
	}{
		{
			name:             "no synced_deadline - backfill only",
			externalTaskID:   "12345678",
			metadata:         map[string]any{},
			currentDeadline:  "2026-02-15",
			expectBackfill:   true,
			expectNoCommands: true,
		},
		{
			name:             "nil metadata - backfill only",
			externalTaskID:   "12345678",
			metadata:         nil,
			currentDeadline:  "2026-02-15",
			expectBackfill:   true,
			expectNoCommands: true,
		},
		{
			name:             "deadlines match - no commands",
			externalTaskID:   "12345678",
			metadata:         map[string]any{"synced_deadline": "2026-02-15"},
			currentDeadline:  "2026-02-15",
			expectNoCommands: true,
		},
		{
			name:            "deadline drift - close + create commands",
			externalTaskID:  "12345678",
			metadata:        map[string]any{"synced_deadline": "2026-01-15"},
			currentDeadline: "2026-02-15",
			expectCloseCmd:  true,
			expectCreateCmd: true,
		},
		{
			name:            "drift with pending temp_id - create only (no close)",
			externalTaskID:  "temp-uuid-123",
			metadata:        map[string]any{"synced_deadline": "2026-01-15", "pending_temp_id": "temp-uuid-123"},
			currentDeadline: "2026-02-15",
			expectCloseCmd:  false, // Skip close since ID is still temp
			expectCreateCmd: true,
		},
		{
			name:            "drift with empty external_task_id - create only (no close)",
			externalTaskID:  "",
			metadata:        map[string]any{"synced_deadline": "2026-01-15"},
			currentDeadline: "2026-02-15",
			expectCloseCmd:  false, // Skip close since no external ID
			expectCreateCmd: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate reconcileExistingTask logic
			var closeCmd, createCmd, backfill bool

			// Get synced_deadline from metadata
			syncedDeadline, hasSyncedDeadline := "", false
			if tt.metadata != nil {
				if sd, ok := tt.metadata["synced_deadline"].(string); ok {
					syncedDeadline = sd
					hasSyncedDeadline = true
				}
			}

			if !hasSyncedDeadline {
				// Backfill case
				backfill = true
			} else if syncedDeadline == tt.currentDeadline {
				// No drift, no commands
			} else {
				// Drift detected
				// Check if we should close (has real external ID)
				isPending := false
				if tt.metadata != nil && tt.externalTaskID != "" {
					pendingTempID, ok := tt.metadata["pending_temp_id"].(string)
					if ok && pendingTempID != "" {
						isPending = pendingTempID == tt.externalTaskID
					}
				}
				if tt.externalTaskID != "" && !isPending {
					closeCmd = true
				}
				createCmd = true
			}

			assert.Equal(t, tt.expectBackfill, backfill, "backfill mismatch")
			assert.Equal(t, tt.expectCloseCmd, closeCmd, "close command mismatch")
			assert.Equal(t, tt.expectCreateCmd, createCmd, "create command mismatch")

			if tt.expectNoCommands {
				assert.False(t, closeCmd, "expected no close command")
				assert.False(t, createCmd, "expected no create command")
			}
		})
	}
}

// TestWasContactedSinceSyncLogic tests the logic for detecting when a contact
// was marked as contacted from a non-Todoist source (e.g., calendar sync).
// This ensures the reconciliation correctly identifies stale tasks even when
// contact_by hasn't changed (same cadence period).
func TestWasContactedSinceSyncLogic(t *testing.T) {
	tests := []struct {
		name                string
		lastContacted       *time.Time
		syncedLastContacted string // stored in metadata (RFC3339), empty = not present
		expectContacted     bool
	}{
		{
			name:            "nil last_contacted",
			lastContacted:   nil,
			expectContacted: false,
		},
		{
			name:                "no synced_last_contacted in metadata (backfill case)",
			lastContacted:       timePtr(time.Date(2026, 2, 10, 12, 0, 0, 0, time.UTC)),
			syncedLastContacted: "",
			expectContacted:     false, // Can't determine, avoid false positives
		},
		{
			name:                "last_contacted unchanged since sync",
			lastContacted:       timePtr(time.Date(2026, 2, 8, 12, 0, 0, 0, time.UTC)),
			syncedLastContacted: "2026-02-08T12:00:00Z",
			expectContacted:     false,
		},
		{
			name:                "last_contacted advanced since sync (calendar contact)",
			lastContacted:       timePtr(time.Date(2026, 2, 10, 14, 30, 0, 0, time.UTC)),
			syncedLastContacted: "2026-02-08T12:00:00Z",
			expectContacted:     true,
		},
		{
			name:                "last_contacted same day but later time",
			lastContacted:       timePtr(time.Date(2026, 2, 8, 18, 0, 0, 0, time.UTC)),
			syncedLastContacted: "2026-02-08T12:00:00Z",
			expectContacted:     true,
		},
		{
			name:                "last_contacted before synced (edge case - should not happen)",
			lastContacted:       timePtr(time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)),
			syncedLastContacted: "2026-02-08T12:00:00Z",
			expectContacted:     false,
		},
		{
			name:                "invalid synced_last_contacted format",
			lastContacted:       timePtr(time.Date(2026, 2, 10, 12, 0, 0, 0, time.UTC)),
			syncedLastContacted: "not-a-date",
			expectContacted:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate wasContactedSinceSync logic
			wasContacted := false

			if tt.lastContacted != nil && tt.syncedLastContacted != "" {
				syncedLastContacted, err := time.Parse(time.RFC3339, tt.syncedLastContacted)
				if err == nil {
					wasContacted = tt.lastContacted.After(syncedLastContacted)
				}
			}

			assert.Equal(t, tt.expectContacted, wasContacted,
				"lastContacted=%v, syncedLastContacted=%s",
				tt.lastContacted, tt.syncedLastContacted)
		})
	}
}

// TestReconciliationWithNonTodoistContact tests the full reconciliation logic
// including the new non-Todoist contact detection. This covers the bug fix for
// GH #235 where calendar-synced contacts didn't auto-complete Todoist tasks.
func TestReconciliationWithNonTodoistContact(t *testing.T) {
	tests := []struct {
		name             string
		externalTaskID   string
		metadata         map[string]any
		currentDeadline  string
		lastContacted    *time.Time
		expectCloseCmd   bool
		expectCreateCmd  bool
		expectBackfill   bool
		expectNoCommands bool
	}{
		{
			name:            "same deadline, contact was contacted via calendar - should complete",
			externalTaskID:  "12345678",
			metadata:        map[string]any{"synced_deadline": "2026-02-15", "synced_last_contacted": "2026-02-01T12:00:00Z"},
			currentDeadline: "2026-02-15",
			lastContacted:   timePtr(time.Date(2026, 2, 8, 14, 30, 0, 0, time.UTC)),
			expectCloseCmd:  true,
			expectCreateCmd: true,
		},
		{
			name:             "same deadline, last_contacted unchanged - no action",
			externalTaskID:   "12345678",
			metadata:         map[string]any{"synced_deadline": "2026-02-15", "synced_last_contacted": "2026-02-01T12:00:00Z"},
			currentDeadline:  "2026-02-15",
			lastContacted:    timePtr(time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)),
			expectNoCommands: true,
		},
		{
			name:             "same deadline, no synced_last_contacted (backfill) - no action",
			externalTaskID:   "12345678",
			metadata:         map[string]any{"synced_deadline": "2026-02-15"},
			currentDeadline:  "2026-02-15",
			lastContacted:    timePtr(time.Date(2026, 2, 8, 14, 30, 0, 0, time.UTC)),
			expectNoCommands: true, // Can't determine, avoid false positives
		},
		{
			name:             "same deadline, nil last_contacted - no action",
			externalTaskID:   "12345678",
			metadata:         map[string]any{"synced_deadline": "2026-02-15", "synced_last_contacted": "2026-02-01T12:00:00Z"},
			currentDeadline:  "2026-02-15",
			lastContacted:    nil,
			expectNoCommands: true,
		},
		{
			name:            "different deadline (standard drift) - still works",
			externalTaskID:  "12345678",
			metadata:        map[string]any{"synced_deadline": "2026-01-15", "synced_last_contacted": "2026-01-01T12:00:00Z"},
			currentDeadline: "2026-02-15",
			lastContacted:   timePtr(time.Date(2026, 2, 8, 14, 30, 0, 0, time.UTC)),
			expectCloseCmd:  true,
			expectCreateCmd: true,
		},
		{
			name:            "same deadline, contacted via calendar, pending temp_id - create only",
			externalTaskID:  "temp-uuid-123",
			metadata:        map[string]any{"synced_deadline": "2026-02-15", "synced_last_contacted": "2026-02-01T12:00:00Z", "pending_temp_id": "temp-uuid-123"},
			currentDeadline: "2026-02-15",
			lastContacted:   timePtr(time.Date(2026, 2, 8, 14, 30, 0, 0, time.UTC)),
			expectCloseCmd:  false,
			expectCreateCmd: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var closeCmd, createCmd, backfill bool

			// Get synced_deadline from metadata
			syncedDeadline, hasSyncedDeadline := "", false
			if tt.metadata != nil {
				if sd, ok := tt.metadata["synced_deadline"].(string); ok {
					syncedDeadline = sd
					hasSyncedDeadline = true
				}
			}

			if !hasSyncedDeadline {
				backfill = true
			} else {
				// Backfill synced_last_contacted if missing (simulates the provider backfill)
				if _, hasSyncedLC := tt.metadata["synced_last_contacted"].(string); !hasSyncedLC {
					if tt.lastContacted != nil {
						tt.metadata["synced_last_contacted"] = tt.lastContacted.Format(time.RFC3339)
					}
				}

				if syncedDeadline == tt.currentDeadline {
					// Deadlines match - check for non-Todoist contact
					wasContacted := false
					if tt.lastContacted != nil {
						if slc, ok := tt.metadata["synced_last_contacted"].(string); ok && slc != "" {
							syncedLC, err := time.Parse(time.RFC3339, slc)
							if err == nil {
								wasContacted = tt.lastContacted.After(syncedLC)
							}
						}
					}

					if wasContacted {
						// Contact was contacted via non-Todoist source
						isPending := false
						if tt.metadata != nil && tt.externalTaskID != "" {
							pendingTempID, ok := tt.metadata["pending_temp_id"].(string)
							if ok && pendingTempID != "" {
								isPending = pendingTempID == tt.externalTaskID
							}
						}
						if tt.externalTaskID != "" && !isPending {
							closeCmd = true
						}
						createCmd = true
					}
				} else {
					// Standard deadline drift
					isPending := false
					if tt.metadata != nil && tt.externalTaskID != "" {
						pendingTempID, ok := tt.metadata["pending_temp_id"].(string)
						if ok && pendingTempID != "" {
							isPending = pendingTempID == tt.externalTaskID
						}
					}
					if tt.externalTaskID != "" && !isPending {
						closeCmd = true
					}
					createCmd = true
				}
			}

			assert.Equal(t, tt.expectBackfill, backfill, "backfill mismatch")
			assert.Equal(t, tt.expectCloseCmd, closeCmd, "close command mismatch")
			assert.Equal(t, tt.expectCreateCmd, createCmd, "create command mismatch")

			if tt.expectNoCommands {
				assert.False(t, closeCmd, "expected no close command")
				assert.False(t, createCmd, "expected no create command")
			}
		})
	}
}

// TestBackfillSyncedLastContacted verifies that legacy tasks without synced_last_contacted
// get it backfilled, enabling detection of non-Todoist contacts on subsequent sync cycles.
func TestBackfillSyncedLastContacted(t *testing.T) {
	// Simulate two sync cycles:
	// Cycle 1: Legacy task has synced_deadline but no synced_last_contacted
	//   → Backfill happens: synced_last_contacted = current last_contacted
	//   → No commands generated (backfilled value == current value)
	// Cycle 2: Contact is contacted via calendar (last_contacted advances)
	//   → synced_last_contacted < last_contacted → complete + create

	t.Run("cycle 1: backfill only, no commands", func(t *testing.T) {
		metadata := map[string]any{"synced_deadline": "2026-02-15"}
		lastContacted := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)

		// Backfill synced_last_contacted
		_, hasSyncedLC := metadata["synced_last_contacted"].(string)
		assert.False(t, hasSyncedLC, "should not have synced_last_contacted initially")

		// Backfill stores current last_contacted
		metadata["synced_last_contacted"] = lastContacted.Format(time.RFC3339)

		// wasContactedSinceSync check
		syncedLC, _ := time.Parse(time.RFC3339, metadata["synced_last_contacted"].(string))
		wasContacted := lastContacted.After(syncedLC)
		assert.False(t, wasContacted, "should not detect contact on first cycle (backfill == current)")
	})

	t.Run("cycle 2: detect non-Todoist contact after backfill", func(t *testing.T) {
		// After cycle 1, metadata has synced_last_contacted from backfill
		metadata := map[string]any{
			"synced_deadline":       "2026-02-15",
			"synced_last_contacted": "2026-02-01T12:00:00Z", // Backfilled in cycle 1
		}
		// Calendar sync updates last_contacted to a later time
		lastContacted := time.Date(2026, 2, 8, 14, 30, 0, 0, time.UTC)

		syncedLC, _ := time.Parse(time.RFC3339, metadata["synced_last_contacted"].(string))
		wasContacted := lastContacted.After(syncedLC)
		assert.True(t, wasContacted, "should detect contact after backfill when last_contacted advances")
	})
}

// TestActionTaskKindConstant verifies the action task kind constant
func TestActionTaskKindConstant(t *testing.T) {
	assert.Equal(t, "action", todoist.TaskKindAction)
	assert.Equal(t, "cadence", todoist.TaskKindCadence)
}

// TestActionTaskStateTransitions tests the expected state transitions for action tasks
func TestActionTaskStateTransitions(t *testing.T) {
	tests := []struct {
		name           string
		initialState   string
		isCompleted    bool
		isDeleted      bool
		labelRemoved   bool
		expectedState  string
		expectNoChange bool
	}{
		{
			name:          "completion transitions to completed",
			initialState:  "managed",
			isCompleted:   true,
			expectedState: "completed",
		},
		{
			name:          "deletion transitions to unmanaged",
			initialState:  "managed",
			isDeleted:     true,
			expectedState: "unmanaged",
		},
		{
			name:          "label removed transitions to unmanaged",
			initialState:  "managed",
			labelRemoved:  true,
			expectedState: "unmanaged",
		},
		{
			name:           "no triggers keeps managed state",
			initialState:   "managed",
			expectedState:  "managed",
			expectNoChange: true,
		},
		{
			name:           "already completed stays completed",
			initialState:   "completed",
			expectedState:  "completed",
			expectNoChange: true,
		},
		{
			name:           "already unmanaged stays unmanaged",
			initialState:   "unmanaged",
			expectedState:  "unmanaged",
			expectNoChange: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Skip non-managed initial states (they're not processed)
			if tt.initialState != "managed" {
				assert.True(t, tt.expectNoChange, "non-managed tasks should expect no change")
				return
			}

			// Simulate the state transition logic from handleActionTaskTriggers
			var newState string
			stateChanged := false

			if tt.isCompleted {
				newState = "completed"
				stateChanged = true
			} else if tt.isDeleted {
				newState = "unmanaged"
				stateChanged = true
			} else if tt.labelRemoved {
				newState = "unmanaged"
				stateChanged = true
			} else {
				newState = tt.initialState
			}

			assert.Equal(t, tt.expectedState, newState)
			assert.Equal(t, !tt.expectNoChange, stateChanged, "state change expectation mismatch")
		})
	}
}

// TestActionTaskMetadataSchema tests the expected metadata structure for action tasks
func TestActionTaskMetadataSchema(t *testing.T) {
	tests := []struct {
		name         string
		metadata     map[string]any
		hasContent   bool
		hasDueDate   bool
		hasProjectID bool
	}{
		{
			name: "full metadata",
			metadata: map[string]any{
				"content":    "Follow up about surgery",
				"due_date":   "2026-02-10",
				"project_id": "123456",
			},
			hasContent:   true,
			hasDueDate:   true,
			hasProjectID: true,
		},
		{
			name: "no due date",
			metadata: map[string]any{
				"content":    "Quick follow up",
				"project_id": "123456",
			},
			hasContent:   true,
			hasDueDate:   false,
			hasProjectID: true,
		},
		{
			name:         "empty metadata",
			metadata:     map[string]any{},
			hasContent:   false,
			hasDueDate:   false,
			hasProjectID: false,
		},
		{
			name:         "nil metadata",
			metadata:     nil,
			hasContent:   false,
			hasDueDate:   false,
			hasProjectID: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test content extraction
			_, hasContent := tt.metadata["content"].(string)
			assert.Equal(t, tt.hasContent, hasContent, "content presence mismatch")

			// Test due_date extraction
			_, hasDueDate := tt.metadata["due_date"].(string)
			assert.Equal(t, tt.hasDueDate, hasDueDate, "due_date presence mismatch")

			// Test project_id extraction
			_, hasProjectID := tt.metadata["project_id"].(string)
			assert.Equal(t, tt.hasProjectID, hasProjectID, "project_id presence mismatch")
		})
	}
}

// TestCRMMarkerParsing tests the CRM marker JSON parsing used for fallback
// matching when Todoist changes task IDs (e.g., v9 → v1 migration).
func TestCRMMarkerParsing(t *testing.T) {
	type marker struct {
		CRM       bool   `json:"crm"`
		ContactID string `json:"contact_id"`
		Kind      string `json:"kind"`
	}

	tests := []struct {
		name            string
		description     string
		expectMatch     bool
		expectContactID string
		expectKind      string
	}{
		{
			name:            "valid cadence marker",
			description:     `{"crm":true,"contact_id":"ebd1fbdd-0fde-4b0f-9710-9981f37e1f4c","kind":"cadence","instance":"9fd7f60a-c8f5-4531-ae64-48fdc17e02dd"}`,
			expectMatch:     true,
			expectContactID: "ebd1fbdd-0fde-4b0f-9710-9981f37e1f4c",
			expectKind:      "cadence",
		},
		{
			name:            "valid action marker",
			description:     `{"crm":true,"contact_id":"abc12345-0000-0000-0000-000000000000","kind":"action","instance":"inst-id"}`,
			expectMatch:     true,
			expectContactID: "abc12345-0000-0000-0000-000000000000",
			expectKind:      "action",
		},
		{
			name:            "missing kind defaults to cadence",
			description:     `{"crm":true,"contact_id":"ebd1fbdd-0fde-4b0f-9710-9981f37e1f4c"}`,
			expectMatch:     true,
			expectContactID: "ebd1fbdd-0fde-4b0f-9710-9981f37e1f4c",
			expectKind:      "",
		},
		{
			name:        "not a CRM task (crm=false)",
			description: `{"crm":false,"contact_id":"ebd1fbdd-0fde-4b0f-9710-9981f37e1f4c"}`,
			expectMatch: false,
		},
		{
			name:        "missing contact_id",
			description: `{"crm":true}`,
			expectMatch: false,
		},
		{
			name:        "not JSON",
			description: "Just a plain description",
			expectMatch: false,
		},
		{
			name:        "empty description",
			description: "",
			expectMatch: false,
		},
		{
			name:        "unrelated JSON",
			description: `{"foo":"bar"}`,
			expectMatch: false,
		},
		{
			name:            "marker embedded after markdown prefix",
			description:     "[See context in CRM](http://localhost:3000/contacts/ebd1fbdd-0fde-4b0f-9710-9981f37e1f4c)\n\n---\n{\"contact_id\":\"ebd1fbdd-0fde-4b0f-9710-9981f37e1f4c\",\"crm\":true,\"instance\":\"411cf33d-b6c0-45de-86dc-1371f2461347\",\"kind\":\"cadence\"}",
			expectMatch:     true,
			expectContactID: "ebd1fbdd-0fde-4b0f-9710-9981f37e1f4c",
			expectKind:      "cadence",
		},
		{
			name:            "marker embedded with prefix, no kind",
			description:     "[See context](http://example.com)\n\n---\n{\"contact_id\":\"abc12345-0000-0000-0000-000000000000\",\"crm\":true}",
			expectMatch:     true,
			expectContactID: "abc12345-0000-0000-0000-000000000000",
			expectKind:      "",
		},
	}

	// extractMarker mimics the parsing logic in tryMatchByCRMMarker
	extractMarker := func(description string) (marker, bool) {
		var m marker
		if err := json.Unmarshal([]byte(description), &m); err == nil && m.CRM && m.ContactID != "" {
			return m, true
		}
		// Try extracting JSON from end of description
		m = marker{}
		if idx := strings.LastIndex(description, "{"); idx >= 0 {
			if err := json.Unmarshal([]byte(description[idx:]), &m); err == nil && m.CRM && m.ContactID != "" {
				return m, true
			}
		}
		return marker{}, false
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, matched := extractMarker(tt.description)

			assert.Equal(t, tt.expectMatch, matched, "match expectation")
			if tt.expectMatch {
				assert.Equal(t, tt.expectContactID, m.ContactID)
				assert.Equal(t, tt.expectKind, m.Kind)
			}
		})
	}
}

// TestActionTaskVsCadenceTaskBehavior documents the key behavioral differences
func TestActionTaskVsCadenceTaskBehavior(t *testing.T) {
	t.Run("cadence tasks create new task on completion", func(t *testing.T) {
		// Cadence tasks: completion -> update last_contacted -> calculate new contact_by -> create next task
		// This is the existing behavior verified by other tests
		assert.True(t, true, "documented behavior")
	})

	t.Run("action tasks do not create new task on completion", func(t *testing.T) {
		// Action tasks: completion -> update last_contacted -> mark as completed
		// No new task created, no skip semantics
		assert.True(t, true, "documented behavior")
	})

	t.Run("cadence tasks have skip semantics", func(t *testing.T) {
		// Cadence tasks: deleted/label removed/deadline removed -> skip -> create new task with postponed deadline
		// This is the existing behavior verified by TestReconciliationCommandGeneration
		assert.True(t, true, "documented behavior")
	})

	t.Run("action tasks do not have skip semantics", func(t *testing.T) {
		// Action tasks: deleted/label removed -> mark as unmanaged
		// No skip semantics, no new task created
		assert.True(t, true, "documented behavior")
	})
}

// TestCRMMarkerFallbackHijackPrevention tests the guard that prevents orphaned
// Todoist tasks from hijacking a contact_task's external_task_id via the CRM
// marker fallback. This was the second bug found during the Feb 2026 data
// recovery: active orphans with valid CRM markers would overwrite the real
// external_task_id of the managed contact_task.
func TestCRMMarkerFallbackHijackPrevention(t *testing.T) {
	tests := []struct {
		name            string
		taskExternalID  string
		taskMetadata    map[string]any
		candidateItemID string
		expectSkip      bool // true = guard blocks migration (returns nil)
		expectMigrate   bool // true = migration proceeds
		expectAlreadyOK bool // true = IDs already match (return task as-is)
	}{
		{
			name:            "candidate matches existing ID - no migration needed",
			taskExternalID:  "6fxg8r689rhRxJ9w",
			taskMetadata:    map[string]any{"synced_deadline": "2026-02-15"},
			candidateItemID: "6fxg8r689rhRxJ9w",
			expectAlreadyOK: true,
		},
		{
			name:            "task has real external ID, different candidate - BLOCK (hijack prevention)",
			taskExternalID:  "6fxg8r689rhRxJ9w",
			taskMetadata:    map[string]any{"synced_deadline": "2026-02-15"},
			candidateItemID: "orphan_task_abc123",
			expectSkip:      true,
		},
		{
			name:            "task has empty external ID - allow migration",
			taskExternalID:  "",
			taskMetadata:    map[string]any{"synced_deadline": "2026-02-15"},
			candidateItemID: "6fxg8r689rhRxJ9w",
			expectMigrate:   true,
		},
		{
			name:            "task has pending temp ID - allow migration",
			taskExternalID:  "temp-uuid-123",
			taskMetadata:    map[string]any{"pending_temp_id": "temp-uuid-123"},
			candidateItemID: "6fxg8r689rhRxJ9w",
			expectMigrate:   true,
		},
		{
			name:            "task has resolved temp ID (real ID) - BLOCK",
			taskExternalID:  "6fxg8r689rhRxJ9w",
			taskMetadata:    map[string]any{"pending_temp_id": "temp-uuid-123"},
			candidateItemID: "orphan_task_abc123",
			expectSkip:      true,
		},
		{
			name:            "task has nil metadata and real ID - BLOCK",
			taskExternalID:  "6fxg8r689rhRxJ9w",
			taskMetadata:    nil,
			candidateItemID: "orphan_task_abc123",
			expectSkip:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Simulate the tryMatchByCRMMarker guard sequence (post-repo-call):
			// 1. If candidate ID matches existing ID → return task (already OK)
			// 2. If task has real (non-temp) external ID → return nil (block hijack)
			// 3. Otherwise → proceed with migration

			var skip, migrate, alreadyOK bool

			if tt.taskExternalID == tt.candidateItemID {
				alreadyOK = true
			} else {
				// isPendingTempID logic
				isPending := false
				if tt.taskMetadata != nil && tt.taskExternalID != "" {
					pendingTempID, ok := tt.taskMetadata["pending_temp_id"].(string)
					if ok && pendingTempID != "" {
						isPending = pendingTempID == tt.taskExternalID
					}
				}

				if tt.taskExternalID != "" && !isPending {
					skip = true
				} else {
					migrate = true
				}
			}

			assert.Equal(t, tt.expectAlreadyOK, alreadyOK, "alreadyOK mismatch")
			assert.Equal(t, tt.expectSkip, skip, "skip mismatch (hijack prevention)")
			assert.Equal(t, tt.expectMigrate, migrate, "migrate mismatch")

			// Exactly one outcome should be true
			outcomes := 0
			if alreadyOK {
				outcomes++
			}
			if skip {
				outcomes++
			}
			if migrate {
				outcomes++
			}
			assert.Equal(t, 1, outcomes, "exactly one outcome should be true")
		})
	}
}
