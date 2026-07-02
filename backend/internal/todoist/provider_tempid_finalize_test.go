package todoist

// State-aware temp-ID finalize tests: a cadence row whose temp mapping (or
// marker-matched sync item) arrives AFTER the row left 'managed' (e.g.
// completed by a contact merge mid-flight) must record the real external id
// WITHOUT being resurrected to 'managed', and a completed row must get the
// durable todoist_followup_close job. Same-package so the tests can call the
// unexported processTempIDMappings / processItem directly.

import (
	"context"
	"fmt"
	"testing"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/consumer/consumerjobs"
	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noopTodoistCloseWorker satisfies the todoist_followup_close kind on the
// atomic env's TestOnly river client so finalize enqueues validate at insert
// time. It never performs remote work.
type noopTodoistCloseWorker struct {
	river.WorkerDefaults[consumerjobs.TodoistFollowUpCloseJobArgs]
}

func (*noopTodoistCloseWorker) Work(context.Context, *river.Job[consumerjobs.TodoistFollowUpCloseJobArgs]) error {
	return nil
}

// seedPendingTempIDTask puts a managed cadence task into the mid-create
// shape: external_task_id == metadata.pending_temp_id == tempID.
func seedPendingTempIDTask(t *testing.T, env *atomicTxTestEnv, namePrefix, tempID string) (*repository.Contact, *repository.ContactTask) {
	t.Helper()
	contact, task := createManagedCadenceTask(t, env, namePrefix)
	_, err := env.contactTaskRepo.UpdateContactTaskExternalID(env.ctx, task.ID, tempID)
	require.NoError(t, err)
	_, err = env.contactTaskRepo.UpdateContactTaskMetadata(env.ctx, task.ID, map[string]any{
		MetadataKeyPendingTempID: tempID,
	})
	require.NoError(t, err)
	return contact, task
}

func closeJobCountFor(t *testing.T, env *atomicTxTestEnv, taskID uuid.UUID) int64 {
	t.Helper()
	n, err := env.contactTaskRepo.CountRiverJobsByContactTask(env.ctx, "todoist_followup_close", taskID)
	require.NoError(t, err)
	return n
}

// TestTodoist_TempIDFinalize_CompletedRowStaysCompletedAndClosesRemote pins
// the merge-race fix: a row completed while its item_add was in flight must
// NOT be flipped back to 'managed' by the temp mapping (the old
// UpdateContactTaskExternalID path set state='managed' unconditionally —
// resurrecting a zombie on a tombstoned contact), and the freshly-created
// remote task must be closed via the durable river job.
func TestTodoist_TempIDFinalize_CompletedRowStaysCompletedAndClosesRemote(t *testing.T) {
	env, cleanup := setupAtomicTxTestEnv(t)
	defer cleanup()

	tempID := "temp-done-" + uuid.New().String()[:8]
	_, task := seedPendingTempIDTask(t, env, "TempDone", tempID)

	// The row leaves 'managed' (e.g. a contact merge closed it) while the
	// item_add / temp mapping is still in flight.
	_, err := env.contactTaskRepo.UpdateContactTaskState(env.ctx, task.ID, repository.ContactTaskStateCompleted)
	require.NoError(t, err)

	realID := "real-done-" + uuid.New().String()[:8]
	rolledBack := env.provider.processTempIDMappings(env.ctx, map[string]string{tempID: realID})
	assert.False(t, rolledBack)

	reloaded, err := env.contactTaskRepo.GetContactTask(env.ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.ContactTaskStateCompleted, reloaded.State, "finalize must NOT resurrect a completed row to managed")
	assert.Equal(t, realID, reloaded.ExternalTaskID, "real id recorded")
	_, hasPending := reloaded.Metadata[MetadataKeyPendingTempID]
	assert.False(t, hasPending, "pending_temp_id cleared")
	assert.EqualValues(t, 1, closeJobCountFor(t, env, task.ID), "durable close job enqueued for the completed row")
}

// TestTodoist_TempIDFinalize_ManagedRowUnchanged is the regression pin: the
// managed path is byte-identical to the old behavior — state stays managed,
// real id recorded, no close job.
func TestTodoist_TempIDFinalize_ManagedRowUnchanged(t *testing.T) {
	env, cleanup := setupAtomicTxTestEnv(t)
	defer cleanup()

	tempID := "temp-mgd-" + uuid.New().String()[:8]
	_, task := seedPendingTempIDTask(t, env, "TempManaged", tempID)

	realID := "real-mgd-" + uuid.New().String()[:8]
	rolledBack := env.provider.processTempIDMappings(env.ctx, map[string]string{tempID: realID})
	assert.False(t, rolledBack)

	reloaded, err := env.contactTaskRepo.GetContactTask(env.ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.ContactTaskStateManaged, reloaded.State)
	assert.Equal(t, realID, reloaded.ExternalTaskID)
	_, hasPending := reloaded.Metadata[MetadataKeyPendingTempID]
	assert.False(t, hasPending)
	assert.EqualValues(t, 0, closeJobCountFor(t, env, task.ID), "no close job on the managed path")
}

// TestTodoist_TempIDFinalize_RemoteCloseDisabledSkipsEnqueue covers the
// mode-'off' gate: the finalize still preserves state + records the real id,
// but no close job is enqueued (WARN path).
func TestTodoist_TempIDFinalize_RemoteCloseDisabledSkipsEnqueue(t *testing.T) {
	env, cleanup := setupAtomicTxTestEnv(t)
	defer cleanup()

	// Provider with remote close DISABLED (follow-up mode off).
	provider := NewCadenceSyncProvider(
		stubOAuthProvider{},
		env.faultyTaskWriter,
		env.contactRepo,
		nil,
		config.TestConfig(),
		env.bus,
		env.cadenceFake,
		env.pool,
		DefaultClientFactory,
		env.riverClient,
		false,
	)

	tempID := "temp-off-" + uuid.New().String()[:8]
	_, task := seedPendingTempIDTask(t, env, "TempOff", tempID)
	_, err := env.contactTaskRepo.UpdateContactTaskState(env.ctx, task.ID, repository.ContactTaskStateCompleted)
	require.NoError(t, err)

	realID := "real-off-" + uuid.New().String()[:8]
	rolledBack := provider.processTempIDMappings(env.ctx, map[string]string{tempID: realID})
	assert.False(t, rolledBack)

	reloaded, err := env.contactTaskRepo.GetContactTask(env.ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.ContactTaskStateCompleted, reloaded.State)
	assert.Equal(t, realID, reloaded.ExternalTaskID)
	assert.EqualValues(t, 0, closeJobCountFor(t, env, task.ID), "no close job when remote close is disabled")
}

// TestTodoist_TryRecoverPendingTempID_CompletedRowViaProcessItem mirrors the
// completed-row finalize through the OTHER site: processItem falls back to
// tryRecoverPendingTempID when the item's real id has no contact_task row,
// and the marker-matched completed row must be finalized without resurrection
// (processItem then skips it as non-managed).
func TestTodoist_TryRecoverPendingTempID_CompletedRowViaProcessItem(t *testing.T) {
	env, cleanup := setupAtomicTxTestEnv(t)
	defer cleanup()

	tempID := "temp-item-" + uuid.New().String()[:8]
	contact, task := seedPendingTempIDTask(t, env, "TempItem", tempID)
	_, err := env.contactTaskRepo.UpdateContactTaskState(env.ctx, task.ID, repository.ContactTaskStateCompleted)
	require.NoError(t, err)

	realID := "real-item-" + uuid.New().String()[:8]
	marker := fmt.Sprintf(`{"crm":true,"contact_id":"%s","kind":"cadence","instance":"x"}`, contact.ID.String())
	result := env.provider.processItem(env.ctx, SyncItem{
		ID:          realID,
		Description: marker,
	}, env.settings, env.accountID)
	require.NoError(t, result.Err)
	assert.False(t, result.RecoveryFailed)

	reloaded, err := env.contactTaskRepo.GetContactTask(env.ctx, task.ID)
	require.NoError(t, err)
	assert.Equal(t, repository.ContactTaskStateCompleted, reloaded.State, "recovery must NOT resurrect a completed row")
	assert.Equal(t, realID, reloaded.ExternalTaskID)
	_, hasPending := reloaded.Metadata[MetadataKeyPendingTempID]
	assert.False(t, hasPending)
	assert.EqualValues(t, 1, closeJobCountFor(t, env, task.ID), "durable close job enqueued via the recovery site")
}
