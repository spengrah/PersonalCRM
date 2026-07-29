package declare

import (
	"testing"
	"time"

	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/synthetic/replay"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- History ----------------------------------------------------------------

// The batch adapter refuses a Gmail batch whose oldest-to-newest span exceeds
// one sync's reach, and it refuses with a Gate-A timeout blaming the wrong
// thing. The spread has to be checked against the ACTIVE table, in EVERY
// environment, because "a day" is a different duration in each — so a future
// change to `weekly` in any of the three tables surfaces here rather than as a
// mysterious timeout on staging.
func TestHistory_SpreadFitsOneBatchSyncInEveryEnvironment(t *testing.T) {
	for _, env := range []string{"", "testing", "test", "accelerated"} {
		t.Run("CRM_ENV="+env, func(t *testing.T) {
			t.Setenv("CRM_ENV", env)
			span := historyMessageAge(0, 48) - historyMessageAge(47, 48)
			assert.LessOrEqual(t, span, replay.GmailBatchMaxSpan(),
				"the declared history spread must fit one batch sync's reach")
			assert.NoError(t, historySpanWithinBatchReach(48))
		})
	}
}

func TestHistory_SpreadIsOldestFirstAndMonotonic(t *testing.T) {
	const n = 48
	prev := historyMessageAge(0, n)
	assert.Equal(t, time.Duration(historyOldestDays)*dayLength(), prev, "message 0 must be the OLDEST — the batch adapter requires chronological order")
	for i := 1; i < n; i++ {
		age := historyMessageAge(i, n)
		assert.LessOrEqual(t, age, prev, "ages must not increase as the index grows")
		prev = age
	}
	assert.Equal(t, time.Duration(historyNewestDays)*dayLength(), prev, "the last message must be the newest")
}

// The creation margin has to be STRICTLY positive. A creation instant exactly
// equal to the oldest message means the first email arrived at the very instant
// the contact was added, which is not the property the margin exists for — and
// the edge asserts created_at strictly before the oldest occurred_at through the
// read path.
func TestHistory_CreationPrecedesTheOldestMessageStrictly(t *testing.T) {
	p := &contactPlan{name: "h", cadence: "monthly"}
	n := 48
	p.history = &n

	age, ok := creationAge(p)
	require.True(t, ok, "a History-bearing contact must be backdated")
	oldestRealized := historyMessageAge(0, n) + sourceHistoryLag
	assert.Greater(t, age, oldestRealized,
		"creation must precede the oldest message's REALIZED instant (requested age plus the source's fixed lag), strictly")
	assert.Equal(t, oldestRealized+historyCreationMargin(), age)
	assert.Positive(t, historyCreationMargin())
}

func TestHistory_PostconditionsAreDerivedFromTheSpread(t *testing.T) {
	e, ok := LookupEdge("long-history")
	require.True(t, ok)
	pcs := e.Postconditions()
	require.Len(t, pcs, 1)
	pc := pcs[0]

	require.NotNil(t, pc.InteractionCount)
	assert.Equal(t, 48, *pc.InteractionCount)
	require.NotNil(t, pc.LastContacted)
	assert.True(t, *pc.LastContacted, "replayed history moves last_contacted")
	assert.True(t, pc.CreatedBeforeOldestInteraction)
	require.NotNil(t, pc.OverdueMember)
	// Derived, not restated: the newest message is one day old and the cadence is
	// monthly, so the contact is NOT overdue.
	assert.False(t, *pc.OverdueMember)
}

func TestHistory_MutualExclusions(t *testing.T) {
	n := 3
	cases := map[string]*contactPlan{
		"with OverdueBy":      {name: "x", cadence: "weekly", history: &n, overdueBy: amountPtr(Days(1))},
		"with CreatedAgo":     {name: "x", cadence: "weekly", history: &n, createdAgo: amountPtr(Days(1))},
		"with NeverContacted": {name: "x", cadence: "weekly", history: &n, neverContacted: true},
		"without an email":    {name: "x", cadence: "weekly", history: &n, noMethods: true},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) { assert.Error(t, p.validate()) })
	}

	zero := 0
	assert.Error(t, (&contactPlan{name: "x", history: &zero}).validate(), "History(0) creates nothing")
}

func amountPtr(a Amount) *Amount { return &a }

// --- name edges + twins -----------------------------------------------------

func TestNameEdge_ValidationRejectsUnknownKindsAndTwinCombination(t *testing.T) {
	assert.Error(t, (&contactPlan{name: "x", nameEdge: "no-such-edge"}).validate())
	assert.Error(t, (&contactPlan{name: "x", nameEdge: NameEdgeLong, sameNameAs: "other"}).validate(),
		"a twin copies the source's rendered name, edge token included — declaring both states two different names")
	assert.NoError(t, (&contactPlan{name: "x", nameEdge: NameEdgeRTL}).validate())
}

func TestSameNameAs_MustReferenceAnEarlierContact(t *testing.T) {
	assert.Error(t, validateEntityOrder([]Entity{
		Contact("a", SameNameAs("b")),
		Contact("b"),
	}), "a forward reference has nothing to resolve against at run time")

	assert.Error(t, validateEntityOrder([]Entity{Contact("a", SameNameAs("a"))}),
		"a self reference is a twin of nothing")

	assert.Error(t, validateEntityOrder([]Entity{
		ExternalCandidate("cand", Source(SourceGContacts)),
		Contact("a", SameNameAs("cand")),
	}), "only a contact can be twinned")

	assert.NoError(t, validateEntityOrder([]Entity{
		Contact("a"),
		Contact("b", SameNameAs("a")),
		ExternalCandidate("cand", Source(SourceGContacts), SameNameAs("a")),
	}))
}

// --- import candidates ------------------------------------------------------

func TestExternalCandidate_OnlyTheTwoKeyableSourcesAreDeclarable(t *testing.T) {
	for _, source := range []string{SourceGContacts, SourceCorrespondence} {
		assert.NoError(t, validateEntityOrder([]Entity{ExternalCandidate("c", Source(source))}), source)
	}
	for _, source := range []string{"", "telegram", "anarlog_humans", "icloud_contacts"} {
		assert.Error(t, validateEntityOrder([]Entity{ExternalCandidate("c", Source(source))}),
			"source %q is not one the seeding primitive can key", source)
	}
}

// --- merges / soft deletes / notes ------------------------------------------

func TestReferencingEntitiesRequireAnEarlierContact(t *testing.T) {
	cases := map[string][]Entity{
		"merge before its contacts":        {Merge("a", "b"), Contact("a"), Contact("b")},
		"merge of a non-contact":           {Contact("a"), Note("n", "a"), Merge("n", "a")},
		"soft delete before its contact":   {SoftDelete("a"), Contact("a")},
		"note before its contact":          {Note("n", "a"), Contact("a")},
		"merge into itself":                {Contact("a"), Merge("a", "a")},
		"duplicate handle across kinds":    {Contact("a"), Note("a", "a")},
		"soft delete of an unknown handle": {Contact("a"), SoftDelete("ghost")},
	}
	for name, entities := range cases {
		t.Run(name, func(t *testing.T) { assert.Error(t, validateEntityOrder(entities)) })
	}

	assert.NoError(t, validateEntityOrder([]Entity{
		Contact("a"), Note("a-note", "a"), Contact("b"), Contact("c"),
		Merge("a", "b"), Merge("b", "c"),
	}))
}

func TestReferencingEntityHandlesAreDistinct(t *testing.T) {
	// The derived handles must not collide when a world declares several merges
	// or soft-deletes; a collision would silently overwrite a manifest entry.
	assert.NoError(t, validateEntityOrder([]Entity{
		Contact("a"), Contact("b"), Contact("c"),
		Merge("a", "b"), Merge("b", "c"),
	}))
	assert.Equal(t, "merge-a-into-b", Merge("a", "b").handle())
	assert.Equal(t, "soft-delete-parent", SoftDelete("parent").handle())
}

// --- birthdays --------------------------------------------------------------

func TestBirthday_ResolvesOnALeapSafeYear(t *testing.T) {
	// A Feb-29 anchor is the case the 1900 sentinel could not express.
	anchor := time.Date(2028, time.February, 29, 12, 0, 0, 0, time.UTC)
	year := factory.LeapSafeBirthYear(anchor)

	today := (&birthdayPlan{inDays: intPtr(0)}).resolve(anchor)
	assert.Equal(t, time.Date(year, time.February, 29, 0, 0, 0, 0, time.UTC), today,
		"BirthdayInDays(0) on a Feb-29 anchor must store Feb 29, not roll to Mar 1")

	leap := (&birthdayPlan{month: time.February, day: 29}).resolve(anchor)
	assert.Equal(t, time.Date(year, time.February, 29, 0, 0, 0, 0, time.UTC), leap)

	tomorrow := (&birthdayPlan{inDays: intPtr(1)}).resolve(anchor)
	assert.Equal(t, time.Date(year, time.March, 1, 0, 0, 0, 0, time.UTC), tomorrow)
}

func TestBirthday_ValidationRejectsImpossibleDates(t *testing.T) {
	assert.Error(t, (&contactPlan{name: "x", birthday: &birthdayPlan{month: time.February, day: 30}}).validate())
	assert.Error(t, (&contactPlan{name: "x", birthday: &birthdayPlan{month: time.April, day: 31}}).validate())
	assert.Error(t, (&contactPlan{name: "x", birthday: &birthdayPlan{month: time.Month(13), day: 1}}).validate())
	// February 29 IS a real date on a leap-safe birth year, so it must be
	// accepted — rejecting it would be the same silent clamp in a new place.
	assert.NoError(t, (&contactPlan{name: "x", birthday: &birthdayPlan{month: time.February, day: 29}}).validate())
}

func TestBirthday_PostconditionCarriesTheResolvedDate(t *testing.T) {
	anchor := time.Date(2026, time.July, 29, 8, 0, 0, 0, time.UTC)
	e, ok := LookupEdge("birthday-window")
	require.True(t, ok)

	byHandle := map[string]*time.Time{}
	for _, pc := range e.PostconditionsAt(anchor) {
		byHandle[pc.Handle] = pc.Birthday
	}
	require.NotNil(t, byHandle["bday-today"])
	assert.Equal(t, time.July, byHandle["bday-today"].Month())
	assert.Equal(t, 29, byHandle["bday-today"].Day())
	require.NotNil(t, byHandle["bday-tomorrow"])
	assert.Equal(t, 30, byHandle["bday-tomorrow"].Day())
	require.NotNil(t, byHandle["bday-leap"])
	assert.Equal(t, time.February, byHandle["bday-leap"].Month())
	assert.Equal(t, 29, byHandle["bday-leap"].Day())
}

func intPtr(n int) *int { return &n }
