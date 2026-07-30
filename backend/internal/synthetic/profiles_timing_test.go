package synthetic

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The phase timer's contract has two halves, and the second is the one that is
// easy to get wrong: the stop closure records a phase only when REACHED, so an
// error return from inside a block records nothing — which is exactly why
// `Current` exists. Without it the degraded summary cannot name the phase a
// failed reseed died in, and the whole failure-diagnostic path is unimplementable.

func TestPhaseTimerRecordsNameDurationAndPayloads(t *testing.T) {
	var timings SeedTimings
	phase := newPhaseTimer(&timings)

	stop := phase("seed-all")
	require.Equal(t, "seed-all", timings.Current, "a running phase is named while it runs")
	time.Sleep(time.Millisecond)
	stop(60)

	require.Len(t, timings.Phases, 1)
	require.Equal(t, "seed-all", timings.Phases[0].Name)
	require.Equal(t, 60, timings.Phases[0].Payloads)
	require.Positive(t, timings.Phases[0].Duration, "a completed phase reports a real duration")
	require.Empty(t, timings.Current, "a completed phase is no longer marked as running")
}

func TestPhaseTimerAppendsInExecutionOrder(t *testing.T) {
	var timings SeedTimings
	phase := newPhaseTimer(&timings)

	for _, name := range []string{"declaration:DSH-001", "edge:long-history", "tail:pinned-tour-fixtures"} {
		phase(name)(0)
	}

	require.Equal(t,
		[]string{"declaration:DSH-001", "edge:long-history", "tail:pinned-tour-fixtures"},
		[]string{timings.Phases[0].Name, timings.Phases[1].Name, timings.Phases[2].Name},
		"phases append in execution order — the order a before/after comparison reads")
}

// The failure shape: phases that completed are recorded, the one that was
// running is named in Current, and nothing after it appears. This is precisely
// what a profile run leaves behind when a block returns an error.
func TestPhaseTimerLeavesFailingPhaseNamed(t *testing.T) {
	var timings SeedTimings
	phase := newPhaseTimer(&timings)

	phase("declaration:DSH-001")(0)
	phase("edge:long-history")(0)
	phase("tail:pinned-tour-fixtures") // never stopped: the block errored out

	require.Len(t, timings.Phases, 2, "only completed phases are recorded")
	require.Equal(t, "tail:pinned-tour-fixtures", timings.Current, "the failing phase stays named")
}

// A phase that seeds no source payloads reports 0, which the summary renders as
// `-`. Pinned so "none by design" cannot drift into an inflated payload count.
func TestPhaseTimerZeroPayloadPhases(t *testing.T) {
	var timings SeedTimings
	phase := newPhaseTimer(&timings)

	phase("edge:zero-method-contact")(0)

	require.Zero(t, timings.Phases[0].Payloads)
}
