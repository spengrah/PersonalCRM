package consumerjobs

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

func TestTodoistTaskOpArgs_Kind(t *testing.T) {
	require.Equal(t, "todoist_task_op", (TodoistTaskOpArgs{}).Kind())
}

func TestTodoistTaskOpArgs_JSONRoundTrip(t *testing.T) {
	id := uuid.New()
	in := TodoistTaskOpArgs{ContactTaskID: id, Op: TaskOpUpdateDeadline}

	raw, err := json.Marshal(in)
	require.NoError(t, err)
	// Snake-case JSON tags are load-bearing for River args-hash stability.
	require.JSONEq(t, `{"contact_task_id":"`+id.String()+`","op":"update_deadline"}`, string(raw))

	var out TodoistTaskOpArgs
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Equal(t, in, out)
}

func TestTodoistTaskOp_VerbConstants(t *testing.T) {
	// The verb set is the arc's wire contract; assert every verb value
	// verbatim so an accidental rename is caught at compile+test time.
	require.Equal(t, "create", TaskOpCreate)
	require.Equal(t, "close", TaskOpClose)
	require.Equal(t, "delete", TaskOpDelete)
	require.Equal(t, "update_deadline", TaskOpUpdateDeadline)
	require.Equal(t, "update_description", TaskOpUpdateDescription)
}

func TestMessagingAggregateUniqueOpts_ExcludesCompletedJobs(t *testing.T) {
	opts := MessagingAggregateUniqueOpts()

	require.True(t, opts.ByArgs)
	require.ElementsMatch(t, []rivertype.JobState{
		rivertype.JobStateAvailable,
		rivertype.JobStatePending,
		rivertype.JobStateRetryable,
		rivertype.JobStateRunning,
		rivertype.JobStateScheduled,
	}, opts.ByState)
	require.NotContains(t, opts.ByState, rivertype.JobStateCompleted)
}
