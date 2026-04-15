package scheduler

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSyncProviderAccountArgs_JSONContract guards the load-bearing JSON
// keys on SyncProviderAccountArgs. The repository's CountInFlightSyncJobs
// SQL query reads river_job.args->>'source' and args->>'account_id' —
// accidental renames of these JSON tags would silently break the dedup
// check. This test fails the build if either key changes.
func TestSyncProviderAccountArgs_JSONContract(t *testing.T) {
	t.Run("source key is lowercase source", func(t *testing.T) {
		args := SyncProviderAccountArgs{Source: "gmail"}
		b, err := json.Marshal(args)
		require.NoError(t, err)

		var m map[string]any
		require.NoError(t, json.Unmarshal(b, &m))

		got, ok := m["source"]
		assert.True(t, ok, "expected 'source' key in marshaled args (got %v)", m)
		assert.Equal(t, "gmail", got)
	})

	t.Run("account_id key is emitted when set", func(t *testing.T) {
		id := "acct-123"
		args := SyncProviderAccountArgs{Source: "gmail", AccountID: &id}
		b, err := json.Marshal(args)
		require.NoError(t, err)

		var m map[string]any
		require.NoError(t, json.Unmarshal(b, &m))

		got, ok := m["account_id"]
		assert.True(t, ok, "expected 'account_id' key in marshaled args (got %v)", m)
		assert.Equal(t, "acct-123", got)
	})

	t.Run("account_id is omitted when nil via omitempty", func(t *testing.T) {
		args := SyncProviderAccountArgs{Source: "gmail"}
		b, err := json.Marshal(args)
		require.NoError(t, err)

		var m map[string]any
		require.NoError(t, json.Unmarshal(b, &m))

		_, ok := m["account_id"]
		assert.False(t, ok, "expected 'account_id' key to be omitted when nil (got %v)", m)
	})

	t.Run("kind strings match query literal", func(t *testing.T) {
		// The CountInFlightSyncJobs query filters kind = 'sync_provider_account'.
		// Also, main.go registers a periodic job with SchedulerTickArgs.
		assert.Equal(t, "sync_provider_account", SyncProviderAccountArgs{}.Kind())
		assert.Equal(t, "scheduler_tick", SchedulerTickArgs{}.Kind())
	})
}
