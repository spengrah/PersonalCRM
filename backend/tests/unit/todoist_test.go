package unit

import (
	"testing"

	"personal-crm/backend/internal/contacttask"

	"github.com/stretchr/testify/assert"
)

// TestKindLifecycleConstants verifies the post-046 kind/lifecycle constants
// match their wire values. These strings are persisted in contact_task.kind
// and contact_task.lifecycle and in CRM marker JSON; changing them would
// orphan existing rows and pre-migration markers.
func TestKindLifecycleConstants(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "reach_out", contacttask.KindReachOut)
	assert.Equal(t, "send", contacttask.KindSend)
	assert.Equal(t, "reminder", contacttask.KindReminder)
	assert.Equal(t, "meet", contacttask.KindMeet)
	assert.Equal(t, "action", contacttask.KindAction)
	assert.Equal(t, "manual", contacttask.LifecycleManual)
	assert.Equal(t, "cadence_due", contacttask.LifecycleCadenceDue)
	assert.Equal(t, "followup_loop", contacttask.LifecycleFollowUpLoop)
}
