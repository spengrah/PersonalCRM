package todoist

import (
	"testing"

	"personal-crm/backend/internal/contacttask"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreateTaskCommand_EmitsCadenceMarker locks the E1 call site: the cadence
// item_add command's description must decode (via DecodeMarker) to
// (reach_out, cadence_due) carrying the contact's id and the configured
// integration instance. Guards that createTaskCommand passes the correct
// fields to EncodeMarker, not just that the codec round-trips.
func TestCreateTaskCommand_EmitsCadenceMarker(t *testing.T) {
	env, cleanup := setupDismissalTest(t)
	defer cleanup()

	contact, _ := createDismissalContact(t, env, "E1Marker")

	deadline := "2099-01-01"
	cmd := env.provider.createTaskCommand(contact, env.settings, &deadline)

	require.Equal(t, "item_add", cmd.Type)
	desc, ok := cmd.Args["description"].(string)
	require.True(t, ok, "item_add must carry a description with the CRM marker")

	marker, ok := contacttask.DecodeMarker(desc)
	require.True(t, ok, "description must decode as a CRM marker")
	assert.Equal(t, contact.ID.String(), marker.ContactID)
	assert.Equal(t, contacttask.KindReachOut, marker.Kind)
	assert.Equal(t, contacttask.LifecycleCadenceDue, marker.Lifecycle)
	assert.Equal(t, env.settings.IntegrationInstanceID, marker.Instance)
}
