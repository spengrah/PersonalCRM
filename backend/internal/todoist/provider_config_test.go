package todoist

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCadenceSyncProvider_Config_RequiresAccount is the real-provider config
// invariant: Todoist is account-scoped (its OAuth token is keyed by account,
// and Sync nil-checks state.AccountID), so its Config must declare
// RequiresAccount so TriggerSync rejects a nil/empty account instead of
// bootstrapping a permanently-erroring sync_state row.
//
// Config() returns a pure struct literal and reads no instance fields, so a
// zero-value &CadenceSyncProvider{} is safe here — the NewCadenceSyncProvider
// constructor dereferences cfg.CORS.FrontendURL, which this sidesteps.
func TestCadenceSyncProvider_Config_RequiresAccount(t *testing.T) {
	cfg := (&CadenceSyncProvider{}).Config()

	assert.Equal(t, SourceName, cfg.Name)
	assert.True(t, cfg.RequiresAccount, "todoist is account-scoped; TriggerSync must reject nil/empty account")
}
