package synthetic

import (
	"context"
	"fmt"
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/synthetic/replay"
)

// Profile names the synthetic world a seed entrypoint builds. It is pure
// orchestration over the existing factory + replay + declare registries — no new
// replay/factory machinery.
type Profile string

const (
	// ProfileMinimalScoped is the smallest viable world: the existing SeedAll
	// shape (a few contacts, one Gmail-settled + one Telegram-settled each). It
	// is the CI/E2E namespacing baseline and the golden-scenario regression
	// pins its shape, so it MUST stay byte-stable == today's SeedAll behavior.
	//
	// It is an EXPLICIT operator override, never a dev/staging default: a world
	// this small has no content for a UI surface or a QA tour to exercise.
	ProfileMinimalScoped Profile = "minimal-scoped"
)

// ProfileResult is the counts-only summary a seed run reports (no PII). The
// crm-admin entrypoints print it; the QA harness parses it (or checks exit 0).
//
// Every field is one the two surviving worlds actually report. `minimal-scoped`
// reports contacts + the two per-source settled counts; `standard` reports the
// contact total plus its marker-bearing riders. Volume knobs and distribution
// counters belonged to the deleted catalog model and are not reintroduced —
// `standard` is exactly what its declaration + edge registries say it is, so its
// content is asserted as named states rather than as statistics.
type ProfileResult struct {
	Profile         Profile
	Namespace       string
	Seed            uint64
	Contacts        int
	GmailSettled    int
	TelegramSettled int
	// SettledInteractions counts the settled source replays the run drove — the
	// replay-call count, and so the upper bound on the interaction rows produced
	// (same-day messages on one contact would aggregate). Counts-only / no PII.
	SettledInteractions int
	// SeededTasks counts every contact_task row the run seeded. Counts-only / no PII.
	SeededTasks int
	// SeededPendingFollowUps counts the LIVE followup_loop rows seeded — the "awaiting
	// reply" state (has_pending_followup). A seeded world cannot reach this state through
	// the production path (see the seeding site), so it is written directly; without it the
	// state is absent from the world entirely, and the agentic judge reads that absence as
	// a missing feature.
	SeededPendingFollowUps int
	// OutboundOnlyContacts / MutualMessageContacts count the two-sided
	// message-direction riders: the OUTBOUND-only contact ("I messaged them, no reply
	// yet" → last_outreach_at set, last_contacted NULL) and the reply-bridged
	// telegram MUTUAL contact (an outbound + a newer inbound within the bridge
	// window promote in place to one mutual interaction). Kept OUT of the
	// SettledInteractions equation on purpose: the mutual pair is two replay calls
	// collapsing to one promoted row, so folding it in would muddy that invariant.
	// Counts-only / no PII.
	OutboundOnlyContacts  int
	MutualMessageContacts int
	// Timings is the run's wall-clock measurement. It is EXCLUDED from every
	// equality assertion (durations are wall-clock and never equal across runs) —
	// the determinism test zeroes it on both sides before comparing, deliberately
	// keeping the rest of the struct under strict equality.
	Timings SeedTimings
}

// SeedTimings carries wall-clock measurement of a profile run: how long the run
// took in total, how long each phase took and how many source payloads it drove,
// and what the settle gates cost. Durations are REAL time, not accelerated —
// reseed cost is infrastructure timing, and an accelerated measurement under
// TIME_ACCELERATION would report a fictional number (the same reasoning the
// settle budgets use; see replay.Stopwatch).
//
// No PII: durations and counts only.
type SeedTimings struct {
	// Total is the whole-run duration, stamped by RunProfile on BOTH the success
	// and the failure path — a reseed that failed after six minutes is exactly
	// the case the number is for.
	Total time.Duration
	// Phases are the completed phases in execution order. A run that failed
	// carries the phases that finished before the failure.
	Phases []PhaseTiming
	// Current names the phase that was RUNNING when the run returned: empty on a
	// clean run, populated on a failure. Without it the partial summary cannot
	// name the failing phase (the phase-stop closure only records when reached).
	Current string
	// Settle is the harness's cumulative settle accounting, refreshed on every
	// return path so an early settle failure still reports what it cost.
	Settle replay.SettleTimings
}

// PhaseTiming is one seeding block's cost. Name is a stable identifier (it is
// what a before/after comparison across a world change is keyed on), so renaming
// one is a deliberate act, not a refactor.
type PhaseTiming struct {
	Name     string
	Duration time.Duration
	// Payloads is the number of source payloads this phase drove through a replay
	// seam. Phases that seed no source payloads report 0 — "none by design", which
	// the summary renders as `-` so it reads differently from "expected some, got
	// none".
	Payloads int
}

// ProfileParams returns the default SeedParams for a named profile (error on an
// unknown name, including the empty string). The caller may override Namespace /
// Seed / Counts before passing to RunProfile.
func ProfileParams(name Profile) (SeedParams, error) {
	switch name {
	case ProfileMinimalScoped:
		p := DefaultParams()
		// DefaultParams is already minimal-scoped; keep it byte-stable.
		return p, nil
	case ProfileStandard:
		// No Counts: the declared world has no volume knobs — it is exactly what
		// the declaration and edge registries say it is.
		return SeedParams{
			Namespace: "standard",
			Seed:      factory.DefaultSeed,
			Profile:   ProfileStandard,
		}, nil
	default:
		return SeedParams{}, fmt.Errorf("synthetic: unknown profile %q (want one of %q, %q)",
			name, ProfileMinimalScoped, ProfileStandard)
	}
}

// RunProfile builds the selected profile's world against the harness. It is the
// single profile definition both crm-admin --seed and --reset-and-seed call; no
// HTTP handler consumes it (the profile world is CLI-only).
//
// The harness must be constructed for the SAME namespace + seed as params so the
// run's identifiers + cleanup scope line up (use NewHarnessWithDBForNamespace).
// RunProfile only SEEDS; the caller decides the lifecycle — Quiesce on success
// (seed-and-leave), the teardown closure on error (stop client + clean the
// partial world).
// The whole-run duration is stamped on BOTH paths — a run that failed carries a
// partial ProfileResult (elapsed time, the phases that completed, the phase that
// was running) and the callers print it as a degraded summary before surfacing
// the error. Without that, a failed reseed is a bare error with nothing to
// diagnose from.
func RunProfile(ctx context.Context, h *Harness, params SeedParams) (ProfileResult, error) {
	sw := replay.NewStopwatch()
	res := ProfileResult{Profile: params.Profile, Namespace: h.Namespace(), Seed: params.Seed}
	var err error
	switch params.Profile {
	case ProfileMinimalScoped:
		res, err = runMinimalScoped(ctx, h, params, res)
	case ProfileStandard:
		res, err = runStandardProfile(ctx, h, params, res)
	default:
		err = fmt.Errorf("synthetic: RunProfile: unknown profile %q", params.Profile)
	}
	res.Timings.Total = sw.Elapsed()
	return res, err
}

// newPhaseTimer returns the `phase` starter for one profile run, writing into
// the supplied SeedTimings. phase(name) marks the named phase as running (so an
// error return from inside it can be attributed) and returns the stop func;
// stop(payloads) appends the completed PhaseTiming and clears Current. The stop
// func is called EXPLICITLY at the block's end rather than deferred, so the span
// boundary is visible at both ends and a misplacement is obvious rather than silent.
//
// The closures draw no generator PRNG and allocate no source ids, so they are
// position-free: inserting them between existing blocks cannot shift the
// deterministic id/handle sequence the source replays depend on.
func newPhaseTimer(t *SeedTimings) func(name string) func(payloads int) {
	return func(name string) func(payloads int) {
		sw := replay.NewStopwatch()
		t.Current = name
		return func(payloads int) {
			t.Phases = append(t.Phases, PhaseTiming{Name: name, Duration: sw.Elapsed(), Payloads: payloads})
			t.Current = ""
		}
	}
}

// runMinimalScoped is exactly today's SeedAll (the byte-stable golden shape).
// It reports a single phase so both profiles surface timings uniformly.
func runMinimalScoped(ctx context.Context, h *Harness, params SeedParams, res ProfileResult) (out ProfileResult, err error) {
	// Refresh the settle accounting on EVERY return path (including an early
	// settle failure, which is precisely the case the instrumentation exists for).
	defer func() { out.Timings.Settle = h.SettleStats() }()
	phase := newPhaseTimer(&res.Timings)

	stop := phase("seed-all")
	seedRes, err := SeedAll(ctx, h, params)
	if err != nil {
		return res, err
	}
	res.Contacts = len(seedRes.GmailContactIDs) + len(seedRes.TelegramContactIDs)
	res.GmailSettled = len(seedRes.GmailContactIDs)
	res.TelegramSettled = len(seedRes.TelegramContactIDs)
	stop(res.Contacts)
	return res, nil
}

// SeedAllowed is the single chokepoint guard the seed/reset entrypoints route
// through. It returns an error iff CRM_ENV is a production alias (production /
// prod), so a destructive reset or shippable fake-fetcher seed can never run in
// production. Checked PRE-DB by the entrypoints.
func SeedAllowed(cfg *config.Config) error {
	switch cfg.Runtime.CRMEnvironment {
	case "production", "prod":
		return fmt.Errorf("synthetic: seed/reset refused — CRM_ENV is %q (production); the synthetic seed entrypoints are non-production only", cfg.Runtime.CRMEnvironment)
	default:
		return nil
	}
}
