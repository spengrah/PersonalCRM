package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/replay"

	"github.com/stretchr/testify/require"
)

// timingFixture is a ProfileResult carrying a representative timing block:
// completed phases covering all three payload renderings (none, one, many) and
// settle accounting whose numbers are chosen so every derived figure in the
// summary is distinguishable from every other — an arithmetic slip in the
// rendering cannot coincidentally print the right string.
func timingFixture() synthetic.ProfileResult {
	return synthetic.ProfileResult{
		Profile:   synthetic.ProfileStandard,
		Namespace: "standard",
		Seed:      42,
		Contacts:  93,
		// Distinct from each other and from every other count in the fixture, so a
		// rendering that printed the wrong field cannot coincidentally match.
		GmailSettled:           11,
		TelegramSettled:        13,
		SettledInteractions:    17,
		SeededTasks:            19,
		SeededPendingFollowUps: 1,
		OutboundOnlyContacts:   23,
		MutualMessageContacts:  29,
		Timings: synthetic.SeedTimings{
			Total: 100 * time.Second,
			Phases: []synthetic.PhaseTiming{
				{Name: "declaration:DSH-001", Duration: 9500 * time.Millisecond, Payloads: 0},
				{Name: "edge:long-history", Duration: 1500 * time.Millisecond, Payloads: 1},
				{Name: "tail:pinned-tour-fixtures", Duration: 48 * time.Second, Payloads: 60},
			},
			Settle: replay.SettleTimings{
				Calls:           104,
				GateAWait:       40 * time.Second,
				GateBWait:       18 * time.Second,
				CaptureWait:     9 * time.Second,
				GateACalls:      100,
				GateAPolls:      210,
				GateAInlineHits: 88,
				GateBPolls:      520,
			},
		},
	}
}

func TestWriteSeedSummaryRendersTimingBlock(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, writeSeedSummary(&buf, timingFixture(), false))
	out := buf.String()

	require.Contains(t, out, "seed summary (profile=standard namespace=standard prng_seed=42):")
	require.NotContains(t, out, "PARTIAL", "a successful run is never marked partial")
	require.Contains(t, out, "  duration:             100.00s")
	require.Contains(t, out, "  settle_calls:         104")
	require.Contains(t, out, "gate_a=40.00s gate_b=18.00s capture=9.00s")
	// The falsifiability discriminator: the inline-hit rate is over gate-A
	// invocations (100), NOT over Settle calls (104) — a nil-predicate Settle
	// evaluates nothing and must not dilute the rate.
	require.Contains(t, out, "gate_a=inline-hits 88/100")
	require.Contains(t, out, "gate_b=polls 520 (avg 5.0/call)")
	// 100s total − (40+18+9)s of gate wait.
	require.Contains(t, out, "  outside_gates:        33.00s")
	require.Contains(t, out, "NOT a hypothesis test", "the residual is labelled as bookkeeping")
}

// Every surviving count reaches the rendering, each from its OWN field. The
// fixture's values are mutually distinct, so a line printing the wrong field
// cannot coincidentally match.
func TestWriteSeedSummaryRendersEverySurvivingCount(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, writeSeedSummary(&buf, timingFixture(), false))
	out := buf.String()

	require.Contains(t, out, "  contacts:             93")
	require.Contains(t, out, "  gmail_settled:        11")
	require.Contains(t, out, "  telegram_settled:     13")
	require.Contains(t, out, "  settled_interactions: 17")
	require.Contains(t, out, "  tasks:                19")
	require.Contains(t, out, "  pending_followups:    1")
	require.Contains(t, out, "  outbound_only:        23")
	require.Contains(t, out, "  mutual_messages:      29")
}

func TestWriteSeedSummaryRendersEveryPhaseWithPayloadVolume(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, writeSeedSummary(&buf, timingFixture(), false))
	out := buf.String()

	require.Contains(t, out, "  phases (3):")
	// A phase with no source payloads prints `-`, so "none by design" reads
	// differently from "expected payloads, got none".
	require.Regexp(t, `\n    declaration:DSH-001\s+9\.50s\s+-\n`, out)
	require.Regexp(t, `\n    tail:pinned-tour-fixtures\s+48\.00s\s+60 payloads\n`, out)
	// Singular for one — a "1 payloads" row in a summary a human reads once per
	// reseed is the kind of thing that erodes trust in the numbers beside it.
	require.Regexp(t, `\n    edge:long-history\s+1\.50s\s+1 payload\n`, out)
}

func TestWriteSeedSummaryRendersPartialFailure(t *testing.T) {
	res := timingFixture()
	// A run that died mid-block: the phases before it completed, the failing one
	// is named, and the counts are whatever had accumulated.
	res.Timings.Current = "tail:pinned-tour-fixtures"
	res.Timings.Phases = res.Timings.Phases[:2]

	var buf bytes.Buffer
	require.NoError(t, writeSeedSummary(&buf, res, true))
	out := buf.String()

	require.Contains(t, out, `(PARTIAL — run failed during phase "tail:pinned-tour-fixtures")`)
	require.Contains(t, out, "  phases (2):")
	// The failing phase is NAMED in the header but must not appear as a completed
	// row: the phase table is exactly the blocks that finished.
	require.Equal(t, []string{"declaration:DSH-001", "edge:long-history"}, phaseTableNames(out))
}

// phaseTableNames extracts the phase rows (four-space-indented) from a rendered
// summary, so a test can assert the table's contents rather than substring
// presence — the failing phase's NAME appears in the header either way.
func phaseTableNames(summary string) []string {
	var names []string
	for _, line := range strings.Split(summary, "\n") {
		if !strings.HasPrefix(line, "    ") {
			continue
		}
		names = append(names, strings.Fields(line)[0])
	}
	return names
}

// A failure BEFORE any phase started (an unknown profile, a preflight error
// inside RunProfile) still gets a partial marker — just without a phase name.
func TestWriteSeedSummaryPartialWithoutRunningPhase(t *testing.T) {
	res := timingFixture()
	res.Timings.Current = ""
	res.Timings.Phases = nil

	var buf bytes.Buffer
	require.NoError(t, writeSeedSummary(&buf, res, true))
	out := buf.String()

	require.Contains(t, out, "(PARTIAL — run failed):")
	require.Contains(t, out, "  phases (0):")
}

// The minimal-scoped / empty case: a result with no timings at all renders
// without panicking and without a division by zero on the poll averages.
func TestWriteSeedSummaryZeroTimings(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, writeSeedSummary(&buf, synthetic.ProfileResult{
		Profile: synthetic.ProfileMinimalScoped, Namespace: "ns",
	}, false))
	out := buf.String()

	require.Contains(t, out, "  duration:             0.00s")
	// 0/0, not a fudged 0/1: a run with no gate-A invocations had no denominator,
	// and inventing one would misreport the rate the whole block exists to show.
	require.Contains(t, out, "gate_a=inline-hits 0/0")
	require.Contains(t, out, "gate_b=polls 0 (avg 0.0/call)")
	require.Contains(t, out, "  phases (0):")
}

// --- entrypoint failure lifecycle -------------------------------------------

// Both seed entrypoints print the degraded summary BEFORE returning the error.
// Without this the timing block is stamped but never seen, and a reseed that
// died six minutes in is a bare error with nothing to diagnose from.
func TestRunSeedEntrypointsPrintPartialSummaryOnFailure(t *testing.T) {
	partial := synthetic.SeedTimings{
		Total:   90 * time.Second,
		Current: "edge:long-history",
		Phases:  []synthetic.PhaseTiming{{Name: "declaration:DSH-001", Duration: time.Second}},
		Settle:  replay.SettleTimings{Calls: 12},
	}

	for _, tc := range []struct {
		name string
		opts runOptions
	}{
		{"seed", runOptions{doSeed: true, seedYes: true}},
		{"reset-and-seed", runOptions{resetAndSeed: true, seedYes: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deps, stdout, _, _, _, _ := newTestDeps()
			deps.seed = &fakeSeedRunner{
				result: synthetic.ProfileResult{Contacts: 37, Timings: partial},
				err:    errors.New("replay gmail 3 msg 1: boom"),
			}

			err := run(context.Background(), tc.opts, deps)
			require.Error(t, err, "the seed failure is still surfaced")
			require.Contains(t, err.Error(), "boom")

			out := stdout.String()
			require.Contains(t, out, `(PARTIAL — run failed during phase "edge:long-history")`)
			require.Contains(t, out, "  contacts:             37", "the counts that accumulated are reported")
			require.Contains(t, out, "  duration:             90.00s")
			require.Contains(t, out, "  settle_calls:         12")
		})
	}
}

// A POST-SEED failure — Quiesce, which runs after the profile completed and
// after teardown was skipped — must NOT print as PARTIAL. The world is fully
// seeded and intact; an operator who reads "run failed" against a good staging
// world re-runs --reset-and-seed and wipes it, which is the inverse of the
// mistake the marker exists to prevent and the more expensive one.
func TestRunSeedPrintsCompleteSummaryWhenOnlyPostSeedStepFailed(t *testing.T) {
	deps, stdout, _, _, _, _ := newTestDeps()
	deps.seed = &fakeSeedRunner{
		result: synthetic.ProfileResult{Contacts: 93, Timings: synthetic.SeedTimings{Total: 71 * time.Second}},
		// The shape seedAdapter.runProfile produces on a Quiesce failure.
		err: fmt.Errorf("%w: quiesce after seed: %w", errSeedWorldIntact, errors.New("stop river client")),
	}

	err := run(context.Background(), runOptions{doSeed: true, seedYes: true}, deps)

	require.Error(t, err, "the command still fails")
	require.Contains(t, err.Error(), "quiesce after seed")
	out := stdout.String()
	require.NotContains(t, out, "PARTIAL", "a completed seed is never labelled a failed run")
	require.NotContains(t, out, "torn down", "an intact world is not described as torn down")
	require.Contains(t, out, "  contacts:             93")
}

// The counterpart: a genuine PROFILE failure still gets the marker, and says the
// rows it counts no longer exist.
func TestWriteSeedSummaryOnErrorMarksProfileFailurePartial(t *testing.T) {
	res := timingFixture()
	res.Timings.Current = "edge:long-history"

	var partialBuf, intactBuf bytes.Buffer
	writeSeedSummaryOnError(&partialBuf, res, errors.New("replay gmail 3 msg 1: boom"))
	writeSeedSummaryOnError(&intactBuf, res, fmt.Errorf("%w: quiesce", errSeedWorldIntact))

	require.Contains(t, partialBuf.String(), "PARTIAL")
	require.Contains(t, partialBuf.String(), "has been torn down")
	require.NotContains(t, intactBuf.String(), "PARTIAL")
}

// A failure BEFORE the profile ran (the additive drain preflight, a harness
// build) leaves a zero result. Printing a summary of nothing would be noise, so
// the degraded summary is suppressed and only the error surfaces.
func TestRunSeedPrintsNoSummaryWhenProfileNeverRan(t *testing.T) {
	deps, stdout, _, _, _, _ := newTestDeps()
	deps.seed = &fakeSeedRunner{err: errors.New("refusing additive --seed: 4 queued river_job row(s)")}

	err := run(context.Background(), runOptions{doSeed: true, seedYes: true}, deps)
	require.Error(t, err)
	require.False(t, strings.Contains(stdout.String(), "seed summary"),
		"a run that never reached the profile prints no summary, got %q", stdout.String())
}
