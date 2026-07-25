package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/accelerated"
	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/contacttask"
	"personal-crm/backend/internal/repository"
	"personal-crm/backend/internal/synthetic"
	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/synthetic/replay"
	"personal-crm/backend/tests/testsupport"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// trailingSeqRE extracts the generator's monotonic sourceIDSeq embedded as the
// trailing "-<digits>" of a synthetic value_text / entity normalized_name (e.g.
// "synth-<ns>-organization-Alice-42" → 42). The seq is namespace-INDEPENDENT (a
// pure counter, unlike the PRNG-drawn name component), so it is a stable
// cross-run/cross-namespace ordering witness. Returns ok=false for values with no
// trailing seq (e.g. the how_met "synth-<ns>-met-<stem>" or a place
// "synth-<ns>-place-<stem>"), which the caller skips.
var trailingSeqRE = regexp.MustCompile(`-(\d+)$`)

func trailingSeq(s string) (int, bool) {
	m := trailingSeqRE.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// fixedAccelBaseUnix is a deterministic TIME_BASE (a fixed Unix second) for the
// high-acceleration settle-budget test, so the fixture makes no wall-clock call.
const fixedAccelBaseUnix int64 = 1735689600 // 2025-01-01T00:00:00Z

// taskAgeBucket coarsely buckets a contact_task's created_at relative to a fixed
// reference (the generator anchor), so the visible-task assertions + fingerprint
// key on link-age CLASS, not an exact timestamp — the cadence/follow-up rows are
// created at ~NOW (a hair after the anchor, differing per run) yet must land in a
// stable bucket. The manual spread's ages (2d / 21d / 90d) split across all three.
func taskAgeBucket(ref, createdAt time.Time) string {
	days := ref.Sub(createdAt).Hours() / 24
	switch {
	case days < 7:
		return "recent"
	case days < 60:
		return "weeks"
	default:
		return "months"
	}
}

// taskFingerprint is the run-to-run determinism witness for the visible-task
// spread: the sorted (full_name, kind, lifecycle, state, age-bucket) tuples over
// every seeded contact_task. Keyed by STABLE identity (full_name) + attributes,
// excluding external_task_id + raw UUIDs, so it is byte-stable iff the spread maps
// shape→named-contact by a stable creation index and DIFFERS if a random UUID keyed
// the bucket assignment. ref is the (pinned) generator anchor.
func taskFingerprint(rows []repository.TaskRow, ref time.Time) []string {
	fp := make([]string, 0, len(rows))
	for _, r := range rows {
		fp = append(fp, strings.Join([]string{r.FullName, r.Kind, r.Lifecycle, r.State, taskAgeBucket(ref, r.CreatedAt)}, "|"))
	}
	sort.Strings(fp)
	return fp
}

// assertVisibleTaskSpread verifies the product-visible task spread: the exact task
// total, the manual-scoped ProfileResult counters, the 0/1/>1 visible-task cohorts
// (subject-scoped to the catalog id set, with the follow-up contact accounted
// separately), and the kind / lifecycle / link-age / content coverage over the
// seeded task rows. Shared by the dev + prod-shaped coverage tests (the fixed
// creation-index allocation is identical at any catalog size ≥ 4).
func assertVisibleTaskSpread(t *testing.T, ctx context.Context, support *repository.SyntheticSupportRepository, h *synthetic.Harness, res synthetic.ProfileResult, prefix string) {
	t.Helper()

	// Exact task total: every seeded contact_task row on the namespace == SeededTasks
	// (cadence + manual + follow-up), so the accounting is closed.
	totalTasks, err := support.CountContactTasksByNamePrefix(ctx, prefix)
	require.NoError(t, err)
	require.Equal(t, int64(res.SeededTasks), totalTasks, "scoped contact_task count == res.SeededTasks (cadence + manual + follow-up)")

	// Manual-scoped counters are EXACT for the fixed creation-index allocation
	// (indices 1,2 → one manual each; index 3 → two), independent of catalog size.
	require.Equal(t, 4, res.SeededManualTasks, "visible spread seeds 4 manual tasks")
	require.Equal(t, 3, res.SeededContactsWithManualTasks, "3 catalog contacts get manual tasks")
	require.Equal(t, 1, res.SeededContactsWithMultipleManualTasks, "1 catalog contact gets >1 manual task")
	require.Len(t, h.ManualCohortIDs(), 3, "3 reserved manual-cohort contact ids recorded")

	// Visible-task cohorts over the CATALOG universe. A LEFT JOIN so a 0-visible
	// contact appears with count 0 — it still holds a background cadence_due row the
	// contact page never lists, so 0-visible is asserted through the UI filters, NOT
	// count(*)==0. The follow-up contact is NON-catalog and asserted separately below.
	catalogIDs := h.CatalogContactIDs()
	require.NotEmpty(t, catalogIDs, "catalog ids recorded for the spread")
	counts, err := support.ListVisibleTaskCountsByContactIds(ctx, catalogIDs)
	require.NoError(t, err)
	require.Len(t, counts, len(catalogIDs), "every catalog contact appears (LEFT JOIN produces the 0-visible rows)")
	var zeroVisible, oneVisible, multiVisible int
	for _, c := range counts {
		switch c.VisibleCount {
		case 0:
			zeroVisible++
		case 1:
			oneVisible++
		default:
			multiVisible++
		}
	}
	require.Equal(t, 2, oneVisible, "exactly 2 catalog contacts have 1 visible task (indices 1,2)")
	require.Equal(t, 1, multiVisible, "exactly 1 catalog contact has >1 visible task (index 3)")
	require.Equal(t, len(catalogIDs)-3, zeroVisible, "the remaining catalog contacts are the 0-visible majority (background cadence_due only)")

	// Enforce the EXACT creation-index allocation, not just the aggregate histogram: a
	// deterministic reallocation to other indices (e.g. 0,1,4) would pass the histogram
	// + the name-keyed fingerprint but must fail HERE. Every catalog slot is
	// cadence-bearing (catalogOptionsFor), so CatalogContactIDs is the same
	// creation-ordered slice SeedVisibleTaskSpread indexed into.
	countByID := make(map[uuid.UUID]int64, len(counts))
	for _, c := range counts {
		countByID[c.ContactID] = c.VisibleCount
	}
	for i, id := range catalogIDs {
		want := int64(0)
		switch i {
		case 1, 2:
			want = 1
		case 3:
			want = 2
		}
		require.Equal(t, want, countByID[id], "catalog creation index %d visible-task count", i)
	}
	require.Equal(t, catalogIDs[1:4], h.ManualCohortIDs(), "manual cohorts are exactly creation indices 1, 2, 3")

	// Row-level coverage over every seeded task (joined to its stable-identity
	// full_name): the product-visible picture, kinds, lifecycles, link-age spread,
	// and non-empty manual content.
	taskRows, err := support.ListTaskRowsByNamePrefix(ctx, prefix)
	require.NoError(t, err)
	require.Len(t, taskRows, res.SeededTasks, "task row list covers every seeded task")

	visibleByContact := map[string]int{}
	visibleRows := 0
	kinds := map[string]int{}
	lifecycles := map[string]int{}
	ageBuckets := map[string]struct{}{}
	anchor := h.Generator().Anchor()
	for _, r := range taskRows {
		lifecycles[r.Lifecycle]++
		if r.State == string(repository.ContactTaskStateManaged) &&
			(r.Lifecycle == contacttask.LifecycleManual || r.Lifecycle == contacttask.LifecycleFollowUpLoop) {
			visibleRows++
			visibleByContact[r.FullName]++
		}
		if r.Lifecycle == contacttask.LifecycleManual {
			kinds[r.Kind]++
			require.NotEmpty(t, r.Content, "manual task carries non-empty content")
			ageBuckets[taskAgeBucket(anchor, r.CreatedAt)] = struct{}{}
		}
	}

	// Product-visible picture (the UI filters: managed + manual/followup_loop) ==
	// the manual cohorts + the 1 follow-up contact — accounted, not double-counted.
	require.Equal(t, res.SeededManualTasks+res.SeededPendingFollowUps, visibleRows,
		"product-visible task rows == manual tasks + the follow-up")
	require.Len(t, visibleByContact, res.SeededContactsWithManualTasks+res.SeededPendingFollowUps,
		"product-visible contacts == manual-cohort catalog contacts + the 1 follow-up contact")

	// Varied kind: all three user-pickable kinds present among manual tasks.
	require.GreaterOrEqual(t, kinds[contacttask.KindReachOut], 1, "≥1 reach_out manual task")
	require.GreaterOrEqual(t, kinds[contacttask.KindSend], 1, "≥1 send manual task")
	require.GreaterOrEqual(t, kinds[contacttask.KindReminder], 1, "≥1 reminder manual task")

	// Varied lifecycle across the seeded tasks.
	require.GreaterOrEqual(t, lifecycles[contacttask.LifecycleCadenceDue], 1, "≥1 cadence_due task")
	require.GreaterOrEqual(t, lifecycles[contacttask.LifecycleManual], 1, "≥1 manual task")
	require.GreaterOrEqual(t, lifecycles[contacttask.LifecycleFollowUpLoop], 1, "≥1 followup_loop task")

	// Varied link age: ≥2 distinct created_at buckets among manual tasks.
	require.GreaterOrEqual(t, len(ageBuckets), 2, "≥2 distinct link-age buckets among manual tasks")
}

// birthdayFingerprint is the run-to-run determinism witness for the clock-anchored
// birthday fixtures: the sorted (full_name, daysUntil-bucket, age-decade) tuples
// over the reserved fixture subjects. Keyed by STABLE identity (full_name) +
// classification, excluding raw UUIDs, so it is byte-stable iff the fixtures map
// shape→named-contact by a stable creation index and DIFFERS if a random UUID keyed
// the allocation or the year-math drifts. anchor is the (pinned) generator anchor.
func birthdayFingerprint(rows []repository.BirthdayFixtureRow, anchor time.Time) []string {
	fp := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.Birthday == nil {
			continue
		}
		// Use the UTC year to match BirthdayFixtureDate's UTC-pinned birth year, so the
		// age decade agrees with the UTC-pinned browser's rendered age around the year
		// boundary (anchor.Year() could differ from anchor.UTC().Year() there).
		ageDecade := (anchor.UTC().Year() - r.Birthday.Year()) / 10
		fp = append(fp, strings.Join([]string{
			r.FullName,
			synthetic.BirthdayBucket(*r.Birthday, anchor),
			strconv.Itoa(ageDecade),
		}, "|"))
	}
	sort.Strings(fp)
	return fp
}

// assertBirthdayFixtures verifies the clock-anchored birthday fixtures (CON-052):
// the SeededDateFacts total via the shared scale helper, the strict guarantee (≥1 of
// the {today,+1} redundancy pair lands imminent, ≥1 fixture recedes past the ≤7-day
// highlight window), per-fixture seed integrity (each reserved[i] stored the date the
// plan computed for its offset), the date-INDEPENDENT classification (forward
// fixtures are exactly their offset days out via daysUntil==offset; the celebrated
// fixture is past this year), and a populated birthday cache on every reserved
// subject. Subject-scoped to the reserved ids only — the catalog seeds its own
// birthdays in the same namespace. anchor is the generator anchor; n is the catalog size.
func assertBirthdayFixtures(t *testing.T, ctx context.Context, support *repository.SyntheticSupportRepository, h *synthetic.Harness, res synthetic.ProfileResult, anchor time.Time, n int) {
	t.Helper()

	reserved := h.BirthdayFixtureIDs()
	plan := synthetic.BirthdayFixturePlan(anchor)
	wantSeeded := min(synthetic.BirthdaylessCatalogCount(n), len(plan))
	require.Equal(t, wantSeeded, res.SeededDateFacts, "SeededDateFacts == min(birthdayless catalog slots, plan size)")
	require.Len(t, reserved, wantSeeded, "reserved fixture id count == SeededDateFacts")
	require.GreaterOrEqual(t, len(reserved), 3, "at least the strict {today,+1,distant} triple is seeded")

	rows, err := support.ListContactBirthdayFixturesByIds(ctx, reserved)
	require.NoError(t, err)
	byID := make(map[uuid.UUID]repository.BirthdayFixtureRow, len(rows))
	for _, r := range rows {
		byID[r.ContactID] = r
	}
	require.Len(t, byID, len(reserved), "every reserved fixture contact resolves")

	// Strict imminent guarantee — over the {today,+1} redundancy PAIR ONLY: ≥1 lands
	// in the ≤7-day highlight window, so a regression of BOTH pair fixtures fails even
	// if the independent +5 this-week fixture still classifies imminent.
	imminentInPair := 0
	for _, id := range reserved[:min(2, len(reserved))] {
		row := byID[id]
		require.NotNil(t, row.Birthday, "reserved pair fixture has a populated birthday cache")
		if synthetic.BirthdayDaysUntil(*row.Birthday, anchor) <= 7 {
			imminentInPair++
		}
	}
	require.GreaterOrEqual(t, imminentInPair, 1, "≥1 of the {today,+1} pair classifies imminent (≤7 days)")

	// Strict recedes guarantee — ≥1 reserved fixture is OUTSIDE the highlight window
	// (daysUntil > 7). Deliberately NOT `&& !past`: a +90/+150 fixture seeded when the
	// anchor is in ~Oct-Dec wraps into next-year Jan-Mar, so its occurrence THIS
	// calendar year is already past (the page files it under "Already Celebrated This
	// Year") though its next occurrence is genuinely ~90 days out — it has still
	// receded from the imminent window, which is the CON-052 quality.
	receded := 0
	for _, id := range reserved {
		if row := byID[id]; row.Birthday != nil && synthetic.BirthdayDaysUntil(*row.Birthday, anchor) > 7 {
			receded++
		}
	}
	require.GreaterOrEqual(t, receded, 1, "≥1 reserved fixture recedes past the ≤7-day highlight window")

	// Per-fixture seed integrity + classification: each reserved[i] stored the date the
	// plan computed for its offset (reserved[i] ⇄ plan[i]), and its next-occurrence
	// distance is exactly that offset. daysUntil==offset is DATE-INDEPENDENT (a +5
	// fixture is 5 days out whether or not its wrapped January occurrence lands the
	// page's "celebrated" section near year-end), so it — not a fragile section bucket
	// — is what we pin. The celebrated fixture (offset<0) is always past-this-year when
	// present (its date-gating keeps it same-year). The section→offset mapping is
	// verified against the real page by the frontend parity test.
	for i, id := range reserved {
		row := byID[id]
		require.NotNil(t, row.Birthday, "reserved fixture %d birthday cache populated", i)
		want := synthetic.BirthdayFixtureDate(anchor, plan[i].OffsetDays)
		require.Equal(t, want.Month(), row.Birthday.Month(), "fixture %d (%s) stored month", i, plan[i].Role)
		require.Equal(t, want.Day(), row.Birthday.Day(), "fixture %d (%s) stored day", i, plan[i].Role)
		if plan[i].OffsetDays >= 0 {
			require.Equal(t, plan[i].OffsetDays, synthetic.BirthdayDaysUntil(*row.Birthday, anchor),
				"fixture %d (%s) is exactly %d days out", i, plan[i].Role, plan[i].OffsetDays)
		} else {
			require.True(t, synthetic.BirthdayIsPastThisYear(*row.Birthday, anchor),
				"fixture %d (%s) is past this year (celebrated)", i, plan[i].Role)
		}
	}
}

// assertPinnedTourFixtures runs the pinned tour-fixture gate. For every marker in
// synthetic.PinnedFixtureMarkers it requires that:
//
//   - exactly ONE contact in the namespace carries the marker (not "at least one":
//     a second match would make the tour's choice a silent coin flip, so it has to
//     be a loud failure here);
//   - that row is the one the harness recorded, so the id-scoped assertions below
//     are about the same contact the tour will resolve;
//   - the API SEARCH the tours actually use returns exactly that row. A marker that
//     exists in the DB but does not survive full-text tokenization is unresolvable,
//     and only running the real search path proves that it does.
//
// It then asserts each fixture carries the state its consuming tour depends on,
// and that the ordering the tours' deliberately POSITIONAL selections read is
// non-degenerate. The tours read a world-wide list where this reads a
// namespace-scoped one; on staging the seeded namespace IS the world, which is the
// case these assertions are about.
func assertPinnedTourFixtures(
	t *testing.T,
	ctx context.Context,
	support *repository.SyntheticSupportRepository,
	contactRepo *repository.ContactRepository,
	h *synthetic.Harness,
	prefix string,
	anchor time.Time,
) {
	t.Helper()
	now := accelerated.GetCurrentTime()

	rows, err := support.ListPinnedFixtureContactsByNamePrefix(ctx, prefix)
	require.NoError(t, err)
	// DOCUMENTATION, NOT A GATE — A6: the contacts tour hard-throws below five
	// contacts, but the pinned block alone seeds eight into this namespace, so this
	// records the tour's requirement rather than gating anything the seed could
	// plausibly lose.
	require.GreaterOrEqual(t, len(rows), 5, "the contacts tour needs ≥5 contacts in the world")

	recorded := h.PinnedFixtureIDs()
	require.Len(t, recorded, len(synthetic.PinnedFixtureMarkers),
		"every declared fixture marker must be recorded on the harness — an unrecorded fixture is one nothing can scope an assertion to")

	byMarker := make(map[string]repository.PinnedFixtureContact, len(synthetic.PinnedFixtureMarkers))
	subjectOf := make(map[uuid.UUID]string, len(synthetic.PinnedFixtureMarkers))
	for _, marker := range synthetic.PinnedFixtureMarkers {
		var matches []repository.PinnedFixtureContact
		for _, row := range rows {
			if strings.Contains(row.FullName, marker) {
				matches = append(matches, row)
			}
		}
		require.Len(t, matches, 1, "fixture %q must resolve to exactly one contact in the namespace", marker)
		require.Equal(t, recorded[marker], matches[0].ID, "fixture %q: the harness-recorded subject must be the marker-resolvable row", marker)

		found, err := contactRepo.ListContacts(ctx, repository.ListContactsParams{Query: marker, Limit: 200})
		require.NoError(t, err)
		scoped := make([]uuid.UUID, 0, 1)
		for _, c := range found {
			if strings.HasPrefix(c.FullName, prefix) {
				scoped = append(scoped, c.ID)
			}
		}
		require.Equal(t, []uuid.UUID{matches[0].ID}, scoped,
			"fixture %q must be returned by the contact SEARCH the tours resolve with, exactly once", marker)

		prior, dup := subjectOf[matches[0].ID]
		require.False(t, dup, "fixtures %q and %q resolved to the SAME contact — each fixture needs its own subject", prior, marker)
		subjectOf[matches[0].ID] = marker
		byMarker[marker] = matches[0]
	}

	flags, err := support.ListCadenceActivityFlagsByNamePrefix(ctx, prefix)
	require.NoError(t, err)
	flagsByID := make(map[uuid.UUID]repository.CadenceActivityFlags, len(flags))
	for _, f := range flags {
		flagsByID[f.ContactID] = f
	}

	// A9 — the delete victim carries no state of its own (it exists to be consumed),
	// so it is discharged ENTIRELY by the resolution rules above: it must be
	// recorded, resolve to exactly one contact, survive the search path, and be a
	// subject no other fixture shares. There is deliberately nothing further to
	// assert about it here.

	// A1 — the no-recent-activity subject. It also carries the tasks-section empty
	// state on the SAME page visit, so zero PRODUCT-VISIBLE tasks is part of the
	// fixture, asserted directly rather than inferred from its seeding position.
	noActivity := byMarker[synthetic.FixtureMarkerNoActivity]
	require.Nil(t, noActivity.LastOutreachAt, "no-activity fixture must carry no outreach")
	require.Nil(t, noActivity.LastResponseAt, "no-activity fixture must carry no response")
	require.Nil(t, noActivity.LastContacted, "no-activity fixture must never have been connected with")
	require.True(t, flagsByID[noActivity.ID].HasNone, "no-activity fixture must carry no pending follow-up either")
	require.NotNil(t, noActivity.Cadence, "no-activity fixture must be cadence-bearing (the recent-never-connected floor requires one)")
	require.NotEmpty(t, *noActivity.Cadence, "no-activity fixture must be cadence-bearing (the recent-never-connected floor requires one)")
	require.NotNil(t, noActivity.CreatedAt, "no-activity fixture must carry a creation timestamp")
	require.True(t, noActivity.CreatedAt.After(now.Add(-14*24*time.Hour)),
		"no-activity fixture must be recently created (it supplies the recent-never-connected floor)")
	visible, err := support.ListVisibleTaskCountsByContactIds(ctx, []uuid.UUID{noActivity.ID})
	require.NoError(t, err)
	require.Len(t, visible, 1, "the visible-task count query must answer for the no-activity fixture")
	require.Zero(t, visible[0].VisibleCount, "no-activity fixture must have zero product-visible tasks (the tasks empty state rides on it)")

	// A2 / A3 — outreach and response, on DISTINCT contacts (the distinctness is
	// already guaranteed above; what matters here is that each carries its column).
	outreach := byMarker[synthetic.FixtureMarkerOutreach]
	require.NotNil(t, outreach.LastOutreachAt, "outreach fixture must carry last_outreach_at")
	response := byMarker[synthetic.FixtureMarkerResponse]
	require.NotNil(t, response.LastResponseAt, "response fixture must carry last_response_at")

	// A4 — the awaiting-reply subject.
	pending := byMarker[synthetic.FixtureMarkerPending]
	require.True(t, flagsByID[pending.ID].HasPending, "pending fixture must carry a live follow-up loop")

	// A5 — the two designated overdue contacts. STATE guarantees, not subjects: no
	// tour resolves them, the dashboard and relationship-loop tours keep their
	// positional most-urgent selection. What is asserted is that the world contains
	// them and that each contributes a creation age no other qualifying contact has —
	// the diversity gap the frozen catalog cannot close on its own.
	overdueFixtures := []repository.PinnedFixtureContact{
		byMarker[synthetic.FixtureMarkerOverdueA],
		byMarker[synthetic.FixtureMarkerOverdueB],
	}
	diversityFloor := now.Add(-7 * 24 * time.Hour)
	for _, f := range overdueFixtures {
		require.NotNil(t, f.Cadence, "overdue fixture must be cadence-bearing")
		require.NotEmpty(t, *f.Cadence, "overdue fixture must be cadence-bearing")
		require.Nil(t, f.LastContacted, "overdue fixture must be never-connected (an honest empty timeline)")
		require.NotNil(t, f.CreatedAt, "overdue fixture must carry a creation timestamp")
		require.True(t, f.CreatedAt.Before(now.Add(-14*24*time.Hour)), "overdue fixture must be backdated past the 14d floor")
		require.NotNil(t, f.ContactBy, "overdue fixture must carry a computed contact_by")
		require.True(t, f.ContactBy.Before(now), "overdue fixture's computed contact_by must have elapsed")
		cadenceType, err := cadence.ParseCadence(*f.Cadence)
		require.NoError(t, err)
		require.True(t, cadence.IsOverdueWithConfig(cadenceType, nil, *f.CreatedAt, now),
			"overdue fixture must be overdue via the production cadence helper")
	}
	require.NotEqual(t, *overdueFixtures[0].Cadence, *overdueFixtures[1].Cadence, "the two overdue fixtures must carry distinct cadences")
	require.False(t, overdueFixtures[0].CreatedAt.Equal(*overdueFixtures[1].CreatedAt), "the two overdue fixtures must carry distinct creation ages")
	overdueFixtureIDs := make(map[uuid.UUID]bool, len(overdueFixtures))
	for _, f := range overdueFixtures {
		overdueFixtureIDs[f.ID] = true
	}
	for _, f := range overdueFixtures {
		// Comparisons are counted: with nothing to compare against, "adds a new age"
		// is trivially true, and a silently empty loop would look identical to a
		// satisfied one. The wider system property this contributes to — ≥3 distinct
		// old creation ages among the cadence-bearing never-connected rows — is gated
		// by the coherence floor, not here.
		compared := 0
		for _, row := range rows {
			if overdueFixtureIDs[row.ID] {
				continue
			}
			if row.Cadence == nil || *row.Cadence == "" || row.LastContacted != nil || row.CreatedAt == nil {
				continue
			}
			if !row.CreatedAt.Before(diversityFloor) {
				continue
			}
			compared++
			require.False(t, row.CreatedAt.Equal(*f.CreatedAt),
				"overdue fixture %s duplicates another contact's creation age — it adds no NEW distinct age to the diversity set", subjectOf[f.ID])
		}
		require.NotZero(t, compared,
			"overdue fixture %s was compared against no other qualifying contact — the distinct-age claim would pass vacuously", subjectOf[f.ID])
	}

	// The tours read the overdue set through GET /contacts/overdue, whose predicate
	// is the persisted contact_by against today's DATE — a strictly different
	// comparison from the `now` the column checks above use, so a fixture overdue by
	// less than a day passes every check above and is still absent from the list the
	// tours capture. Assert both fixtures come back from that predicate. Read
	// namespace-scoped and unbounded rather than through the production query's
	// global LIMIT, which an accumulated shared test DB could overflow.
	endpointOverdue, err := support.ListOverdueContactIdsByNamePrefix(ctx, prefix, cadence.Today(now))
	require.NoError(t, err)
	returnedByEndpoint := make(map[uuid.UUID]bool, len(endpointOverdue))
	for _, id := range endpointOverdue {
		returnedByEndpoint[id] = true
	}
	for _, f := range overdueFixtures {
		require.True(t, returnedByEndpoint[f.ID],
			"overdue fixture %s must be returned by the overdue endpoint's predicate, not merely carry overdue columns", subjectOf[f.ID])
	}

	// A7 — the merge pair. The preview flags a field conflicting only when the
	// SOURCE's value is non-empty and differs from the target's, so both fields are
	// checked from that side.
	mergeTarget := byMarker[synthetic.FixtureMarkerMergeTarget]
	mergeSource := byMarker[synthetic.FixtureMarkerMergeSource]
	require.NotNil(t, mergeTarget.Cadence, "the merge target must carry a cadence — the pair conflicts on it")
	require.NotNil(t, mergeSource.Cadence, "the merge source must carry a cadence — a conflict needs the source value set")
	require.NotEqual(t, *mergeTarget.Cadence, *mergeSource.Cadence, "the merge pair must differ in cadence")
	require.NotNil(t, mergeTarget.Location, "the merge target must carry a location — the pair conflicts on it")
	require.NotNil(t, mergeSource.Location, "the merge source must carry a location — a conflict needs the source value set")
	require.NotEqual(t, *mergeTarget.Location, *mergeSource.Location,
		"the merge pair must differ in location too — one conflicting field is not enough to prove the preview surfaces the ACTUALLY-conflicting ones")

	// A8 — the searched navigation subject must survive the has_cadence filter.
	searchSubject := byMarker[synthetic.FixtureMarkerSearch]
	require.NotNil(t, searchSubject.Cadence, "the search subject must be cadence-bearing (it is reached through cadence_filter=has_cadence)")
	require.NotEmpty(t, *searchSubject.Cadence, "the search subject must be cadence-bearing (it is reached through cadence_filter=has_cadence)")

	// DOCUMENTATION, NOT A GATE — A6: the "≥3 unique-named contacts" requirement is
	// about the subjects the contacts tour resolves by exact name text. That is five,
	// not three: the merge pair and the navigation subject go through the tour's
	// uniqueName check, the delete victim does too, and the birthday card is matched
	// by hasText on its full name. This check is DOMINATED by the exactly-one-match
	// rule above (any row sharing a fixture's full_name necessarily contains that
	// fixture's marker, so it would fail there first) and is kept as a statement of
	// what the tour's selectors need.
	nameCount := map[string]int{}
	for _, row := range rows {
		nameCount[row.FullName]++
	}
	for _, marker := range []string{
		synthetic.FixtureMarkerMergeTarget,
		synthetic.FixtureMarkerMergeSource,
		synthetic.FixtureMarkerSearch,
		synthetic.FixtureMarkerDelete,
		synthetic.FixtureMarkerBirthday,
	} {
		require.Equal(t, 1, nameCount[byMarker[marker].FullName],
			"fixture %q must carry a name unique in the world — the merge selector filters by exact name text", marker)
	}

	// A10 — the dedicated highlight-window birthday. Pinned by DAY COUNT, which is
	// date-independent; the page's section for it is not (see the fixture's own
	// comment), so the section is deliberately not asserted.
	birthday := byMarker[synthetic.FixtureMarkerBirthday]
	require.NotNil(t, birthday.Birthday, "the birthday fixture must have a populated birthday cache")
	require.Equal(t, synthetic.FixtureBirthdayOffsetDays, synthetic.BirthdayDaysUntil(*birthday.Birthday, anchor),
		"the birthday fixture must be exactly %d days out", synthetic.FixtureBirthdayOffsetDays)

	// B-group non-degeneracy. These selections are deliberately NOT pinned — they
	// test the ordering itself — so what is asserted is that the ordering is
	// well-defined for them. rows arrive in the tours' default cadence order.
	require.GreaterOrEqual(t, len(rows), 2, "the navigation boundary captures need ≥2 contacts in the order")
	sortKey := func(c repository.PinnedFixtureContact) string {
		cadenceValue := ""
		if c.Cadence != nil {
			cadenceValue = *c.Cadence
		}
		return cadenceValue + "\x00" + c.FullName
	}
	require.NotEqual(t, sortKey(rows[0]), sortKey(rows[1]),
		"the first two rows of the default order must have distinct sort keys — a tie there is broken by a per-run random id, so the tour's first-row subject would rebind between sweeps")
	for _, marker := range synthetic.PinnedFixtureMarkers {
		require.NotEqual(t, rows[0].ID, byMarker[marker].ID,
			"pinned fixture %q must not occupy the first row of the default order — the contacts tour mutates that row and reserves the fixtures separately", marker)
	}
}

// These profile-orchestration integration tests are SLOW-gated
// (testsupport.RequireLongTests) and routed to the nightly slow suite via the
// TestSynthetic name prefix. Each sub-test uses a UNIQUE namespace so
// shared-test-DB reuse cannot collide; assertions read namespace-scoped harness
// repos (NEVER global counts — the shared DB accumulates).
//
// The profiles run with SMALL overridden counts so a settle-heavy prod-shaped
// world does not blow the CI/pre-push budget; the volume is a profile default
// the orchestration multiplies over, not a behavior the orchestration depends
// on, so a small count exercises the same code paths.

// TestSyntheticProfile_MinimalScopedMatchesSeedAll proves the minimal-scoped
// profile is byte-stable == today's SeedAll + DefaultParams (the golden-scenario
// regression depends on this).
func TestSyntheticProfile_MinimalScopedMatchesSeedAll(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	params, err := synthetic.ProfileParams(synthetic.ProfileMinimalScoped)
	require.NoError(t, err)
	params.Namespace = syntheticNS(t)

	h := synthetic.NewHarnessForNamespace(t, ctx, database, params.Namespace, params.Seed)
	res, err := synthetic.RunProfile(ctx, h, params)
	require.NoError(t, err)
	require.Equal(t, synthetic.ProfileMinimalScoped, res.Profile)
	// minimal-scoped == SeedAll: one Gmail-settled + one Telegram-settled contact.
	require.Equal(t, 1, res.GmailSettled)
	require.Equal(t, 1, res.TelegramSettled)
	require.Equal(t, 2, res.Contacts)
	// minimal-scoped reports its one `seed-all` phase, so all three profiles
	// surface timings uniformly — otherwise this path's phase bracket could be
	// dropped without any test noticing.
	require.Len(t, res.Timings.Phases, 1, "minimal-scoped reports its single phase")
	require.Equal(t, "seed-all", res.Timings.Phases[0].Name)
	require.Positive(t, res.Timings.Settle.Calls, "minimal-scoped reports settle accounting")

	remaining, err := h.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(2), remaining, "minimal-scoped seeds exactly two contacts")
}

// TestSyntheticProfile_DevCoversCatalog asserts the dev profile produces the
// producible edge-case + pending coverage. Counts are scoped to the harness's
// namespace.
// assertSeedCoherence runs the post-seed coherence gate over one namespace: no
// production-impossible last_contacted (gate a), a non-vacuous interaction-moved
// cohort (gate a-positive), prod-shaped managed follow-ups (gate c) whose marker
// decodes and points at the seeded contact (gate c-Go), a cause-driven overdue
// cohort computed via the production cadence helper (env-robust), the CAD-029
// tour capacity (four distinct assignable contacts), and no stranded
// knowledge-cache columns (gate f: every current-accepted cutover assertion has a
// populated cache column, and — when dateFactID is set — the specific target row,
// the ReplayAssertion date-fact birthday's own contact, has a populated birthday
// cache). Called by both coverage tests. dateFactID is h.DateFactContactID() (the
// contact the date-fact block seeded; uuid.Nil when that block did not run).
func assertSeedCoherence(t *testing.T, ctx context.Context, support *repository.SyntheticSupportRepository, prefix string, dateFactID uuid.UUID) {
	t.Helper()
	now := accelerated.GetCurrentTime()

	// Gate (a): zero production-impossible last_contacted (a non-creation, non-null
	// last_contacted must be backed by a live inbound/mutual interaction at the same
	// occurred_at, with last_interaction_at in lockstep).
	incoherent, err := support.IncoherentLastContactedCountByNamePrefix(ctx, prefix)
	require.NoError(t, err)
	require.Equal(t, int64(0), incoherent, "no contact may carry a moved last_contacted without a backing interaction + lockstep last_interaction_at")

	// Gate (a-positive): the interaction-moved cohort exists (proves gate a is not
	// vacuous) and sets last_interaction_at in lockstep.
	backed, err := support.InteractionBackedLastContactedCountByNamePrefix(ctx, prefix)
	require.NoError(t, err)
	require.GreaterOrEqual(t, backed, int64(1), "≥1 contact whose last_contacted is backed by an inbound/mutual interaction with lockstep last_interaction_at")

	// Gate (c): managed follow-ups carry the production Todoist shape.
	incoherentFU, err := support.IncoherentManagedFollowUpCountByNamePrefix(ctx, prefix)
	require.NoError(t, err)
	require.Equal(t, int64(0), incoherentFU, "every managed follow-up loop must carry an alphanumeric external id + prod metadata + content/marker shape")

	// Gate (c-Go): the follow-up's marker decodes and references its own contact.
	fu, err := support.GetLiveFollowUpByNamePrefix(ctx, prefix)
	require.NoError(t, err)
	var meta map[string]any
	require.NoError(t, json.Unmarshal(fu.Metadata, &meta))
	markerJSON, _ := meta["marker_json"].(string)
	marker, ok := contacttask.DecodeMarker(markerJSON)
	require.True(t, ok, "follow-up marker_json must decode")
	require.Equal(t, fu.ContactID.String(), marker.ContactID, "marker must reference the follow-up's own contact")
	require.Equal(t, contacttask.KindReachOut, marker.Kind)
	require.Equal(t, contacttask.LifecycleFollowUpLoop, marker.Lifecycle)
	instanceID, _ := meta["integration_instance_id"].(string)
	require.NotEmpty(t, marker.Instance, "marker instance must be non-empty")
	require.Equal(t, instanceID, marker.Instance, "marker instance must agree with integration_instance_id metadata")
	content, _ := meta["content"].(string)
	require.Contains(t, content, "/contacts/"+fu.ContactID.String(), "follow-up content must link the contact")

	// Cause-driven overdue cohort + the D1 verification, computed from the bucket
	// rows via the production cadence helper (env-robust: it does not matter whether
	// the harness runs with time acceleration on).
	buckets, err := support.ListContactBucketsByNamePrefix(ctx, prefix)
	require.NoError(t, err)
	overdueFloor := now.Add(-14 * 24 * time.Hour)
	var overdueByHelper, backdatedOverdue int
	for _, b := range buckets {
		if b.Cadence == nil || *b.Cadence == "" || b.CreatedAt == nil {
			continue
		}
		cadenceType, perr := cadence.ParseCadence(*b.Cadence)
		if perr != nil {
			continue
		}
		if cadence.IsOverdueWithConfig(cadenceType, b.LastContacted, *b.CreatedAt, now) {
			overdueByHelper++
		}
		// D1: a backdated (created-long-ago) contact is overdue with an honest empty
		// timeline — created_at far in the past, last_contacted NULL (no connection
		// ever happened, CON-001), and a computed contact_by already elapsed. The
		// production cadence helper is invoked ON THIS ROW with a nil last_contacted
		// and must agree it is overdue, proving the created_at fallback (CAD-002[0])
		// drives overdue-ness directly rather than any residual last_contacted value.
		if b.LastContacted == nil &&
			b.CreatedAt.Before(overdueFloor) &&
			b.ContactBy != nil && b.ContactBy.Before(now) &&
			cadence.IsOverdueWithConfig(cadenceType, nil, *b.CreatedAt, now) {
			backdatedOverdue++
		}
	}
	require.GreaterOrEqual(t, overdueByHelper, 1, "≥1 overdue contact via the production cadence helper")
	require.GreaterOrEqual(t, backdatedOverdue, 1, "≥1 backdated overdue contact (far-past created_at, NULL last_contacted, contact_by elapsed)")

	// Tour capacity (CAD-029): the world must contain FOUR DISTINCT contacts
	// assignable to the outreach / response / pending / none states — the precondition
	// for cadence-followup.tour.ts. Proven via a maximum bipartite matching that must
	// saturate all four states.
	flags, err := support.ListCadenceActivityFlagsByNamePrefix(ctx, prefix)
	require.NoError(t, err)
	require.Equal(t, 4, maxDistinctStateAssignment(flags), "four CAD-029 states must be assignable to four distinct contacts")

	// Gate (f): zero stranded knowledge-cache columns (every current-accepted cutover
	// assertion on a live contact has its derived cache column populated — the
	// production-impossible state where an accepted assertion exists but its cache
	// column is NULL is absent).
	stranded, err := support.StrandedKnowledgeCacheCountByNamePrefix(ctx, prefix, now)
	require.NoError(t, err)
	require.Equal(t, int64(0), stranded, "no live contact may hold a current-accepted cutover assertion (lives_in/birthday/how_met) whose derived cache column is NULL")

	// Gate (f-positive): the specific target row is coherent. A generic ">=1 populated
	// cutover cache" would pass on the contact-create authority-flip bio facts alone, so
	// target the ReplayAssertion date-fact birthday's OWN contact and assert its birthday
	// cache is populated — proving the refresh reaches the exact row that would otherwise
	// be stranded, not just that some cutover cache is populated. (lives_in/how_met
	// non-vacuity rides the existing ContactsWithLocation/ContactsWithHowMet coverage
	// assertions feeding gate f.)
	if dateFactID != uuid.Nil {
		cache, err := support.GetContactCacheColumns(ctx, dateFactID)
		require.NoError(t, err)
		require.NotNil(t, cache.Birthday, "the ReplayAssertion date-fact birthday's contact must have a populated birthday cache (target row)")
	}
}

// maxDistinctStateAssignment returns the size of a maximum matching between the
// four CAD-029 states (outreach, response, pending, none) and the given contacts,
// where a state may be assigned to any contact carrying that flag and each contact
// covers at most one state. Standard augmenting-path (Kuhn's) search — trivial at
// this scale.
func maxDistinctStateAssignment(flags []repository.CadenceActivityFlags) int {
	// state index → the flag each state requires on a contact.
	stateHas := []func(repository.CadenceActivityFlags) bool{
		func(f repository.CadenceActivityFlags) bool { return f.HasOutreach },
		func(f repository.CadenceActivityFlags) bool { return f.HasResponse },
		func(f repository.CadenceActivityFlags) bool { return f.HasPending },
		func(f repository.CadenceActivityFlags) bool { return f.HasNone },
	}
	contactToState := make([]int, len(flags))
	for i := range contactToState {
		contactToState[i] = -1
	}
	var augment func(state int, seen []bool) bool
	augment = func(state int, seen []bool) bool {
		for ci, f := range flags {
			if !stateHas[state](f) || seen[ci] {
				continue
			}
			seen[ci] = true
			if contactToState[ci] == -1 || augment(contactToState[ci], seen) {
				contactToState[ci] = state
				return true
			}
		}
		return false
	}
	matched := 0
	for s := 0; s < len(stateHas); s++ {
		if augment(s, make([]bool, len(flags))) {
			matched++
		}
	}
	return matched
}

// f4MessageSources is the exact message-source allowlist the two-sided
// direction gate (F4, d-positive) asserts non-inbound coverage for. F4 measured 0
// outbound/mutual interactions for EACH of these four sources. It DELIBERATELY
// excludes gcal — the awaiting-reply GCal event is already mutual, so including it
// would let the per-source assertion pass without any message-source direction
// coverage — plus todoist / manual / anarlog_sessions / phone_calls.
var f4MessageSources = []string{
	repository.InteractionSourceEmail,
	repository.InteractionSourceTelegram,
	repository.InteractionSourceGChat,
	repository.InteractionSourceMessages,
}

// assertMessageDirectionCoverage runs the F4 two-sided message-direction gate over
// one namespace: no last_outreach_at without a backing outbound/mutual interaction
// (gate d), each of the four message sources carries ≥1 outbound/mutual interaction
// (gate d-positive, was 0/0/0/0), the seeded cohort counts match (3 outbound-only + 1
// mutual), the telegram-mutual contact's two seeded messages collapsed into exactly
// one promoted mutual row (gate d-mutual-verify), and that promoted mutual set all
// four cadence timestamps non-null and equal. Called by both coverage tests.
func assertMessageDirectionCoverage(t *testing.T, ctx context.Context, support *repository.SyntheticSupportRepository, h *synthetic.Harness, res synthetic.ProfileResult) {
	t.Helper()
	prefix := h.Generator().Prefix()

	// Gate (d): zero last_outreach_at values not backed by a live outbound/mutual
	// interaction at the same occurred_at.
	incoherentOut, err := support.IncoherentOutreachCountByNamePrefix(ctx, prefix)
	require.NoError(t, err)
	require.Equal(t, int64(0), incoherentOut, "no contact may carry a moved last_outreach_at without a backing outbound/mutual interaction")

	// Gate (d, not-vacuous): the zero-violation check above is only meaningful if the
	// world actually contains last_outreach_at values. Assert the PURE-OUTBOUND cohort
	// exists (last_outreach_at set, last_contacted NULL) — the three outbound-only
	// contacts (gmail/gchat/imessage). Without this a world with no last_outreach_at at
	// all would satisfy the == 0 gate vacuously.
	pureOutbound, err := support.PureOutboundContactCountByNamePrefix(ctx, prefix)
	require.NoError(t, err)
	require.GreaterOrEqual(t, pureOutbound, int64(1), "≥1 pure-outbound contact (last_outreach_at set, last_contacted NULL) — the outbound gate is not vacuous")

	// Gate (d-positive): EACH message source has ≥1 outbound/mutual interaction (the
	// F4 fix is per-source; every one measured 0 before this).
	for _, source := range f4MessageSources {
		n, err := support.NonInboundMessageInteractionCountBySourceByNamePrefix(ctx, prefix, source)
		require.NoError(t, err)
		require.GreaterOrEqual(t, n, int64(1), "message source %q must carry ≥1 outbound/mutual interaction (F4)", source)
	}

	// The block seeds a fixed cohort: 3 outbound-only (gmail/gchat/imessage) + 1
	// telegram mutual, in both profiles.
	require.Equal(t, 3, res.OutboundOnlyContacts, "three outbound-only message contacts seeded")
	require.Equal(t, 1, res.MutualMessageContacts, "one reply-bridged telegram mutual contact seeded")

	// Gate (d-mutual-verify): the telegram-mutual contact's two seeded messages must
	// have COLLAPSED into a single promoted mutual row — proving
	// PromoteInteractionToMutualTx ran, not that an outbound and an inbound coexist.
	mutualID := h.MutualMessageContactID()
	require.NotEqual(t, uuid.Nil, mutualID, "the two-sided block must have recorded the mutual contact id")
	dirCounts, err := support.InteractionDirectionCountsForContact(ctx, mutualID, repository.InteractionSourceTelegram)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{repository.InteractionDirectionMutual: 1}, dirCounts,
		"telegram-mutual contact must have exactly one live mutual interaction and no residual outbound/inbound rows")

	// Gate (d-mutual-verify, timestamps): the promoted mutual set all four cadence
	// columns together (mutual writes last_contacted / last_interaction_at /
	// last_outreach_at / last_response_at to the same replyAt).
	ts, err := support.ContactCadenceTimestampsForContact(ctx, mutualID)
	require.NoError(t, err)
	require.NotNil(t, ts.LastContacted, "mutual contact last_contacted must be set")
	require.NotNil(t, ts.LastInteractionAt, "mutual contact last_interaction_at must be set")
	require.NotNil(t, ts.LastOutreachAt, "mutual contact last_outreach_at must be set")
	require.NotNil(t, ts.LastResponseAt, "mutual contact last_response_at must be set")
	require.True(t, ts.LastContacted.Equal(*ts.LastInteractionAt), "mutual last_contacted == last_interaction_at")
	require.True(t, ts.LastContacted.Equal(*ts.LastOutreachAt), "mutual last_contacted == last_outreach_at")
	require.True(t, ts.LastContacted.Equal(*ts.LastResponseAt), "mutual last_contacted == last_response_at")
}

// assertArchetypeCohorts is the archetype coverage gate both catalog profiles
// run: the assignment the seed applied must be the one the pure function
// derives, the cohorts the ladder promises must exist, their samples must
// actually DIFFER, and the two archetype counters must show the collapse rather
// than agree.
//
// The cohort and multi-sample floors are gated by catalog size, and the two
// thresholds are deliberately different. Per-archetype PRESENCE is asserted from
// n >= 12, where the ladder switches to the full assignment. The >=2-samples
// floor is asserted from n >= 13, and the derivation is arithmetic: the no-method
// slot is structurally history-free whatever archetype it is given, leaving n - 1
// usable slots, and six history archetypes at two samples each need twelve of
// them. At n = 12 exactly the floor is unsatisfiable over the frozen catalog, and
// archetypes add history rather than contacts, so it cannot be fixed by seeding
// another slot.
func assertArchetypeCohorts(t *testing.T, ctx context.Context, h *synthetic.Harness, res synthetic.ProfileResult, n int) {
	t.Helper()

	samples := h.ArchetypeSamples()
	require.Len(t, samples, n, "the archetype block records one sample per catalog slot, including the history-free ones")

	cohorts := map[factory.Archetype][]replay.ArchetypeSample{}
	totalExpected := 0
	for i, sample := range samples {
		require.Equal(t, i, sample.SlotIndex, "samples are recorded in frozen catalog order")
		require.Equal(t, synthetic.ArchetypeForIndex(i, n), sample.Archetype,
			"slot %d must carry the archetype the assignment independently derives", i)
		cohorts[sample.Archetype] = append(cohorts[sample.Archetype], sample)
		totalExpected += sample.ExpectedInteractions
	}

	// The collapse relation between the two counters, never an equality: a mail
	// promotion pair lands one mutual row and a chat burst lands one session, so
	// rows are strictly fewer than payloads wherever any history exists.
	require.Equal(t, totalExpected, res.ArchetypeInteractions,
		"the landed-row counter must equal the sum of the per-contact expectations")
	if res.ArchetypePayloads > 0 {
		require.Greater(t, res.ArchetypePayloads, res.ArchetypeInteractions,
			"payloads must exceed landed rows — that difference IS the aggregation collapse")
	}
	assertArchetypeSettleBudget(t, res)

	if n < 12 {
		return
	}
	for _, archetype := range []factory.Archetype{
		factory.ArchetypeMutualRegular,
		factory.ArchetypeMutualDrifting,
		factory.ArchetypeDormant,
		factory.ArchetypeOutboundHeavy,
		factory.ArchetypeInboundOnly,
		factory.ArchetypeBurstThenQuiet,
		factory.ArchetypeNeverContacted,
	} {
		require.NotEmpty(t, cohorts[archetype], "n=%d: every archetype must have a cohort on the full assignment rung (%s)", n, archetype)
	}

	if n < 13 {
		return
	}
	for archetype, cohort := range cohorts {
		if archetype == factory.ArchetypeNeverContacted {
			continue
		}
		require.GreaterOrEqual(t, len(cohort), 2, "n=%d: %s must carry >=2 samples", n, archetype)
		// The distinguisher is the full per-contact occurred_at MULTISET, not a
		// (row count, newest age) projection: two legitimately jittered samples
		// collide on that projection often enough to be a designed-in flake.
		distinct := map[string]bool{}
		for _, sample := range cohort {
			rows, err := h.InteractionRepo().ListContactInteractions(ctx, sample.ContactID, 500, 0)
			require.NoError(t, err)
			instants := make([]string, 0, len(rows))
			for _, row := range rows {
				instants = append(instants, row.OccurredAt.UTC().Format(time.RFC3339Nano))
			}
			sort.Strings(instants)
			distinct[strings.Join(instants, "|")] = true
		}
		require.GreaterOrEqual(t, len(distinct), 2,
			"n=%d: %s must carry >=2 samples with DIFFERENT interaction timelines — jitter has to be observable, not merely drawn", n, archetype)
	}
}

// assertArchetypeSettleBudget pins the reason this phase batches at all.
//
// Settle is O(all harness contacts) and rebuilds the whole event-id union on
// every call, so the phase's cost is its SETTLE count, not its payload count.
// Batching collapses that to one Settle per dependency GENERATION — a handful for
// the whole catalog — where the same history through per-payload single replays
// would cost one per payload: roughly a hundred at dev and a thousand at
// prod-shaped. A regression from batch replay back to single replay would leave
// every other assertion in this suite passing while the reseed's gate wait
// regressed by about two orders of magnitude, which is precisely what this bound
// exists to catch.
func assertArchetypeSettleBudget(t *testing.T, res synthetic.ProfileResult) {
	t.Helper()
	if res.ArchetypePayloads == 0 {
		require.Zero(t, res.ArchetypeSettleCalls, "no payloads means no settles")
		return
	}
	require.Positive(t, res.ArchetypeSettleCalls, "driving payloads must settle at least once")
	// Calendar, up to two mail buckets and chat, each at most two generations.
	const maxArchetypeSettles = 8
	require.LessOrEqual(t, res.ArchetypeSettleCalls, maxArchetypeSettles,
		"the archetype phase settled %d times for %d payloads — a per-payload replay loop looks exactly like this",
		res.ArchetypeSettleCalls, res.ArchetypePayloads)
	require.Less(t, res.ArchetypeSettleCalls, res.ArchetypePayloads,
		"settles must stay strictly below payloads — a per-payload replay loop settles once PER payload, so equality is the regression")
}

// assertOverdueBand measures the overdue population from the DATABASE, TWICE —
// once by recomputing overdue-ness from (cadence, last_contacted, created_at),
// and once through the predicate GET /contacts/overdue actually runs — and holds
// each against its own model, the absolute ceiling, and the target share.
//
// Measuring twice is the point. The two quantities are not the same, and the
// difference is not a rounding error: contact_by is written FORWARD-ONLY, so an
// archetype whose newest two-way entry predates its contact's created_at moves
// last_contacted backwards past created_at while leaving contact_by at the
// creation-time value. The recompute calls that contact overdue; the endpoint
// does not return it. Asserting the band on the recompute alone let a
// nine-contact divergence ship silently to staging (gh #751), because the band
// is a statement about what the PRODUCT shows and the recompute is not that.
//
// The recompute constructs PRODUCTION cadence durations explicitly rather than
// reading the ambient config, so it states a production-duration expectation
// whatever CRM_ENV happens to be. The persisted column cannot do that — it was
// written by the seed through the ambient config — so the production-duration
// precondition is asserted outright below instead of assumed.
//
// The prediction-versus-measurement equalities are what keep the assignment's
// MODELS honest: a model is never the assertion, the database is.
//
// wantNonCatalogLive is how many LIVE contacts the profile seeds outside the
// catalog, so the live denominator is pinned rather than merely read. Without it
// the share assertions accept any denominator that keeps the ratio in band — at
// dev that is a 23-contact window — and a profile knob change would move the world
// without moving a single assertion.
func assertOverdueBand(t *testing.T, ctx context.Context, support *repository.SyntheticSupportRepository, h *synthetic.Harness, n, wantNonCatalogLive int) {
	t.Helper()

	// Precondition, not an assumption. The persisted contact_by column holds
	// base + AMBIENT cadence period, so the endpoint-visible count below is the
	// production quantity only while the ambient table IS the production one.
	// Under CRM_ENV=test annual is two hours and every column would be minutes
	// wide, which would make the band meaningless rather than merely wrong — so
	// fail loudly and say why, rather than grading nonsense.
	require.Equal(t, cadence.ProductionCadenceConfig(), cadence.GetCadenceConfig(),
		"the overdue band is a production-duration statement and the persisted contact_by column is written through the AMBIENT cadence table; run this suite without a compressed CRM_ENV")

	buckets, err := support.ListContactBucketsByNamePrefix(ctx, h.Generator().Prefix())
	require.NoError(t, err)
	anchor := h.Generator().Anchor()

	require.Equal(t, n+wantNonCatalogLive, len(buckets),
		"the live population must be the catalog (%d) plus this profile's non-catalog cohort (%d) — the overdue share's denominator is pinned, not merely read, so a knob change that moves the world cannot leave every assertion still passing",
		n, wantNonCatalogLive)

	recomputedIDs := make([]uuid.UUID, 0, len(buckets))
	for _, b := range buckets {
		if b.Cadence == nil || *b.Cadence == "" || b.CreatedAt == nil {
			continue
		}
		if synthetic.OverdueAtProduction(*b.Cadence, b.LastContacted, *b.CreatedAt, anchor) {
			recomputedIDs = append(recomputedIDs, b.ID)
		}
	}
	overdue := len(recomputedIDs)

	// The endpoint's own read: contact_by set and strictly before today's DATE,
	// namespace-scoped and unbounded (the production query's global LIMIT would
	// let an accumulated shared test DB truncate the window). Evaluated at the
	// same anchor the recompute uses, so the two reads answer one question about
	// one instant instead of straddling the seed's own duration.
	endpointOverdueIDs, err := support.ListOverdueContactIdsByNamePrefix(ctx, h.Generator().Prefix(), cadence.Today(anchor))
	require.NoError(t, err)
	endpointOverdue := len(endpointOverdueIDs)

	// MEMBERSHIP first, and over the id SETS rather than their sizes. The endpoint
	// returns contact_by in the past, which the forward-only write can only ever
	// make a subset of the recomputed set — a contact the endpoint lists as overdue
	// while the recompute does not means the persisted column and the cadence
	// formula have drifted apart, which is a different and worse failure than a
	// count being off. Comparing counts would miss it entirely whenever the
	// cardinalities happen to match, and the two exact equalities below would
	// happily pass on a set with the wrong members in it.
	require.Subset(t, recomputedIDs, endpointOverdueIDs,
		"every contact the overdue ENDPOINT returns must also be overdue by the cadence formula — an endpoint-only overdue contact means contact_by no longer agrees with (cadence, last_contacted, created_at)")

	// The pinned overdue fixtures sit outside the catalog and are always overdue,
	// so the assignment's catalog predictions account for them separately. The
	// count is READ from the seed rather than restated here, so a change to the
	// fixture table cannot drift the budget and these assertions apart.
	require.Equal(t, synthetic.PredictedCatalogOverdue(n)+synthetic.PinnedOverdueFixtureCount, overdue,
		"the recomputed overdue population must equal what the assignment predicts — a drifting model has to fail here rather than be trusted")
	require.Equal(t, synthetic.PredictedCatalogOverduePersisted(n)+synthetic.PinnedOverdueFixtureCount, endpointOverdue,
		"the ENDPOINT-visible overdue population must equal what the assignment predicts for the persisted contact_by column — this is the assertion gh #751 was missing. Its anti-revert margin is the gap between the two models AT THIS n: three contacts at n=18, and ZERO at n=9, where they coincide and this assertion cannot detect a revert at all (pinned in TestArchetypeAssignmentOverdueBand)")
	require.LessOrEqual(t, overdue, synthetic.OverdueCeiling,
		"the overdue population must stay under the ceiling; the overdue tours refuse to run above their own, higher, capture cap")

	if n < 12 {
		return
	}
	live := len(buckets)
	require.Positive(t, live)
	share := 100.0 * float64(endpointOverdue) / float64(live)
	require.GreaterOrEqual(t, share, synthetic.OverdueBandFloorPercent,
		"endpoint-visible overdue share %.1f%% of %d live contacts is below the target band", share, live)
	require.LessOrEqual(t, share, synthetic.OverdueBandCeilingPercent,
		"endpoint-visible overdue share %.1f%% of %d live contacts is above the target band", share, live)
}

func TestSyntheticProfile_DevCoversCatalog(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	params, err := synthetic.ProfileParams(synthetic.ProfileDev)
	require.NoError(t, err)
	params.Namespace = syntheticNS(t)

	h := synthetic.NewHarnessForNamespace(t, ctx, database, params.Namespace, params.Seed)
	res, err := synthetic.RunProfile(ctx, h, params)
	require.NoError(t, err)

	// Catalog contacts are seeded WITHOUT a settling replay, so the per-source
	// settled counts reflect the DEDICATED settled contacts (≥1 each), NOT the
	// catalog size. Total contacts = catalog (SeededContacts) + the settled set.
	require.Greater(t, res.Contacts, params.Counts.SeededContacts, "catalog + dedicated settled contacts")
	require.GreaterOrEqual(t, res.GmailSettled, 1)
	require.GreaterOrEqual(t, res.TelegramSettled, 1)
	require.GreaterOrEqual(t, res.GCalSettled, 1)
	require.GreaterOrEqual(t, res.GChatSettled, 1)
	require.GreaterOrEqual(t, res.IMessageSettled, 1)
	require.Equal(t, params.Counts.UnmatchedExternal, res.UnmatchedExternal)
	require.Equal(t, params.Counts.StrandedTelegram, res.StrandedTelegram)
	require.Equal(t, params.Counts.StrandedMessages, res.StrandedMessages)
	require.Equal(t, params.Counts.UnmatchedCalendar, res.UnmatchedCalendar)
	require.Equal(t, params.Counts.OrphanMeetingNote, res.OrphanMeetingNote)
	require.Equal(t, params.Counts.SeededAssertions, res.SeededAssertions)
	require.Equal(t, 2, res.SeededAssertions, "dev profile seeds graph assertions")
	// Bio facts (birthday / how_met) ride on the contact-create authority flip,
	// spread by catalog index; the dev catalog is large enough to carry ≥1 of each.
	require.GreaterOrEqual(t, res.ContactsWithBirthday, 1, "dev profile seeds birthday bio facts")
	require.GreaterOrEqual(t, res.ContactsWithHowMet, 1, "dev profile seeds how_met bio facts")
	// Cadence tasks: the dev catalog is cadence-bearing, so ReplayTodoist seeds a
	// managed task per contact (res.SeededTasks is the actual rows, a catalog-wide
	// count, not the >0 gate value in Counts.SeededTasks), and one is driven to
	// `unmanaged` via the real recurring edit. Same F7 coherence gate as the
	// prod-shaped profile: both prod-reachable states present, zero prod-impossible
	// (completed/dismissed or non-finalized external id) cadence_due rows.
	require.GreaterOrEqual(t, res.SeededTasks, 1, "dev profile seeds cadence tasks")
	{
		cadenceSupport := repository.NewSyntheticSupportRepository(database.Queries)
		devPrefix := h.Generator().Prefix()
		managedCadence, err := cadenceSupport.CountCadenceDueByStateByNamePrefix(ctx, string(repository.ContactTaskStateManaged), devPrefix)
		require.NoError(t, err)
		require.GreaterOrEqual(t, managedCadence, int64(1), "≥1 managed cadence_due task (reconcile default)")
		unmanagedCadence, err := cadenceSupport.CountCadenceDueByStateByNamePrefix(ctx, string(repository.ContactTaskStateUnmanaged), devPrefix)
		require.NoError(t, err)
		require.GreaterOrEqual(t, unmanagedCadence, int64(1), "≥1 unmanaged cadence_due task (real recurring-edit path)")
		incoherentCadence, err := cadenceSupport.IncoherentCadenceDueCountByNamePrefix(ctx, devPrefix)
		require.NoError(t, err)
		require.Equal(t, int64(0), incoherentCadence, "no cadence_due task may be completed/dismissed or carry a non-finalized external id")
		// Product-visible task spread (dev catalog is 18 ≥ 4, so the spread runs): the
		// same fixed 0/1/multiple manual-task allocation + exact accounting.
		assertVisibleTaskSpread(t, ctx, cadenceSupport, h, res, devPrefix)
	}
	// Value-type + edge graph rows: the dev catalog (18) far exceeds the small
	// dev knob counts, so the seeded counts are exact (no catalog-size bounding).
	// The clock-anchored birthday date-facts are asserted subject-scoped below via
	// assertBirthdayFixtures.
	require.Equal(t, params.Counts.SeededBoolFacts, res.SeededBoolFacts, "dev profile seeds bool facts")
	require.Equal(t, params.Counts.SeededRelationships, res.SeededRelationships, "dev profile seeds person→person edges")
	// Entity pool + person→entity edges: the dev catalog (18) far exceeds the small
	// dev knob counts, so the seeded counts are exact (no catalog-size bounding).
	require.Equal(t, params.Counts.SeededEntities, res.SeededEntities, "dev profile seeds the entity pool")
	require.Equal(t, params.Counts.SeededEntityEdges, res.SeededEntityEdges, "dev profile seeds person→entity edges")
	// relationship_signal rows: the dev catalog (18) exceeds the small dev signal
	// knob, so the seeded count is exact (no catalog-size bounding).
	require.Equal(t, params.Counts.SeededSignals, res.SeededSignals, "dev profile seeds relationship signals")
	// lives_in locations ride the contact-create authority flip, spread by catalog
	// index; the dev catalog is large enough to carry ≥1.
	require.GreaterOrEqual(t, res.ContactsWithLocation, 1, "dev profile seeds location bio facts")
	// MessagesPerContact spreads MULTIPLE settled interactions per dedicated source
	// contact: SettledInteractions == (settled contacts) × MessagesPerContact — PLUS the
	// single GCal event the awaiting-reply scenario replays (one per seeded follow-up).
	// That event is the OUTBOUND half of the scenario's causal chain: mutual direction
	// (CAD-006) gives the contact the last_outreach_at a follow-up requires (CAD-011).
	settledContacts := res.GmailSettled + res.TelegramSettled + res.GCalSettled + res.GChatSettled + res.IMessageSettled
	require.Equal(t, settledContacts*params.Counts.MessagesPerContact+res.SeededPendingFollowUps, res.SettledInteractions,
		"dev profile spreads MessagesPerContact interactions per settled contact, plus the awaiting-reply scenario's GCal event")
	require.Greater(t, res.SettledInteractions, settledContacts, "dev profile seeds >1 interaction per settled contact")
	// Merge + soft-delete scenarios are standalone contacts seeded at full count
	// (independent of catalog size), so the dev result equals the dev knobs.
	require.Equal(t, params.Counts.SeededSoftDeleted, res.SeededSoftDeleted, "dev profile seeds soft-deleted contacts")
	require.Equal(t, params.Counts.SeededMerged, res.SeededMerged, "dev profile seeds merged contact pairs")

	// Import-source candidates: one per subtab per ImportCandidatesPerSource, seeded at
	// full count (independent of catalog size), so each result equals the dev knob.
	require.Equal(t, params.Counts.ImportCandidatesPerSource, res.UnmatchedGContacts, "dev profile seeds gcontacts candidates")
	require.Equal(t, params.Counts.ImportCandidatesPerSource, res.UnmatchedGmailCorrespondence, "dev profile seeds gmail_correspondence candidates")
	require.Equal(t, params.Counts.ImportCandidatesPerSource, res.UnmatchedAnarlogHumans, "dev profile seeds anarlog_humans candidates")
	require.Equal(t, params.Counts.ImportCandidatesPerSource, res.TelegramDiscovery, "dev profile seeds telegram discovery candidates")

	remaining, err := h.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Greater(t, remaining, int64(0), "dev profile seeds catalog contacts")

	// Post-seed coherence gate (F1/F3 + tour capacity), scoped to the namespace.
	support := repository.NewSyntheticSupportRepository(database.Queries)
	assertSeedCoherence(t, ctx, support, h.Generator().Prefix(), h.DateFactContactID())
	// Clock-anchored birthday fixtures (CON-052), subject-scoped to the reserved ids.
	assertBirthdayFixtures(t, ctx, support, h, res, h.Generator().Anchor(), params.Counts.SeededContacts)
	// Two-sided message-direction coverage (F4).
	assertMessageDirectionCoverage(t, ctx, support, h, res)
	// Notes coverage (F6).
	assertNotesCoverage(t, ctx, support, h, res, params.Counts.SeededContacts)
	// Pinned tour fixtures: every state the QA tours depend on resolves to exactly
	// one named contact carrying that state.
	assertPinnedTourFixtures(t, ctx, support, repository.NewContactRepository(database.Queries), h, h.Generator().Prefix(), h.Generator().Anchor())
	// Archetype cohorts: the catalog's interaction state now EMERGES from replayed
	// source payloads, so assert the cohorts exist, vary, and collapse as intended.
	assertArchetypeCohorts(t, ctx, h, res, params.Counts.SeededContacts)
	// Dev seeds at its natural knobs, so its non-catalog cohort is the shipped one
	// and this call PINS synthetic.DevNonCatalogLiveContacts against the database.
	assertOverdueBand(t, ctx, support, h, params.Counts.SeededContacts, synthetic.DevNonCatalogLiveContacts)
}

// assertNotesCoverage runs the F6 notes gate: the profile seeds a note on a
// deterministic fraction (every third catalog contact by index, ⌈n/3⌉ total), so the
// result count must equal ⌈seededContacts/3⌉ AND the notepad notes must actually be
// present in the DB for that many contacts (namespace-scoped). Tying the expectation to
// the catalog size — not a floor — catches a constant-count regression (e.g. "always
// seed 2") that would leave a prod-shaped catalog near-empty. Called by both coverage
// tests.
func assertNotesCoverage(t *testing.T, ctx context.Context, support *repository.SyntheticSupportRepository, h *synthetic.Harness, res synthetic.ProfileResult, seededContacts int) {
	t.Helper()
	expectedNotes := (seededContacts + 2) / 3 // ⌈seededContacts/3⌉ (the i%3==0 subset)
	require.Equal(t, expectedNotes, res.ContactsWithNotes, "notes seeded on ⌈catalog/3⌉ contacts")
	notepadContacts, err := support.ContactsWithNotepadCountByNamePrefix(ctx, h.Generator().Prefix())
	require.NoError(t, err)
	require.Equal(t, int64(res.ContactsWithNotes), notepadContacts, "non-empty notepad notes present in the DB for exactly the seeded contacts")
}

// TestSyntheticProfile_ProdShapedCoverageCheck is the load-bearing coverage
// assertion for #380: a prod-shaped seed (bounded counts) must populate every
// PRODUCIBLE load-bearing UI bucket, and must leave the three DEFERRED
// (no-toolkit-producer) pending states absent-by-design so the gap is documented
// rather than silently claimed.
func TestSyntheticProfile_ProdShapedCoverageCheck(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	params, err := synthetic.ProfileParams(synthetic.ProfileProdShaped)
	require.NoError(t, err)
	// Captured BEFORE the CI bounding below; the live-population expectation at the
	// assertOverdueBand call is derived from the difference.
	shippedSeededMerged := params.Counts.SeededMerged
	params.Namespace = syntheticNS(t)
	// Bound the prod-shaped volume for CI: the orchestration is the same at a
	// smaller scale, and the coverage buckets each still get ≥1 representative.
	params.Counts.SeededContacts = 9 // ≥3 so the unicode/descender catalog slots fill
	params.Counts.UnmatchedExternal = 2
	params.Counts.StrandedTelegram = 1
	params.Counts.StrandedMessages = 1
	params.Counts.UnmatchedCalendar = 1
	params.Counts.OrphanMeetingNote = 1
	// 5 assertions walk the full text-fact predicate cycle once (CI-safe), so every
	// predicate — incl. the always-confirm ones (health_condition, occurrence →
	// proposed) and the accepted ones (home_address/job_title/preference) — is
	// exercised.
	params.Counts.SeededAssertions = 5
	// One full bool-fact cycle (job_seeking/on_sabbatical/traveling) + one full
	// edge cycle (knows/introduced_by/sibling_of) so every new predicate — incl.
	// the always-confirm sibling_of (→ proposed) and the auto-if-confident rest
	// (→ accepted) — is exercised. (Birthday date-facts are seeded independently,
	// gated only on birthdayless catalog slots.)
	params.Counts.SeededBoolFacts = 3
	params.Counts.SeededRelationships = 3
	// Enable cadence-task seeding (>0 gate). The 9-contact catalog is all
	// cadence-bearing, so reconcile creates 9 `managed` tasks and one is driven to
	// `unmanaged` via the real recurring-edit path — the two states cadence_due can
	// hold in prod (completed/dismissed are unreachable for this lifecycle).
	params.Counts.SeededTasks = 1
	// Entity pool of 3 (1 org + 1 topic + 1 tag) + one full person→entity edge cycle
	// (works_at/interested_in/tagged_as) so every entity subtype + edge type is
	// exercised. lives_in rides WithLocation (index-spread, present at this catalog
	// size) — not a count knob.
	params.Counts.SeededEntities = 3
	params.Counts.SeededEntityEdges = 3
	// A few relationship_signal rows (SP1 derived storage) across distinct catalog
	// person nodes, one full signal-key cycle (closeness/real_cadence_days/trend).
	params.Counts.SeededSignals = 3
	// Two settled interactions per dedicated source contact (the CI-safe minimum
	// that still exercises the temporal spread): the second message is one
	// contact-indexed spread gap (>= 9 days) older, so the per-contact span clears the
	// multi-day floor asserted below. Kept small to bound the settle budget.
	params.Counts.MessagesPerContact = 2
	// One soft-deleted contact + one merged pair (the CI-safe minimum): enough to
	// assert the tombstone + assertion re-point invariants below.
	params.Counts.SeededSoftDeleted = 1
	params.Counts.SeededMerged = 1
	// One candidate per Imports subtab (gcontacts/gmail_correspondence/anarlog_humans/
	// telegram-discovery): the CI-safe minimum that still gives each subtab ≥1 queue
	// entry. Kept at 1 to bound the settle budget (telegram discovery replays 3 group
	// messages + settles; anarlog ingests + settles).
	params.Counts.ImportCandidatesPerSource = 1

	h := synthetic.NewHarnessForNamespace(t, ctx, database, params.Namespace, params.Seed)
	res, err := synthetic.RunProfile(ctx, h, params)
	require.NoError(t, err)

	// PRODUCIBLE buckets all have ≥1 representative.
	require.GreaterOrEqual(t, res.Contacts, params.Counts.SeededContacts, "catalog contacts seeded")
	require.GreaterOrEqual(t, res.GmailSettled, 1, "gmail-settled present")
	require.GreaterOrEqual(t, res.TelegramSettled, 1, "telegram-settled present")
	require.GreaterOrEqual(t, res.GCalSettled, 1, "gcal-settled present")
	require.GreaterOrEqual(t, res.GChatSettled, 1, "gchat-settled present")
	require.GreaterOrEqual(t, res.IMessageSettled, 1, "imessage-settled present")
	require.GreaterOrEqual(t, res.UnmatchedExternal, 1, "unmatched external (Imports queue) present")
	require.GreaterOrEqual(t, res.StrandedTelegram, 1, "stranded telegram present")
	require.GreaterOrEqual(t, res.StrandedMessages, 1, "stranded iMessage present")
	require.GreaterOrEqual(t, res.UnmatchedCalendar, 1, "unmatched calendar attendee present")
	require.GreaterOrEqual(t, res.OrphanMeetingNote, 1, "orphan meeting-note present")

	// DEFERRED pending states (no toolkit producer yet): the ProfileResult has
	// no field for them precisely because RunProfile produces none. This guards
	// the documented gap — if a future change starts producing them it must add a
	// counter, which forces revisiting this test. Asserting the producible set is
	// complete (every res.* counter > 0) is the absent-by-design check: there is
	// no conflict-meeting-note / title-candidate / gchat-pending counter to assert
	// because none is produced.

	// DB-LEVEL cadence/no-method bucket check: the catalog contacts must NOT be
	// clobbered by a settling replay (a MatchSeeded inbound would let the cadence
	// updater overwrite last_contacted). Scoped to the namespace prefix.
	support := repository.NewSyntheticSupportRepository(database.Queries)
	buckets, err := support.ListContactBucketsByNamePrefix(ctx, h.Generator().Prefix())
	require.NoError(t, err)

	// The backdated cohort is created ~90 days ago and carries NO last_contacted — it
	// has never been connected with (CON-001). A settling replay must not clobber that
	// into a connection (a MatchSeeded inbound letting the cadence updater write
	// last_contacted), so identify the cohort by its far-past created_at and require it
	// intact. The 14-day-ago floor distinguishes it from the recent-creation bucket
	// (<48h ago), so a clobber regression (the bug this guards) would FAIL here.
	// Post-change, every never-connected contact carries NULL last_contacted regardless
	// of age, so "backdated" and "never-contacted" are no longer disjoint by
	// last_contacted (they were, pre-change) — split them by created_at instead so each
	// assertion proves a DISTINCT cohort and neither is satisfied vacuously by the other:
	// backdated = far-past created_at (the overdue cohort), recent = created after the
	// 14d floor (the recent cohort, which gets no settling replay so its NULL survives).
	var backdatedIntact, recentNeverConnected, noMethod int
	now := accelerated.GetCurrentTime()
	overdueFloor := now.Add(-14 * 24 * time.Hour)
	for _, b := range buckets {
		if b.MethodCount == 0 {
			noMethod++
		}
		if b.Cadence != nil && *b.Cadence != "" && b.LastContacted == nil && b.CreatedAt != nil {
			if b.CreatedAt.Before(overdueFloor) {
				backdatedIntact++
			} else {
				recentNeverConnected++
			}
		}
	}
	require.GreaterOrEqual(t, backdatedIntact, 1, "≥1 cadence-bearing backdated never-connected contact un-clobbered (far-past created_at, NULL last_contacted)")
	require.GreaterOrEqual(t, recentNeverConnected, 1, "≥1 cadence-bearing recent never-connected contact (created after the 14d floor, NULL last_contacted) — a cohort distinct from the backdated one")
	require.GreaterOrEqual(t, noMethod, 1, "≥1 no-method contact (the no-method bucket exists)")

	// Overdue-cohort DIVERSITY (DSH-010): the overdue surface must show a RANGE of
	// days-overdue and cadences, not a single monthly / ~60-day monoculture the
	// dashboard urgency tiers cannot separate. Select the backdated cohort
	// STRUCTURALLY — cadence set, last_contacted NULL (never connected, CON-001),
	// created_at older than a fixed floor (now − 7d, which deterministically
	// excludes the <48h recent cohort in EVERY env) — so this does NOT depend on the
	// env-reading cadence helper, which collapses days-overdue under compressed test
	// durations. Assert ≥3 distinct created-ages AND ≥2 distinct cadences; this is the
	// assertion that would have caught the pre-change monoculture (all overdue slots
	// were monthly + 90d), and it is env-independent by construction.
	diversityFloor := now.Add(-7 * 24 * time.Hour)
	distinctCreatedAges := map[int64]bool{}
	distinctOverdueCadences := map[string]bool{}
	for _, b := range buckets {
		if b.Cadence == nil || *b.Cadence == "" || b.CreatedAt == nil || b.LastContacted != nil {
			continue
		}
		if !b.CreatedAt.Before(diversityFloor) {
			continue
		}
		distinctCreatedAges[b.CreatedAt.UnixNano()] = true
		distinctOverdueCadences[*b.Cadence] = true
	}
	require.GreaterOrEqual(t, len(distinctCreatedAges), 3, "overdue cohort spans ≥3 distinct created-ages (days-overdue diversity, not a monoculture)")
	require.GreaterOrEqual(t, len(distinctOverdueCadences), 2, "overdue cohort spans ≥2 distinct cadences")

	// Post-seed coherence gate (F1/F3 + tour capacity), scoped to the namespace.
	assertSeedCoherence(t, ctx, support, h.Generator().Prefix(), h.DateFactContactID())
	// Clock-anchored birthday fixtures (CON-052), subject-scoped to the reserved ids.
	assertBirthdayFixtures(t, ctx, support, h, res, h.Generator().Anchor(), params.Counts.SeededContacts)
	// Two-sided message-direction coverage (F4).
	assertMessageDirectionCoverage(t, ctx, support, h, res)
	// Notes coverage (F6).
	assertNotesCoverage(t, ctx, support, h, res, params.Counts.SeededContacts)
	// Pinned tour fixtures: every state the QA tours depend on resolves to exactly
	// one named contact carrying that state.
	assertPinnedTourFixtures(t, ctx, support, repository.NewContactRepository(database.Queries), h, h.Generator().Prefix(), h.Generator().Anchor())

	// Bio facts (how_met, location, real-year birthdays) ride on the contact-create
	// authority flip; the catalog carries ≥1 of each. A location additionally mints
	// a place entity node + a lives_in edge (asserted below).
	require.GreaterOrEqual(t, res.ContactsWithBirthday, 1, "≥1 contact with a birthday bio fact")
	require.GreaterOrEqual(t, res.ContactsWithHowMet, 1, "≥1 contact with a how_met bio fact")
	require.GreaterOrEqual(t, res.ContactsWithLocation, 1, "≥1 contact with a location bio fact")

	// Accept/pending coverage (D6) + full text-fact spread: assert the SPECIFIC
	// (predicate, review-policy) outcome for EVERY predicate in the Phase-4 cycle,
	// not merely that some assertion of each status exists — so a predicate
	// silently dropping out of the spread (e.g. occurrence) or no longer landing
	// its expected status is caught. health_condition + occurrence (always-confirm)
	// land PROPOSED (pending-review surface); home_address / job_title / preference
	// (auto-if-confident) land ACCEPTED (accepted-knowledge surface). Scoped to the
	// namespace.
	assertions, err := support.ListAssertionsByNodePrefix(ctx, h.Generator().Prefix())
	require.NoError(t, err)
	seen := make(map[string]bool) // "<predicate>/<status>"
	var realYearBirthday bool
	var boolFactTrue bool
	for _, a := range assertions {
		seen[a.PredicateKey+"/"+a.Status] = true
		// Positive guard: EnsurePlaceTx mints FLAT place nodes (no synonym /
		// hierarchy resolution), so the seed must produce ZERO place→place `within`
		// edges. A surviving `within` would FK-block the entity-node teardown sweep;
		// catching it here makes the regression loud at seed time. (A `within`
		// subject is an ns-prefixed place node, so it would appear in this list.)
		require.NotEqual(t, "within", a.PredicateKey, "no place→place within hierarchy assertion (flat place nodes)")
		// item 5: a real-year birthday from the spread must exist, not just the
		// 1900 month/day-only sentinel (which alone would satisfy
		// ContactsWithBirthday even if the real-year spread regressed).
		if a.PredicateKey == "birthday" && a.ValueDate != nil && !strings.HasPrefix(*a.ValueDate, "1900-") {
			realYearBirthday = true
		}
		// A bool fact must carry value_bool (proves the ValueBool plumbing routes
		// the scalar, not just the predicate).
		if a.ValueBool != nil && *a.ValueBool {
			boolFactTrue = true
		}
	}
	for _, want := range []string{
		"health_condition/" + repository.AssertionStatusProposed,
		"occurrence/" + repository.AssertionStatusProposed,
		"home_address/" + repository.AssertionStatusAccepted,
		"job_title/" + repository.AssertionStatusAccepted,
		"preference/" + repository.AssertionStatusAccepted,
		// bool facts — all auto-if-confident → accepted.
		"job_seeking/" + repository.AssertionStatusAccepted,
		"on_sabbatical/" + repository.AssertionStatusAccepted,
		"traveling/" + repository.AssertionStatusAccepted,
		// person→person edges — knows/introduced_by accepted, sibling_of
		// (always-confirm) proposed, so BOTH edge review surfaces are present.
		"knows/" + repository.AssertionStatusAccepted,
		"introduced_by/" + repository.AssertionStatusAccepted,
		"sibling_of/" + repository.AssertionStatusProposed,
		// person→entity edges — all auto-if-confident → accepted. lives_in rides the
		// contact-create authority flip (WithLocation); works_at/interested_in/
		// tagged_as ride the entity pool + edge spread.
		"lives_in/" + repository.AssertionStatusAccepted,
		"works_at/" + repository.AssertionStatusAccepted,
		"interested_in/" + repository.AssertionStatusAccepted,
		"tagged_as/" + repository.AssertionStatusAccepted,
	} {
		require.True(t, seen[want], "graph value-type/edge spread must land %s", want)
	}
	require.True(t, realYearBirthday, "≥1 real-year (non-1900-sentinel) birthday from the spread")
	require.True(t, boolFactTrue, "≥1 bool fact carrying value_bool=true")

	// Value-type + edge result counts (bool facts + the toolkit date fact +
	// person→person edges). The CI override seeds one full cycle of each.
	require.GreaterOrEqual(t, res.SeededBoolFacts, 1, "bool facts seeded")
	require.GreaterOrEqual(t, res.SeededRelationships, 1, "person→person edges seeded")
	require.GreaterOrEqual(t, res.SeededDateFacts, 1, "toolkit-authored date fact seeded")

	// Entity layer: the org/topic/tag pool + the place nodes from WithLocation must
	// all be present, and the person→entity edges seeded. Scoped to the namespace via
	// the node label prefix.
	require.GreaterOrEqual(t, res.SeededEntities, 1, "entity pool seeded")
	require.GreaterOrEqual(t, res.SeededEntityEdges, 1, "person→entity edges seeded")
	prefix := h.Generator().Prefix()
	entityNames, err := support.ListEntityNamesByNodePrefix(ctx, prefix)
	require.NoError(t, err)
	subtypesSeen := make(map[string]bool)
	for _, e := range entityNames {
		subtypesSeen[e.Subtype] = true
	}
	for _, subtype := range []string{
		repository.EntitySubtypePlace, // from WithLocation (contact-create authority flip)
		repository.EntitySubtypeOrganization,
		repository.EntitySubtypeTopic,
		repository.EntitySubtypeTag,
	} {
		require.True(t, subtypesSeen[subtype], "≥1 %q entity node present", subtype)
	}

	// Positive guard: tags are modeled as `tagged_as` graph edges (asserted above),
	// NOT legacy contact_tag rows — the legacy table MUST stay zero for the
	// namespace, so a future reader does not "fix" a perceived gap by re-seeding it.
	legacyContactTags, err := support.CountContactTagsByContactNamePrefix(ctx, prefix)
	require.NoError(t, err)
	require.Equal(t, int64(0), legacyContactTags, "tags seeded as tagged_as edges, not legacy contact_tag rows")

	// relationship_signal (SP1 derived storage): the seed writes per-node scalar
	// signals on a subset of catalog person nodes through the production upsert path.
	// Assert the result count and that the rows actually landed in the DB (scoped to
	// THIS run's tracked signal nodes). The harness teardown (t.Cleanup) deletes them
	// before the node deletes; the explicit signals-remaining==0 check lives in the
	// determinism test, which tears down inline.
	require.GreaterOrEqual(t, res.SeededSignals, 1, "relationship signals seeded")
	signalsLanded, err := h.SignalsRemaining(ctx)
	require.NoError(t, err)
	require.GreaterOrEqual(t, signalsLanded, int64(1), "≥1 relationship_signal row present for the seeded nodes")

	// Cadence tasks: ReplayTodoist seeds a `managed` cadence_due task on each
	// cadence-bearing catalog contact, and ReplayTodoistRecurringEdit drives one to
	// `unmanaged` through the real recurring-edit path. cadence_due has exactly two
	// prod-reachable persistent states — `managed` and `unmanaged` — so assert BOTH
	// are present (namespace-scoped) and that NO cadence_due row is in a prod-
	// impossible shape (gate e): `completed`/`dismissed` states (a completed row is
	// deleted by the next reconcile; a skip routes to a managed replacement, never to
	// dismissed) or a non-finalized external id (a raw UUID / lingering
	// pending_temp_id instead of a Todoist-v1 alphanumeric).
	require.GreaterOrEqual(t, res.SeededTasks, 1, "cadence tasks seeded")
	managedCadence, err := support.CountCadenceDueByStateByNamePrefix(ctx, string(repository.ContactTaskStateManaged), prefix)
	require.NoError(t, err)
	require.GreaterOrEqual(t, managedCadence, int64(1), "≥1 managed cadence_due task (reconcile default)")
	unmanagedCadence, err := support.CountCadenceDueByStateByNamePrefix(ctx, string(repository.ContactTaskStateUnmanaged), prefix)
	require.NoError(t, err)
	require.GreaterOrEqual(t, unmanagedCadence, int64(1), "≥1 unmanaged cadence_due task (real recurring-edit path)")
	incoherentCadence, err := support.IncoherentCadenceDueCountByNamePrefix(ctx, prefix)
	require.NoError(t, err)
	require.Equal(t, int64(0), incoherentCadence, "no cadence_due task may be completed/dismissed or carry a non-finalized external id")

	// The "awaiting reply" state (has_pending_followup). REGRESSION GUARD, not a nicety:
	// a seeded world cannot reach this state through the production path (FollowUpManager
	// is off-mode in the harness; CAD-012 suppresses follow-ups for backdated automated
	// outbounds), so before it was seeded explicitly the state was ABSENT from every
	// seeded world. The tours therefore could not capture it, and the agentic judge —
	// shown only contact pages with no "Awaiting reply" marker — concluded the feature did
	// not exist and emitted a confident, well-cited, FALSE CAD-036 regression on every
	// run. Absence of evidence read as absence of the feature. If this assertion ever goes
	// back to zero, that false verdict returns.
	require.Equal(t, 1, res.SeededPendingFollowUps, "profile seeds exactly one live follow-up")
	liveFollowUps, err := support.CountLiveFollowUpsByNamePrefix(ctx, prefix)
	require.NoError(t, err)
	// The query requires last_outreach_at IS NOT NULL — a follow-up is opened BY an outbound
	// (CAD-011), so one on a contact with no outbound is a state production cannot reach.
	// The first version of this seed produced exactly that incoherent world, and the judge
	// (correctly) failed the contact page for showing "Awaiting reply" with nothing to be
	// awaiting a reply to. Asserting COHERENCE, not just presence, is the point.
	require.GreaterOrEqual(t, liveFollowUps, int64(1),
		"≥1 COHERENT live followup_loop task (on a contact with an outbound) — the has_pending_followup state the contact page renders")

	// Product-visible task spread: 0/1/multiple manual tasks across catalog cohorts,
	// varied kind/lifecycle/link-age, exact accounting (cadence + manual + follow-up).
	assertVisibleTaskSpread(t, ctx, support, h, res, prefix)

	// Interaction volume: each dedicated source contact carries MessagesPerContact
	// settled interactions, so SettledInteractions == (settled contacts) ×
	// MessagesPerContact — PLUS the single GCal event the awaiting-reply scenario
	// replays (one per seeded follow-up), which is a real settled interaction and is
	// counted as one. It is the OUTBOUND half of that scenario's causal chain: mutual
	// direction (CAD-006) is what gives the contact the last_outreach_at a follow-up
	// requires (CAD-011), so it is not bookkeeping — it is the reason the state is
	// coherent at all.
	settledContacts := res.GmailSettled + res.TelegramSettled + res.GCalSettled + res.GChatSettled + res.IMessageSettled
	require.Equal(t, settledContacts*params.Counts.MessagesPerContact+res.SeededPendingFollowUps, res.SettledInteractions,
		"MessagesPerContact interactions per settled contact, plus the awaiting-reply scenario's GCal event")
	require.Greater(t, res.SettledInteractions, settledContacts, ">1 interaction per settled contact")

	// Interaction temporal spread (DB-level): ≥1 settled contact must carry ≥2
	// interactions whose earliest→latest span clears a multi-day floor — proving the
	// per-message age offsets landed them on distinct days (a prod-like history),
	// not a single ~1h window. Scoped to the namespace; the interaction-free
	// edge-case catalog contacts are excluded by the query's INNER JOIN.
	spreads, err := support.ListInteractionSpreadByContactNamePrefix(ctx, prefix)
	require.NoError(t, err)
	var spread bool
	for _, s := range spreads {
		if s.InteractionCount >= 2 && s.Span >= 7*24*time.Hour {
			spread = true
			break
		}
	}
	require.True(t, spread, "≥1 settled contact has ≥2 interactions spanning ≥7 days (temporal spread, not one window)")

	// Spacing NON-UNIFORMITY (F9): the settled history must not be one uniform gap
	// repeated across every contact (the pre-change shape was a fixed 21-day
	// interval). The spread ladder is contact-indexed, so even at
	// MessagesPerContact=2 (a single gap) different settled contacts carry different
	// spans. Assert ≥2 distinct span values across settled contacts with ≥2
	// interactions — the assertion that would have caught the uniform-spacing shape.
	distinctSpans := map[time.Duration]bool{}
	for _, s := range spreads {
		if s.InteractionCount >= 2 {
			distinctSpans[s.Span] = true
		}
	}
	require.GreaterOrEqual(t, len(distinctSpans), 2, "≥2 distinct interaction spans across settled contacts (non-uniform spacing, not a single fixed gap)")

	// Merge + soft-delete scenarios (item 12). These are standalone contacts seeded
	// at full count (independent of the catalog), so the result equals the override.
	require.Equal(t, params.Counts.SeededSoftDeleted, res.SeededSoftDeleted, "soft-deleted contacts seeded")
	require.Equal(t, params.Counts.SeededMerged, res.SeededMerged, "merged contact pairs seeded")

	// Soft-delete invariant: the person node is tombstoned, so its assertion is
	// RETAINED in the table but DROPS from live graph reads. Scoped to THIS run's
	// tracked soft-delete node ids.
	softNodeIDs := h.SoftDeletedNodeIDs()
	require.Len(t, softNodeIDs, res.SeededSoftDeleted, "tracked soft-delete node ids match the result count")
	softInTable, err := support.CountAssertionsForSubjects(ctx, softNodeIDs)
	require.NoError(t, err)
	require.GreaterOrEqual(t, softInTable, int64(len(softNodeIDs)), "soft-deleted assertions retained in the table")
	softLive, err := support.CountLiveAssertionsForSubjects(ctx, softNodeIDs)
	require.NoError(t, err)
	require.Zero(t, softLive, "soft-deleted assertions dropped from live graph reads")
	softLiveNodes, err := support.CountLiveNodesByIds(ctx, softNodeIDs)
	require.NoError(t, err)
	require.Zero(t, softLiveNodes, "soft-deleted person nodes tombstoned (not live)")

	// Merge invariant: the loser node is tombstoned with merged_into set, its
	// assertions are re-pointed OFF the loser onto the live winner, and the winner
	// stays live carrying its own + the re-pointed assertions. Scoped to THIS run's
	// tracked merge node ids.
	loserIDs := h.MergedLoserNodeIDs()
	winnerIDs := h.MergedWinnerNodeIDs()
	require.Len(t, loserIDs, res.SeededMerged, "tracked merge-loser node ids match the pair count")
	require.Len(t, winnerIDs, res.SeededMerged, "tracked merge-winner node ids match the pair count")
	loserMerged, err := support.CountMergedIntoNodesByIds(ctx, loserIDs)
	require.NoError(t, err)
	require.Equal(t, int64(len(loserIDs)), loserMerged, "every merge-loser node carries merged_into")
	loserLiveNodes, err := support.CountLiveNodesByIds(ctx, loserIDs)
	require.NoError(t, err)
	require.Zero(t, loserLiveNodes, "merge-loser nodes tombstoned (not live)")
	loserAssertions, err := support.CountAssertionsForSubjects(ctx, loserIDs)
	require.NoError(t, err)
	require.Zero(t, loserAssertions, "merge-loser assertions re-pointed off the loser")
	winnerLiveNodes, err := support.CountLiveNodesByIds(ctx, winnerIDs)
	require.NoError(t, err)
	require.Equal(t, int64(len(winnerIDs)), winnerLiveNodes, "merge-winner nodes stay live")
	winnerLiveAssertions, err := support.CountLiveAssertionsForSubjects(ctx, winnerIDs)
	require.NoError(t, err)
	require.GreaterOrEqual(t, winnerLiveAssertions, int64(2*len(winnerIDs)),
		"merge-winner carries its own + the re-pointed loser assertion (≥2 per pair)")

	// Import-source candidates (item 13): EVERY Imports subtab must have ≥1 unmatched
	// candidate so staging's Imports queue is populated. Assert both the result counter
	// and the actual DB row, scoped to THIS namespace. gcontacts/gmail_correspondence/
	// anarlog_humans carry an ns-prefixed source_id; telegram discovery keys on the bare
	// peer id, so it is scoped by the namespace's reserved peer band instead.
	require.GreaterOrEqual(t, res.UnmatchedGContacts, 1, "gcontacts candidate seeded")
	require.GreaterOrEqual(t, res.UnmatchedGmailCorrespondence, 1, "gmail_correspondence candidate seeded")
	require.GreaterOrEqual(t, res.UnmatchedAnarlogHumans, 1, "anarlog_humans candidate seeded")
	require.GreaterOrEqual(t, res.TelegramDiscovery, 1, "telegram discovery candidate seeded")
	for _, source := range []string{"gcontacts", "gmail_correspondence", "anarlog_humans"} {
		count, err := support.CountUnmatchedExternalContactBySourceAndPrefix(ctx, source, prefix)
		require.NoError(t, err)
		require.GreaterOrEqual(t, count, int64(1), "≥1 unmatched %q import candidate present", source)
	}
	tgDiscovery, err := support.CountUnmatchedTelegramDiscoveryInBand(ctx, h.Generator().PeerBandStart(), h.Generator().PeerBandEnd())
	require.NoError(t, err)
	require.GreaterOrEqual(t, tgDiscovery, int64(1), "≥1 unmatched telegram discovery candidate present")

	// Archetype cohorts. At this catalog size (9) the ladder is on its RESERVED
	// rung, so per-archetype presence is not asserted; what is asserted is that the
	// assignment the seed applied is the one the pure function derives and that the
	// two counters show the collapse.
	assertArchetypeCohorts(t, ctx, h, res, params.Counts.SeededContacts)
	// This test bounds SeededMerged for CI, which shrinks its non-catalog cohort
	// below the shipped prod-shaped figure: each merge pair creates two contacts and
	// leaves one LIVE (MergeContacts soft-deletes the loser, and the buckets query
	// filters deleted_at IS NULL), so every pair dropped costs one live contact.
	// Derived from the shipped constant and the override rather than restated as a
	// literal, so it tracks either value.
	wantNonCatalogLive := synthetic.ProdShapedNonCatalogLiveContacts - (shippedSeededMerged - params.Counts.SeededMerged)
	assertOverdueBand(t, ctx, support, h, params.Counts.SeededContacts, wantNonCatalogLive)

	// The reserved rung's whole point: exactly one CONTACTED-AND-OVERDUE contact —
	// a state the seed produces nowhere else — and it is the slot the assignment
	// gave the drifting archetype to. Every other overdue contact in the world is
	// never-connected, which is a different state and a different card.
	anchor := h.Generator().Anchor()
	var contactedOverdue []uuid.UUID
	for _, b := range buckets {
		if b.Cadence == nil || *b.Cadence == "" || b.CreatedAt == nil || b.LastContacted == nil {
			continue
		}
		if synthetic.OverdueAtProduction(*b.Cadence, b.LastContacted, *b.CreatedAt, anchor) {
			contactedOverdue = append(contactedOverdue, b.ID)
		}
	}
	require.Len(t, contactedOverdue, 1, "the reserved rung supplies exactly one contacted-and-overdue contact")
	var driftingID uuid.UUID
	for _, sample := range h.ArchetypeSamples() {
		if sample.Archetype == factory.ArchetypeMutualDrifting {
			driftingID = sample.ContactID
		}
	}
	require.Equal(t, driftingID, contactedOverdue[0],
		"the contacted-and-overdue contact must be the slot the assignment gave mutual-drifting to")

	// …and the product has to AGREE that it is overdue. The state exists to put a
	// contacted-and-overdue card on the dashboard, so recomputing overdue-ness is
	// not enough: the same contact must come back from the predicate the overdue
	// endpoint runs against the persisted contact_by column. Recompute-only was
	// exactly how a nine-contact shortfall reached staging unnoticed (gh #751).
	endpointOverdueIDs, err := support.ListOverdueContactIdsByNamePrefix(ctx, prefix, cadence.Today(anchor))
	require.NoError(t, err)
	require.Contains(t, endpointOverdueIDs, driftingID,
		"the reserved rung's contacted-and-overdue contact must also be returned by the overdue ENDPOINT — a contact the product does not list as overdue does not supply the state")

	remaining, err := h.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Greater(t, remaining, int64(0), "prod-shaped profile seeds contacts")
}

// catalogProfilePhaseCount is how many comment-delimited seeding blocks
// runCatalogProfile brackets with a phase timer. It is pinned so a block added
// (or a stop() call lost) without a phase is caught here rather than silently
// dropping a row from the reseed summary.
const catalogProfilePhaseCount = 24

// phaseShape projects phase timings onto their DETERMINISTIC components — name
// and payload volume — dropping the wall-clock duration, so a run-to-run
// comparison asserts the seeding shape without asserting the impossible.
func phaseShape(phases []synthetic.PhaseTiming) []string {
	out := make([]string, 0, len(phases))
	for _, p := range phases {
		out = append(out, fmt.Sprintf("%s=%d", p.Name, p.Payloads))
	}
	return out
}

// TestSyntheticProfile_ProdShapedDeterministic proves the prod-shaped seed is
// deterministic: two runs at the same (namespace, seed, anchor) produce
// byte-identical ProfileResult counts AND an identical fingerprint of the
// generated assertion values. Count-pinning alone is insufficient — counts can
// match while a non-deterministic source (map iteration, wall-clock leak) shifts
// generated values — so the (value_text, value_date, value_bool) fingerprint is
// the drift detector.
//
// Scope (what this catches vs. where): the "seed gen.X() LAST" rule is covered by
// THREE complementary guards. (1) This run-to-run fingerprint comparison detects
// NON-deterministic drift only (map iteration, wall-clock leak); on its own it
// cannot see a DETERMINISTIC mis-ordering (both runs shift identically and still
// match), and a full-VALUE golden is precluded — syntheticNS(t) randomizes the
// namespace per run and the PRNG is seeded with fnv(namespace), so the fingerprinted
// values have no run-stable form to pin. (2) The ordering guard below pins the one
// thing that IS run-stable: the monotonic sourceIDSeq counter (namespace-independent),
// asserting every gen.Entity-pool seq exceeds every text-fact seq — so a deterministic
// mis-ordering of the entity block (the case (1) can't see) fails there. (3) The
// coverage test's Gate-A settle catches a mis-placed gen.X() that shifts later
// source-replay ids into a cross-contact peer-id collision that fails the settle
// loudly (the original SP1 mis-order regression surfaced exactly this way). A
// mis-ordering that shifts NO subsequent source-replay draw is harmless
// (distinct-but-valid synthetic data), so the three guards together cover the
// load-bearing class.
//
// Both runs share the SAME namespace (with a full teardown
// between) so the ns-prefix + PRNG stream line up, AND a PINNED generator anchor
// so the anchor-relative timestamps (birthday date) are byte-identical too —
// without the pin, birthday value_date would legitimately drift between runs
// (anchor-relative per the factory contract) and could not be fingerprinted.
// proposition_key and the edge object node id are NOT fingerprinted (they embed
// the per-run subject/object UUID, so they are not run-stable); a person→person
// edge therefore contributes only its (predicate_key, status) to the fingerprint.
func TestSyntheticProfile_ProdShapedDeterministic(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	params, err := synthetic.ProfileParams(synthetic.ProfileProdShaped)
	require.NoError(t, err)
	params.Namespace = syntheticNS(t)
	// Bound the volume for CI: determinism is a property of the orchestration, not
	// of the scale. 5 assertions walk the full predicate cycle.
	params.Counts.SeededContacts = 5
	params.Counts.SeededAssertions = 5
	// One full bool-fact + edge cycle each (bounded, deterministic at this scale).
	params.Counts.SeededBoolFacts = 3
	params.Counts.SeededRelationships = 3
	// Entity pool (1 org + 1 topic + 1 tag) + one full person→entity edge cycle, so
	// the entity normalized_name fingerprint covers org/topic/tag/place subtypes.
	params.Counts.SeededEntities = 3
	params.Counts.SeededEntityEdges = 3
	// A few relationship_signal rows so the run-to-run ProfileResult count
	// (res.SeededSignals) is exercised and the teardown sweep is asserted below.
	params.Counts.SeededSignals = 3
	// One settled message per source contact: the temporal-spread timestamps are a
	// pure function of the (pinned) anchor + message index, so they are
	// deterministic by construction and not fingerprinted here (interactions are not
	// in the fingerprint); keeping this at 1 holds the settle load at baseline while
	// res.SettledInteractions still participates in the run-to-run count equality.
	params.Counts.MessagesPerContact = 1
	params.Counts.UnmatchedExternal = 1
	params.Counts.StrandedTelegram = 1
	params.Counts.StrandedMessages = 1
	params.Counts.UnmatchedCalendar = 1
	params.Counts.OrphanMeetingNote = 1
	// One soft-deleted contact + one merged pair so the run-to-run ProfileResult
	// count is exercised and the teardown of the tombstoned + merged rows (the
	// merged_into self-FK is the trickiest sweep) is asserted FK-clean below.
	params.Counts.SeededSoftDeleted = 1
	params.Counts.SeededMerged = 1
	// One candidate per Imports subtab so the import-producer source_ids participate in
	// the determinism fingerprint (the direct-upsert + ingest source_ids are ns-prefixed)
	// and the run-to-run ProfileResult count equality, and the teardown of the
	// external_contact candidates is asserted clean below.
	params.Counts.ImportCandidatesPerSource = 1

	// Capture the generator anchor ONCE and reuse it for both runs: identical
	// across runs (so timestamped output incl. birthday value_date is
	// byte-identical and the fingerprint covers it) yet ≈now so the settle
	// pipeline — which runs on the real system clock — is not skewed by a far-past
	// anchor (a far-past anchor starves Gate A: the consumers never link the
	// stale-dated messages within the real-time budget).
	anchor := accelerated.GetCurrentTime()
	support := repository.NewSyntheticSupportRepository(database.Queries)

	// run seeds the profile, captures (ProfileResult, assertion fingerprint, entity
	// normalized_name fingerprint, import-candidate source_id fingerprint), then fully
	// tears down so the next run starts from a clean namespace. It also asserts the
	// teardown swept every entity node (pool + place) — a leaked entity node would
	// FK-block or duplicate-violate the next run, and a surviving place node from a
	// `within` edge would FK-block the sweep — and every import-candidate row.
	run := func() (synthetic.ProfileResult, []repository.AssertionSummary, []repository.EntityNameSummary, []repository.ExternalContactSummary, []string, []string) {
		h, teardown, err := synthetic.NewHarnessWithDBForNamespaceAt(ctx, database, params.Namespace, params.Seed, anchor)
		require.NoError(t, err)
		res, err := synthetic.RunProfile(ctx, h, params)
		require.NoError(t, err)
		prefix := h.Generator().Prefix()
		fp, err := support.ListAssertionsByNodePrefix(ctx, prefix)
		require.NoError(t, err)
		entFP, err := support.ListEntityNamesByNodePrefix(ctx, prefix)
		require.NoError(t, err)
		// Import-producer fingerprint: the ns-prefixed external_contact source_ids
		// (gcontacts/gmail_correspondence/anarlog_humans + the existing icloud/gcal
		// candidates). The telegram discovery candidate keys on the bare peer id (not
		// the prefix), so it is excluded here; its determinism rides the deterministic
		// peer band + the run-to-run ProfileResult count equality.
		extFP, err := support.ListExternalContactSourceIDsByPrefix(ctx, prefix)
		require.NoError(t, err)
		// Visible-task fingerprint: keyed by stable identity (full_name) + attributes +
		// link-age bucket over every seeded task, so a random-UUID-keyed cohort reshuffle
		// (which the name stays stable through) is caught. Computed off the pinned outer
		// anchor before teardown drops the rows.
		taskRows, err := support.ListTaskRowsByNamePrefix(ctx, prefix)
		require.NoError(t, err)
		taskFP := taskFingerprint(taskRows, anchor)
		// Birthday-fixture fingerprint: keyed by stable identity (full_name) +
		// daysUntil-bucket + age-decade over the reserved fixture subjects, so a
		// random-UUID-keyed allocation or a year-math drift is caught. Computed off the
		// pinned outer anchor before teardown drops the rows.
		bdayRows, err := support.ListContactBirthdayFixturesByIds(ctx, h.BirthdayFixtureIDs())
		require.NoError(t, err)
		bdayFP := birthdayFingerprint(bdayRows, anchor)
		// n=5 degrading allocation is exact + deterministic: the strict {today,+1,distant}
		// triple on the birthdayless catalog creation indices {1,3,4} (index 3 is the
		// no-method slot — a birthday is an assertion, not a contact_method).
		require.Equal(t, 3, res.SeededDateFacts, "n=5 seeds exactly the {today,+1,distant} triple")
		catalog := h.CatalogContactIDs()
		require.GreaterOrEqual(t, len(catalog), 5, "n=5 catalog populated for the allocation check")
		require.Equal(t, []uuid.UUID{catalog[1], catalog[3], catalog[4]}, h.BirthdayFixtureIDs(),
			"birthday fixtures land on catalog creation indices 1,3,4 (today,+1,distant)")
		require.NoError(t, teardown(context.Background()))
		// Teardown correctness: the entity nodes (org/topic/tag pool + place nodes)
		// are swept, so the namespace is clean for the next run.
		remaining, err := support.ListEntityNamesByNodePrefix(ctx, prefix)
		require.NoError(t, err)
		require.Empty(t, remaining, "entity nodes (pool + place) swept by teardown")
		// Teardown correctness: the import-candidate external_contact rows are swept —
		// the ns-prefixed ones (gcontacts/gmail_correspondence/anarlog/icloud/gcal) by
		// the source_id-prefix sweep, the telegram discovery candidate (bare peer id) by
		// the telegram-peer sweep — so none remain for the next run.
		extRemaining, err := support.ListExternalContactSourceIDsByPrefix(ctx, prefix)
		require.NoError(t, err)
		require.Empty(t, extRemaining, "ns-prefixed external_contact candidates swept by teardown")
		tgDiscoveryRemaining, err := support.CountUnmatchedTelegramDiscoveryInBand(ctx, h.Generator().PeerBandStart(), h.Generator().PeerBandEnd())
		require.NoError(t, err)
		require.Zero(t, tgDiscoveryRemaining, "telegram discovery candidate swept by teardown")
		// Teardown correctness: the relationship_signal rows (FK→node, NO ACTION) are
		// swept BEFORE the node deletes, so none remain for the seeded subject nodes —
		// a leaked signal would FK-block the next run's person-node delete.
		sigRemaining, err := h.SignalsRemaining(ctx)
		require.NoError(t, err)
		require.Zero(t, sigRemaining, "relationship_signal rows swept by teardown")
		// Teardown correctness for merge + soft-delete: the require.NoError(teardown)
		// above already proves the merged_into self-FK did not block the
		// single-statement node sweep; this confirms every seeded contact (incl. the
		// tombstoned soft-deleted + merged-loser rows) was hard-deleted, leaving no
		// stranded node/assertion behind.
		contactsRemaining, err := h.ContactsRemaining(ctx)
		require.NoError(t, err)
		require.Zero(t, contactsRemaining, "all seeded contacts (incl. merge/soft-delete) swept by teardown")
		return res, fp, entFP, extFP, taskFP, bdayFP
	}

	res1, fp1, ent1, ext1, task1, bday1 := run()
	res2, fp2, ent2, ext2, task2, bday2 := run()

	// Timings are wall-clock and will never be equal across runs, so they are the
	// one field excluded from the whole-struct equality below. Zeroing the nested
	// struct — rather than comparing field-by-field around it — keeps every other
	// counter, including counters added later, under strict equality.
	require.NotZero(t, res1.Timings.Total, "the run must report a wall-clock duration")
	require.Positive(t, res1.Timings.Settle.Calls, "the run must report settle accounting")
	// Each of the four instrumented settle hunks gets its own witness. Calls alone
	// would stay green if the Gate-B or capture recording were deleted — and those
	// are two of the five numbers the reseed summary prints, so a silent zero would
	// read as "gate B is free" and invert this PR's measured conclusion. waitGateB
	// always polls at least once and captureEventIDs always runs, so both are
	// non-zero on any real run.
	require.Positive(t, res1.Timings.Settle.GateAPolls, "gate A accounting is recorded")
	require.Positive(t, res1.Timings.Settle.GateBPolls, "gate B accounting is recorded")
	require.Positive(t, res1.Timings.Settle.CaptureWait, "capture accounting is recorded")
	require.Empty(t, res1.Timings.Current, "a clean run leaves no phase marked as running")
	require.Len(t, res1.Timings.Phases, catalogProfilePhaseCount,
		"every catalog-profile seeding block reports a phase")
	// Phase NAMES and payload volumes ARE deterministic (durations are not), so
	// they are compared through a duration-free projection.
	require.Equal(t, phaseShape(res1.Timings.Phases), phaseShape(res2.Timings.Phases),
		"phase names + payload volumes must be deterministic across runs")
	res1.Timings = synthetic.SeedTimings{}
	res2.Timings = synthetic.SeedTimings{}

	require.Equal(t, res1, res2, "prod-shaped ProfileResult must be deterministic across runs")
	require.NotEmpty(t, fp1, "the seed must produce assertions to fingerprint")
	require.Equal(t, fp1, fp2, "assertion (value_text, value_date, value_bool) fingerprint must be deterministic across runs")
	require.NotEmpty(t, ent1, "the seed must produce entity nodes to fingerprint")
	require.Equal(t, ent1, ent2, "entity (subtype, normalized_name) fingerprint must be deterministic across runs")
	require.NotEmpty(t, ext1, "the seed must produce import-candidate external_contact rows to fingerprint")
	require.Equal(t, ext1, ext2, "import-candidate (source, source_id) fingerprint must be deterministic across runs")
	require.NotEmpty(t, task1, "the seed must produce contact_task rows to fingerprint")
	require.Equal(t, task1, task2, "visible-task (full_name, kind, lifecycle, state, age-bucket) fingerprint must be deterministic across runs")
	require.NotEmpty(t, bday1, "the seed must produce birthday fixtures to fingerprint")
	require.Equal(t, bday1, bday2, "birthday (full_name, daysUntil-bucket, age-decade) fingerprint must be deterministic across runs")

	// Ordering guard for the "seed gen.X() LAST" rule. The run-to-run
	// comparison above catches NON-deterministic drift but NOT a DETERMINISTIC
	// mis-ordering of the gen.Entity pool (both runs would shift identically). A
	// literal name golden is precluded here — syntheticNS(t) randomizes the namespace
	// per run and the PRNG is seeded fnv(namespace), so the name component is not
	// run-stable. The embedded sourceIDSeq IS run-stable (a pure counter, namespace-
	// independent), so it is the pinnable witness: gen.Entity is seeded LAST, after
	// the Phase-4 text-fact spread, so every gen.Entity-pool seq must exceed every
	// text-fact value seq. A mis-ordered entity block (moved before the spread, or
	// earlier among the source replays) inverts this and fails loudly — the
	// regression class a same-impl two-run comparison cannot see. (place nodes ride
	// WithLocation, not gen.Entity, and carry a stem not a seq, so they are excluded.)
	maxTextFactSeq := -1
	for _, a := range fp1 {
		if a.ValueText == nil {
			continue
		}
		if seq, ok := trailingSeq(*a.ValueText); ok && seq > maxTextFactSeq {
			maxTextFactSeq = seq
		}
	}
	minEntitySeq := -1
	for _, e := range ent1 {
		if e.Subtype == repository.EntitySubtypePlace {
			continue // place names carry a stem, not a seq
		}
		seq, ok := trailingSeq(e.NormalizedName)
		require.True(t, ok, "gen.Entity normalized_name %q must carry a trailing sourceIDSeq", e.NormalizedName)
		if minEntitySeq == -1 || seq < minEntitySeq {
			minEntitySeq = seq
		}
	}
	require.GreaterOrEqual(t, maxTextFactSeq, 0, "the seed must produce text-fact assertions with a seq witness")
	require.GreaterOrEqual(t, minEntitySeq, 0, "the seed must produce gen.Entity pool nodes with a seq witness")
	require.Greater(t, minEntitySeq, maxTextFactSeq,
		"gen.Entity pool must be seeded LAST (after the text-fact spread): entity seq %d must exceed max text-fact seq %d — a smaller entity seq means a mis-ordered gen.X() insertion",
		minEntitySeq, maxTextFactSeq)
}

// TestSyntheticProfile_QuiesceLeavesRows proves the seed-and-leave lifecycle:
// after RunProfile + Quiesce, the namespace's rows STILL EXIST. The test cleans
// its OWN namespace via the harness's namespace-scoped delete (NOT the global
// reset, which would nuke parallel tests on the shared DB).
func TestSyntheticProfile_QuiesceLeavesRows(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	params, err := synthetic.ProfileParams(synthetic.ProfileMinimalScoped)
	require.NoError(t, err)
	params.Namespace = syntheticNS(t)

	// Use the explicit-teardown constructor so we can clean our own namespace
	// after asserting the leave-behind (the *testing.T constructor auto-cleans on
	// t.Cleanup, which would mask the leave-behind assertion).
	h, teardown, err := synthetic.NewHarnessWithDBForNamespace(ctx, database, params.Namespace, params.Seed)
	require.NoError(t, err)

	_, err = synthetic.RunProfile(ctx, h, params)
	require.NoError(t, err)

	// Success path: Quiesce stops the client but LEAVES the rows.
	require.NoError(t, h.Quiesce(ctx))
	remaining, err := h.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Greater(t, remaining, int64(0), "Quiesce must leave the seeded rows standing")

	// Clean our own namespace so the shared DB does not accumulate.
	require.NoError(t, teardown(context.Background()))
	gone, err := h.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), gone, "the namespace's own teardown removes its rows")
}

// TestSyntheticProfile_ErrorPathTearsDown proves the error-path lifecycle: when
// a profile run fails partway, the entrypoint contract is to run the FULL
// teardown (stop client + clean the partial world) — a failed seed is never a
// leave-behind, and the River client is always stopped. This mirrors crm-admin's
// runSeed error branch.
func TestSyntheticProfile_ErrorPathTearsDown(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	params, err := synthetic.ProfileParams(synthetic.ProfileDev)
	require.NoError(t, err)
	params.Namespace = syntheticNS(t)

	h, teardown, err := synthetic.NewHarnessWithDBForNamespace(ctx, database, params.Namespace, params.Seed)
	require.NoError(t, err)

	// Seed a PARTIAL world so there is something to clean, then exercise the
	// teardown contract the orchestration's error branch invokes: a failed seed
	// runs the FULL teardown (stop client + clean the partial namespace).
	gen := h.Generator()
	spec := gen.Contact(factory.WithEmail())
	contact, err := h.SeedContact(ctx, spec)
	require.NoError(t, err)
	_, err = h.ReplayGmail(ctx, contact.ID, gen.GmailMessage(spec, factory.MatchSeeded))
	require.NoError(t, err)

	before, err := h.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Greater(t, before, int64(0))

	// The runSeed error branch: on a forced failure the entrypoint runs the full
	// teardown closure rather than Quiesce.
	require.NoError(t, teardown(context.Background()), "teardown after a forced failure must succeed")

	after, err := h.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), after, "a failed seed must clean its partial world (no leave-behind)")
}

// TestSyntheticProfile_SettlesUnderHighAcceleration guards the real-wall-clock
// settle/teardown budget: the harness uses a harness-local monotonic timer, so a
// high TIME_ACCELERATION — under which "30 accelerated-seconds" would otherwise
// collapse to a sub-second real budget — does NOT cause a spurious settle
// timeout. A minimal-scoped seed still settles and its teardown still clears
// under acceleration 1000.
func TestSyntheticProfile_SettlesUnderHighAcceleration(t *testing.T) {
	testsupport.RequireLongTests(t)
	database, ctx := newSyntheticDB(t)

	// Pin an accelerated clock for this test's process: GetCurrentTime reads
	// TIME_ACCELERATION + TIME_BASE from the process env directly. A FIXED
	// Unix-second base keeps the fixture deterministic (no wall-clock call — the
	// core rule is absolute for tests). The test only asserts that the
	// real-wall-clock settle/teardown budget does not collapse under acceleration;
	// the accelerated domain "now" the base produces is irrelevant to that.
	t.Setenv("TIME_BASE", strconv.FormatInt(fixedAccelBaseUnix, 10))
	t.Setenv("TIME_ACCELERATION", "1000")

	params, err := synthetic.ProfileParams(synthetic.ProfileMinimalScoped)
	require.NoError(t, err)
	params.Namespace = syntheticNS(t)

	// Explicit teardown so the settle+teardown budget is exercised end-to-end.
	h, teardown, err := synthetic.NewHarnessWithDBForNamespace(ctx, database, params.Namespace, params.Seed)
	require.NoError(t, err)

	_, err = synthetic.RunProfile(ctx, h, params)
	require.NoError(t, err, "a minimal-scoped seed must settle even under high acceleration")

	// Teardown (quiesce + Gate-B-gated cleanup) must clear, not spuriously time
	// out on the real-time budget.
	require.NoError(t, teardown(context.Background()))
	gone, err := h.ContactsRemaining(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(0), gone, "teardown must clear under high acceleration")
}
