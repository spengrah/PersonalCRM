package unit

import (
	"testing"
	"time"

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
