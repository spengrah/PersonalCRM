package declare

import (
	"fmt"
	"os"
	"sort"
	"time"

	"personal-crm/backend/internal/synthetic/replay"
)

// This file states PRODUCT FACTS locally — deliberate, tripwire-guarded
// duplicates of the deployed cadence configuration (internal/cadence).
//
// The independence rule (arc #759 §1.6): a declaration translates domain terms
// ("overdue by 3 days on a weekly cadence") into durations WITHOUT calling the
// app's own cadence math. If GetCadenceDuration regressed (monthly → 300d), a
// fixture derived from it would stay overdue and hide the regression; a locally
// stated 30d fixture goes NOT-overdue and the citing E2E spec fails loudly. Non-test code in this package therefore must never import
// internal/cadence — imports_test.go enforces that recursively, and
// facts_test.go asserts these tables equal the cadence package's per env so an
// intentional product change surfaces as a conscious two-sided edit.
//
// The CRM_ENV=testing table is NOT ratio-proportional to production (weekly is
// 2min but monthly is 10min, not 30/7 × 2min), so the full per-env tables are
// restated rather than one ratio table plus a scale factor.

const day = 24 * time.Hour

// productionPeriods is the real-world cadence table (production + staging).
var productionPeriods = map[string]time.Duration{
	"weekly":    7 * day,
	"biweekly":  14 * day,
	"monthly":   30 * day,
	"quarterly": 90 * day,
	"biannual":  180 * day,
	"annual":    365 * day,
}

// testingPeriods is the compressed CRM_ENV=test/testing table (weeks in minutes).
var testingPeriods = map[string]time.Duration{
	"weekly":    2 * time.Minute,
	"biweekly":  4 * time.Minute,
	"monthly":   10 * time.Minute,
	"quarterly": 30 * time.Minute,
	"biannual":  1 * time.Hour,
	"annual":    2 * time.Hour,
}

// acceleratedPeriods is the CRM_ENV=accelerated table (months in hours).
var acceleratedPeriods = map[string]time.Duration{
	"weekly":    10 * time.Minute,
	"biweekly":  20 * time.Minute,
	"monthly":   1 * time.Hour,
	"quarterly": 3 * time.Hour,
	"biannual":  6 * time.Hour,
	"annual":    12 * time.Hour,
}

// activePeriods selects the table for the ambient environment, using the same
// CRM_ENV discriminator the deployed cadence configuration uses. Anything
// unrecognized falls back to production, matching that configuration's own
// default-for-safety branch.
func activePeriods() map[string]time.Duration {
	switch os.Getenv("CRM_ENV") {
	case "test", "testing":
		return testingPeriods
	case "accelerated":
		return acceleratedPeriods
	default:
		return productionPeriods
	}
}

// period returns the cadence period for the ambient environment. An unknown
// cadence returns 0; callers validate the vocabulary at Register time, so a
// zero here can only mean a programming error downstream of that validation.
func period(cadence string) time.Duration {
	return activePeriods()[cadence]
}

// dayLength is what one "day" is worth in the active table: weekly / 7. It
// matches the scaled-day arithmetic the overdue-days display uses (2min/7 under
// CRM_ENV=testing, 10min/7 under accelerated, 24h in production), so
// `OverdueBy(Days(n))` is n scaled days of overdue-ness in whichever environment
// it runs.
//
// It does NOT make the RENDERED day count equal n. The declared semantics are a
// floor — overdue by AT LEAST the stated amount — and a replayed history source
// carries a fixed pre-anchor safety lag on top (sourceHistoryLag, below), which
// is additive with the requested age. Where a day is long relative to that lag
// the rendered number is close to n; under the compressed CRM_ENV=testing table,
// where a day is ~17 seconds, the lag dominates and the card renders a much
// larger count. A fixture that must pin an exact rendered day count needs a
// non-email history source, which the vocabulary does not have yet.
func dayLength() time.Duration {
	return activePeriods()["weekly"] / 7
}

// sourceHistoryLag is the fixed pre-anchor safety lag every replayed source
// message carries: the providers scan already-CLOSED windows, so a payload
// timestamped too close to the anchor would never be listed. It is additive
// with any age a declaration requests, so the lowering has to account for it
// when deriving the creation age (a contact must exist before the history it
// carries). Stated locally for the same reason the cadence tables are —
// deriving it from the factory would make a fixture track a regression instead
// of failing on one — and pinned by a tripwire against the real factory output.
const sourceHistoryLag = 2 * time.Hour

// History spread. A declared History(n) lays its n inbound messages linearly
// across ages historyOldestDays … historyNewestDays before the anchor, with no
// PRNG: the spread is a pure function of (i, n) so the same declaration always
// produces the same timeline.
const (
	historyOldestDays = 120
	historyNewestDays = 1
)

// historyMessageAge is how far before the anchor message i of n sits — the
// oldest first, which is the chronological order the batch adapter requires.
func historyMessageAge(i, n int) time.Duration {
	days := historyOldestDays
	if n > 1 {
		days = historyOldestDays - ((historyOldestDays-historyNewestDays)*i)/(n-1)
	}
	return time.Duration(days) * dayLength()
}

// historyCreationMargin is how much EARLIER than its oldest message a
// History-bearing contact is created. It has to be strictly positive: a
// creation instant exactly equal to the oldest message would mean the first
// email arrived at the very instant the contact was added, which is not the
// "a contact exists before the connection it carries" property the margin is
// for — and which the edge's read-path assertion (created_at STRICTLY before
// the oldest occurred_at) would fail. One day in the active table, so it is a
// real margin in production and a proportional one under the compressed tables.
//
// Unlike OverdueBy's margin it does NOT need a further period: History is not
// dragging a forward-only due date backwards (its newest message is recent, so
// the derived due date moves forward, which the app does anyway).
func historyCreationMargin() time.Duration { return dayLength() }

// historySpanWithinBatchReach rejects a history whose oldest-to-newest span
// exceeds what ONE Gmail batch sync can reach. The bound comes from the batch
// adapter itself rather than a copy of its number, and it is checked against
// the ACTIVE table so a future change to `weekly` in either environment's
// cadence configuration surfaces here rather than as a Gate A timeout.
func historySpanWithinBatchReach(n int) error {
	if n < 2 {
		return nil
	}
	span := historyMessageAge(0, n) - historyMessageAge(n-1, n)
	if reach := replay.GmailBatchMaxSpan(); span > reach {
		return fmt.Errorf("History(%d) spans %s from oldest to newest message but one batch sync reaches only %s", n, span, reach)
	}
	return nil
}

// Cadences is the cadence vocabulary a declaration may use, sorted. It is the
// validation set for Cadence(...).
func Cadences() []string {
	out := make([]string, 0, len(productionPeriods))
	for name := range productionPeriods {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// knownCadence reports whether name is in the vocabulary.
func knownCadence(name string) bool {
	_, ok := productionPeriods[name]
	return ok
}
