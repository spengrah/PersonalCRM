package synthetic

import (
	"testing"
	"time"

	"personal-crm/backend/internal/cadence"
	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/synthetic/factory"

	"github.com/stretchr/testify/require"
)

// validCadences is the migration-005 CHECK constraint's cadence allowlist; the
// overdue ladder + the recent / never-contacted rotation pools may only draw from it.
var validCadences = map[string]bool{
	"weekly": true, "biweekly": true, "monthly": true,
	"quarterly": true, "biannual": true, "annual": true,
}

// TestSeedAllowed asserts the prod gate denies the two production aliases and
// allows every other valid CRM_ENV (the validCRMEnvs set in config.go).
func TestSeedAllowed(t *testing.T) {
	cases := []struct {
		env     string
		allowed bool
	}{
		{"production", false},
		{"prod", false},
		{"staging", true},
		{"accelerated", true},
		{"test", true},
		{"testing", true},
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Runtime.CRMEnvironment = tc.env
			err := SeedAllowed(cfg)
			if tc.allowed {
				require.NoError(t, err, "CRM_ENV=%s should be allowed", tc.env)
			} else {
				require.Error(t, err, "CRM_ENV=%s should be denied", tc.env)
			}
		})
	}
}

// TestProfileParams asserts each named profile returns its expected Profile +
// non-zero contact count, and an unknown name (including "") errors.
func TestProfileParams(t *testing.T) {
	for _, name := range []Profile{ProfileMinimalScoped, ProfileDev, ProfileProdShaped} {
		t.Run(string(name), func(t *testing.T) {
			p, err := ProfileParams(name)
			require.NoError(t, err)
			require.Equal(t, name, p.Profile)
			require.Greater(t, p.Counts.SeededContacts, 0, "profile %s should seed at least one contact", name)
			require.NotEmpty(t, p.Namespace)
			require.Equal(t, factory.DefaultSeed, p.Seed)
		})
	}

	t.Run("unknown", func(t *testing.T) {
		_, err := ProfileParams("nope")
		require.Error(t, err)
	})
	t.Run("empty", func(t *testing.T) {
		_, err := ProfileParams("")
		require.Error(t, err)
	})

	// prod-shaped must carry the producible pending-state knobs so the coverage
	// check has something to assert against.
	prod, err := ProfileParams(ProfileProdShaped)
	require.NoError(t, err)
	require.Greater(t, prod.Counts.UnmatchedExternal, 0)
	require.Greater(t, prod.Counts.StrandedTelegram, 0)
	require.Greater(t, prod.Counts.StrandedMessages, 0)
	require.Greater(t, prod.Counts.UnmatchedCalendar, 0)
	require.Greater(t, prod.Counts.OrphanMeetingNote, 0)
	// prod-shaped seeds a prod-shaped slice of text-fact assertions (~1/3 of the
	// catalog); the coverage check overrides this down for CI.
	require.Greater(t, prod.Counts.SeededAssertions, 0)
	// prod-shaped seeds bool facts + person→person edges (the value-type/edge
	// plumbing) so the coverage check has graph rows to assert per-status.
	require.Greater(t, prod.Counts.SeededBoolFacts, 0)
	require.Greater(t, prod.Counts.SeededRelationships, 0)
	// prod-shaped enables cadence-task seeding (>0 gate) so the coverage check has
	// contact_task rows to assert per-state.
	require.Greater(t, prod.Counts.SeededTasks, 0)
	// prod-shaped seeds an entity pool + person→entity edges so the coverage check
	// has org/topic/tag entity nodes + works_at/interested_in/tagged_as edges.
	require.Greater(t, prod.Counts.SeededEntities, 0)
	require.Greater(t, prod.Counts.SeededEntityEdges, 0)
	// prod-shaped seeds relationship_signal rows so the coverage check has SP1
	// derived-storage signals to assert.
	require.Greater(t, prod.Counts.SeededSignals, 0)
	// prod-shaped seeds MULTIPLE settled interactions per dedicated source contact
	// (>1) so the coverage check has a multi-interaction temporal spread to assert.
	require.Greater(t, prod.Counts.MessagesPerContact, 1)
	// prod-shaped seeds merge + soft-delete scenarios so the coverage check has
	// tombstoned + re-pointed graph rows to assert.
	require.Greater(t, prod.Counts.SeededSoftDeleted, 0)
	require.Greater(t, prod.Counts.SeededMerged, 0)
	// prod-shaped seeds per-subtab Imports candidates (gcontacts/gmail_correspondence/
	// anarlog_humans/telegram-discovery) so the coverage check can assert every Imports
	// subtab has a queue.
	require.Greater(t, prod.Counts.ImportCandidatesPerSource, 0)
}

// TestDefaultParamsUnchanged pins DefaultParams field-for-field so the
// golden-scenario regression (Element 2) keeps passing: minimal-scoped ==
// today's SeedAll + DefaultParams, byte-stable. The Profile field is set
// EXPLICITLY (the zero value "" is an error profile, not minimal-scoped).
func TestDefaultParamsUnchanged(t *testing.T) {
	p := DefaultParams()
	require.Equal(t, "seedall", p.Namespace)
	require.Equal(t, factory.DefaultSeed, p.Seed)
	require.Equal(t, ProfileMinimalScoped, p.Profile)
	require.Equal(t, 1, p.Counts.SeededContacts)
	require.Equal(t, 0, p.Counts.UnmatchedExternal)
	require.Equal(t, 0, p.Counts.StrandedTelegram)
	require.Equal(t, 0, p.Counts.StrandedMessages)
	require.Equal(t, 0, p.Counts.UnmatchedCalendar)
	require.Equal(t, 0, p.Counts.OrphanMeetingNote)
	require.Equal(t, 0, p.Counts.SeededAssertions)
	require.Equal(t, 0, p.Counts.SeededBoolFacts)
	require.Equal(t, 0, p.Counts.SeededRelationships)
	require.Equal(t, 0, p.Counts.SeededTasks)
	require.Equal(t, 0, p.Counts.SeededEntities)
	require.Equal(t, 0, p.Counts.SeededEntityEdges)
	require.Equal(t, 0, p.Counts.SeededSignals)
	require.Equal(t, 0, p.Counts.MessagesPerContact)
	require.Equal(t, 0, p.Counts.SeededSoftDeleted)
	require.Equal(t, 0, p.Counts.SeededMerged)
	require.Equal(t, 0, p.Counts.ImportCandidatesPerSource)
}

// TestCatalogOverdueLadderWellFormed asserts the overdue ladder is a well-formed
// urgency distribution: every pair is genuinely overdue under PRODUCTION cadence
// durations, every cadence is CHECK-valid, the D1 backdated-overdue floor
// (created_at before now-14d) is robustly satisfiable, and the days-overdue
// magnitudes span the single-digit / tens / hundreds tiers the dashboard urgency
// tiers exist to separate (DSH-010). Pure — no DB, no PRNG. The 9-contact bounded
// integration test only surfaces the first few ladder entries, so this covers the
// whole table directly. Prod durations are read from the cadence package's own source
// of truth (not hardcoded) so the test cannot drift from prod semantics.
func TestCatalogOverdueLadderWellFormed(t *testing.T) {
	t.Setenv("CRM_ENV", "production")

	const d1Floor = 14 * 24 * time.Hour
	require.NotEmpty(t, catalogOverdueLadder)

	belowD1Floor := 0
	tensTier := 0 // pairs whose days-overdue lands in [10d, 100d)
	overdueMagnitudes := map[time.Duration]bool{}
	var minOverdue, maxOverdue time.Duration
	for i, pair := range catalogOverdueLadder {
		require.True(t, validCadences[pair.cadence], "ladder[%d] cadence %q must be a migration-005 CHECK cadence", i, pair.cadence)

		ct, err := cadence.ParseCadence(pair.cadence)
		require.NoError(t, err)
		period := cadence.GetCadenceDuration(ct)

		// Genuinely overdue under prod durations: created-age exceeds the cadence
		// period, so the computed contact_by has elapsed.
		require.Greater(t, pair.createdAge, period,
			"ladder[%d] (%s, age %s) must be overdue under prod durations (age > period %s)", i, pair.cadence, pair.createdAge, period)

		overdue := pair.createdAge - period
		overdueMagnitudes[overdue] = true
		if minOverdue == 0 || overdue < minOverdue {
			minOverdue = overdue
		}
		if overdue > maxOverdue {
			maxOverdue = overdue
		}
		if pair.createdAge <= d1Floor {
			belowD1Floor++
		}
		if overdue >= 10*24*time.Hour && overdue < 100*24*time.Hour {
			tensTier++
		}
	}

	// D1 floor robustly satisfiable: at most one ladder pair sits below now-14d, so any
	// overdue cohort spanning >=2 rotation slots always surfaces a backdated-overdue
	// contact for the coherence D1 gate (the single-digit tier below is the only pair
	// permitted under the floor).
	require.LessOrEqual(t, belowD1Floor, 1, "at most one ladder pair may sit below the 14d D1 floor (else the D1 gate can be starved)")

	// Distinct days-overdue magnitudes spanning ALL THREE urgency tiers (DSH-010): a
	// single-digit low tier, a tens middle tier, and a hundreds high tier must each be
	// present (>=3 distinct alone would permit a ladder that skips the middle tier).
	require.GreaterOrEqual(t, len(overdueMagnitudes), 3, "ladder must span >=3 distinct days-overdue magnitudes")
	require.Less(t, minOverdue, 10*24*time.Hour, "ladder must include a single-digit-days-overdue (low-urgency) tier")
	require.GreaterOrEqual(t, tensTier, 1, "ladder must include a tens-of-days-overdue (medium-urgency) tier in [10d, 100d)")
	require.GreaterOrEqual(t, maxOverdue, 100*24*time.Hour, "ladder must include a hundreds-of-days-overdue (high-urgency) tier")
}

// TestInteractionSpreadAgeShape asserts the settled-interaction spread is a
// deterministic, non-uniform distribution: age 0 for the newest message, strictly
// increasing in j, every successive gap >= 7d (so the messaging-aggregate sources
// never collapse two of a contact's messages), gaps that widen for j>=2 (non-uniform
// WITHIN a contact — invisible at MessagesPerContact=2, so the bounded integration
// test cannot cover it), and a first gap that differs across contactIdx (non-uniform
// ACROSS contacts, giving span variety even at MessagesPerContact=2). Pure — no DB.
func TestInteractionSpreadAgeShape(t *testing.T) {
	const minGap = 7 * 24 * time.Hour
	const contacts = 6
	const messages = 5

	firstGaps := map[time.Duration]bool{}
	for c := 0; c < contacts; c++ {
		require.Zero(t, interactionSpreadAge(c, 0), "j=0 is the newest message (age 0, drives last_contacted)")

		var prevAge, prevGap time.Duration
		for j := 1; j < messages; j++ {
			age := interactionSpreadAge(c, j)
			gap := age - prevAge
			require.Greater(t, age, prevAge, "contact %d: age must strictly increase in j (j=%d)", c, j)
			require.GreaterOrEqual(t, gap, minGap, "contact %d: gap at j=%d must be >= 7d", c, j)
			if j == 1 {
				firstGaps[gap] = true
			}
			if j >= 2 {
				require.Greater(t, gap, prevGap, "contact %d: gaps must widen for j>=2 (non-uniform within a contact)", c)
			}
			prevAge, prevGap = age, gap
		}
	}
	require.GreaterOrEqual(t, len(firstGaps), 2,
		"the first gap must differ across contactIdx (span variety even at MessagesPerContact=2)")
}

// TestCatalogCadenceRotation asserts the recent + never-contacted cadence rotation
// pools each offer >=2 distinct CHECK-valid cadences, and that the rotation actually
// varies the cadence across the population deterministically (a pure function of the
// slot index — no PRNG). It exercises catalogOptionsFor end-to-end through the factory
// (in-memory, no DB). Pure.
func TestCatalogCadenceRotation(t *testing.T) {
	for name, pool := range map[string][]string{
		"recent":         catalogRecentCadences,
		"neverContacted": catalogNeverContactedCadences,
	} {
		distinct := map[string]bool{}
		for _, c := range pool {
			require.True(t, validCadences[c], "%s pool cadence %q must be a migration-005 CHECK cadence", name, c)
			distinct[c] = true
		}
		require.GreaterOrEqual(t, len(distinct), 2, "%s cadence pool must offer >=2 distinct cadences", name)
	}

	const n = 18
	collect := func() (recent, never map[string]bool) {
		g := factory.NewGenerator(factory.DefaultSeed, "ladderunit")
		recent, never = map[string]bool{}, map[string]bool{}
		for i := 0; i < n; i++ {
			spec := g.Contact(catalogOptionsFor(i, n, g.Anchor(), g.Prefix())...)
			switch i % 3 {
			case 1: // recent cohort
				require.NotNil(t, spec.Cadence)
				recent[*spec.Cadence] = true
			case 2: // never-contacted cohort
				require.NotNil(t, spec.Cadence)
				never[*spec.Cadence] = true
			}
		}
		return recent, never
	}
	recent1, never1 := collect()
	recent2, never2 := collect()
	require.GreaterOrEqual(t, len(recent1), 2, "recent cohort carries >=2 distinct cadences across the population")
	require.GreaterOrEqual(t, len(never1), 2, "never-contacted cohort carries >=2 distinct cadences across the population")
	require.Equal(t, recent1, recent2, "recent cadence rotation is deterministic (pure function of i)")
	require.Equal(t, never1, never2, "never-contacted cadence rotation is deterministic (pure function of i)")
}
