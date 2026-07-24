package replay

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The settle accumulator is what makes the reseed's cost attributable, and its
// load-bearing claim is narrow: an INLINE HIT is a gate satisfied by the first
// predicate evaluation, before any sleep, and therefore cost ~one query rather
// than polling latency. These tests pin that distinction directly — a gate that
// polls must never be counted as an inline hit, and vice versa — because the
// whole falsifiability argument for "is the reseed waiting on polling or on
// worker throughput" rests on it.
//
// waitGateA touches only the accumulator, so a zero-value Harness is a complete
// fixture: no DB, no River client.

func TestWaitGateAAccountsInlineHit(t *testing.T) {
	h := &Harness{}
	calls := 0

	require.NoError(t, h.waitGateA(context.Background(), func(context.Context) (bool, error) {
		calls++
		return true, nil
	}))

	stats := h.SettleStats()
	require.Equal(t, 1, calls, "a satisfied predicate is evaluated exactly once")
	require.Equal(t, 1, stats.GateACalls)
	require.Equal(t, 1, stats.GateAPolls)
	require.Equal(t, 1, stats.GateAInlineHits, "satisfied before the first sleep")
	require.Zero(t, stats.Calls, "waitGateA alone is not a Settle call")
}

func TestWaitGateAAccountsPollsWithoutInlineHit(t *testing.T) {
	h := &Harness{}
	const wantPolls = 3
	calls := 0

	require.NoError(t, h.waitGateA(context.Background(), func(context.Context) (bool, error) {
		calls++
		return calls >= wantPolls, nil
	}))

	stats := h.SettleStats()
	require.Equal(t, wantPolls, stats.GateAPolls)
	require.Zero(t, stats.GateAInlineHits, "a gate that slept before succeeding is not an inline hit")
	require.GreaterOrEqual(t, stats.GateAWait, time.Duration(wantPolls-1)*settlePollInterval,
		"the recorded wait covers the sleeps between evaluations")
}

// A predicate that ERRORS still polls; the accounting must reflect the cost
// rather than silently dropping it, because an erroring gate is exactly the
// expensive case worth seeing in the summary.
func TestWaitGateAAccountsErroringPredicate(t *testing.T) {
	h := &Harness{}
	calls := 0

	require.NoError(t, h.waitGateA(context.Background(), func(context.Context) (bool, error) {
		calls++
		if calls < 2 {
			return false, errors.New("not ready")
		}
		return true, nil
	}))

	stats := h.SettleStats()
	require.Equal(t, 2, stats.GateAPolls)
	require.Zero(t, stats.GateAInlineHits)
}

// A nil predicate evaluates nothing, so it must not inflate the inline-hit
// DENOMINATOR — that denominator is what the reported hit rate is divided by.
func TestWaitGateANilPredicateIsNotAccounted(t *testing.T) {
	h := &Harness{}

	require.NoError(t, h.waitGateA(context.Background(), nil))

	stats := h.SettleStats()
	require.Zero(t, stats.GateACalls)
	require.Zero(t, stats.GateAPolls)
	require.Zero(t, stats.GateAInlineHits)
}

// Accounting accumulates ACROSS gates: the summary reports run totals, so a
// second gate must add to the first rather than replace it.
func TestSettleStatsAccumulateAcrossGates(t *testing.T) {
	h := &Harness{}
	always := func(context.Context) (bool, error) { return true, nil }

	require.NoError(t, h.waitGateA(context.Background(), always))
	require.NoError(t, h.waitGateA(context.Background(), always))
	h.recordSettleCall()
	h.recordSettleCall()
	h.recordGateB(7*time.Millisecond, 4)
	h.recordCapture(3 * time.Millisecond)

	stats := h.SettleStats()
	require.Equal(t, 2, stats.Calls)
	require.Equal(t, 2, stats.GateACalls)
	require.Equal(t, 2, stats.GateAInlineHits)
	require.Equal(t, 4, stats.GateBPolls)
	require.Equal(t, 7*time.Millisecond, stats.GateBWait)
	require.Equal(t, 3*time.Millisecond, stats.CaptureWait)
}

func TestStopwatchMeasuresRealElapsedTime(t *testing.T) {
	sw := NewStopwatch()
	time.Sleep(2 * time.Millisecond)
	require.GreaterOrEqual(t, sw.Elapsed(), 2*time.Millisecond)
}
