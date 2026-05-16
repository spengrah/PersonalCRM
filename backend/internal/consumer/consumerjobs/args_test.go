package consumerjobs

import (
	"testing"

	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

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
