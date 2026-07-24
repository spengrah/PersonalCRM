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

	stop := phase("per-source-settled")
	require.Equal(t, "per-source-settled", timings.Current, "a running phase is named while it runs")
	time.Sleep(time.Millisecond)
	stop(60)

	require.Len(t, timings.Phases, 1)
	require.Equal(t, "per-source-settled", timings.Phases[0].Name)
	require.Equal(t, 60, timings.Phases[0].Payloads)
	require.Positive(t, timings.Phases[0].Duration, "a completed phase reports a real duration")
	require.Empty(t, timings.Current, "a completed phase is no longer marked as running")
}

func TestPhaseTimerAppendsInExecutionOrder(t *testing.T) {
	var timings SeedTimings
	phase := newPhaseTimer(&timings)

	for _, name := range []string{"catalog-contacts", "catalog-notes", "per-source-settled"} {
		phase(name)(0)
	}

	require.Equal(t,
		[]string{"catalog-contacts", "catalog-notes", "per-source-settled"},
		[]string{timings.Phases[0].Name, timings.Phases[1].Name, timings.Phases[2].Name},
		"phases append in execution order — the order a before/after comparison reads")
}

// The failure shape: phases that completed are recorded, the one that was
// running is named in Current, and nothing after it appears. This is precisely
// what runCatalogProfile leaves behind when a block returns an error.
func TestPhaseTimerLeavesFailingPhaseNamed(t *testing.T) {
	var timings SeedTimings
	phase := newPhaseTimer(&timings)

	phase("catalog-contacts")(0)
	phase("catalog-notes")(0)
	phase("per-source-settled") // never stopped: the block errored out

	require.Len(t, timings.Phases, 2, "only completed phases are recorded")
	require.Equal(t, "per-source-settled", timings.Current, "the failing phase stays named")
}

// A phase that seeds no source payloads reports 0, which the summary renders as
// `-`. Pinned so "none by design" cannot drift into an inflated payload count.
func TestPhaseTimerZeroPayloadPhases(t *testing.T) {
	var timings SeedTimings
	phase := newPhaseTimer(&timings)

	phase("graph-signals")(0)

	require.Zero(t, timings.Phases[0].Payloads)
}
