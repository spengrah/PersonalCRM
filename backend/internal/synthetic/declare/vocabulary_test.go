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

func TestHistory_CompressedAdjacentMessagesStillHaveDistinctEmailKeys(t *testing.T) {
	t.Setenv("CRM_ENV", "testing")
	gen := factory.NewGenerator(factory.DefaultSeed, "history-compressed")
	contact := gen.Contact(factory.WithEmail())
	const n = 48

	threads := make(map[string]struct{}, n)
	var previous time.Time
	foundInsideManualWindow := false
	for i := 0; i < n; i++ {
		message := gen.GmailMessage(contact, factory.MatchSeeded, factory.WithMessageAge(historyMessageAge(i, n)))
		thread := message.Message.ThreadId
		require.NotEmpty(t, thread)
		require.NotContains(t, threads, thread,
			"email source_ref includes thread id, so every History message needs a distinct thread")
		threads[thread] = struct{}{}

		sentAt := time.UnixMilli(message.Message.InternalDate)
		if !previous.IsZero() && sentAt.Sub(previous) < 30*time.Minute {
			foundInsideManualWindow = true
		}
		previous = sentAt
	}
	assert.True(t, foundInsideManualWindow,
		"the boundary must include adjacent compressed messages inside the manual dedup window")
	assert.Len(t, threads, n)
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

// Every source the vocabulary admits must have a production writer the lowering
// dispatches to; a source outside the set would fall into whichever branch was
// last and produce a row its own sync path cannot.
func TestExternalCandidate_OnlyWriterBackedSourcesAreDeclarable(t *testing.T) {
	// anarlog_title's row IS a token, so it is declared with one.
	for _, source := range []string{
		SourceGContacts, SourceCorrespondence, SourceCalendarAttendee,
		SourceICloudContacts, SourceAnarlogHumans, SourceTelegram,
	} {
		assert.NoError(t, validateEntityOrder([]Entity{ExternalCandidate("c", Source(source))}), source)
	}
	assert.NoError(t, validateEntityOrder([]Entity{
		ExternalCandidate("c", Source(SourceAnarlogTitle), TitleToken("lena")),
	}))

	for _, source := range []string{"", "test", "gcal", "anarlog_sessions"} {
		assert.Error(t, validateEntityOrder([]Entity{ExternalCandidate("c", Source(source))}),
			"source %q has no production writer the lowering can dispatch to", source)
	}
}

// The props are the reason the source set can be this wide: each one is only
// meaningful for the writers that can actually store it, so the validator refuses
// the combinations that would silently produce an unproducible row.
func TestCandidateProps_AreRefusedOnSourcesThatCannotStoreThem(t *testing.T) {
	cases := map[string][]Entity{
		"methods on an ingest source": {
			ExternalCandidate("c", Source(SourceICloudContacts), Emails(2)),
		},
		"methods on correspondence, whose single address is its source_id": {
			ExternalCandidate("c", Source(SourceCorrespondence), Emails(2)),
		},
		"phones on a discovery source": {
			ExternalCandidate("c", Source(SourceTelegram), Phones(1)),
		},
		"a twin name on an ingest source, which mints its own": {
			Contact("a"),
			ExternalCandidate("c", Source(SourceAnarlogHumans), SameNameAs("a")),
		},
		"a telegram handle on a non-telegram source": {
			ExternalCandidate("c", Source(SourceGContacts), TelegramHandle()),
		},
		"no identity on a non-telegram source": {
			ExternalCandidate("c", Source(SourceGContacts), NoIdentity()),
		},
		"no identity together with a pinned twin name": {
			Contact("a"),
			ExternalCandidate("c", Source(SourceTelegram), NoIdentity(), SameNameAs("a")),
		},
		"a title token on a non-title source": {
			ExternalCandidate("c", Source(SourceGContacts), TitleToken("lena")),
		},
		"a title source with no token": {
			ExternalCandidate("c", Source(SourceAnarlogTitle)),
		},
		"correspondence evidence on a non-correspondence source": {
			Contact("a"),
			ExternalCandidate("c", Source(SourceGContacts), CorrespondenceEvidence(4, "a")),
		},
		"a same email as a contact that carries none": {
			Contact("a", NoMethods()),
			ExternalCandidate("c", Source(SourceGContacts), SameEmailAs("a")),
		},
		"a same email as a handle declared later": {
			ExternalCandidate("c", Source(SourceGContacts), SameEmailAs("a")),
			Contact("a"),
		},
	}
	for name, entities := range cases {
		t.Run(name, func(t *testing.T) { assert.Error(t, validateEntityOrder(entities)) })
	}

	assert.NoError(t, validateEntityOrder([]Entity{
		Contact("a", Methods(MethodEmail)),
		ExternalCandidate("c", Source(SourceCalendarAttendee),
			SameNameAs("a"), SameEmailAs("a"), Emails(2), Phones(1)),
		ExternalCandidate("tg", Source(SourceTelegram), NoIdentity(), TelegramHandle()),
		ExternalCandidate("corr", Source(SourceCorrespondence), CorrespondenceEvidence(4, "a")),
	}))
}

// --- meeting notes + method suggestions -------------------------------------

func TestMethodSuggestion_NeedsAnEarlierContactAndAnAddressBookSource(t *testing.T) {
	assert.Error(t, validateEntityOrder([]Entity{
		MethodSuggestion("s", "target", SourceGContacts),
		Contact("target"),
	}), "a forward reference has nothing to resolve against at run time")

	assert.Error(t, validateEntityOrder([]Entity{
		Contact("target"),
		MethodSuggestion("s", "target", SourceICloudContacts),
	}), "icloud_contacts rows come from the ingest pipeline, which cannot produce a linked suggestion row")

	assert.NoError(t, validateEntityOrder([]Entity{
		Contact("target"),
		MethodSuggestion("s", "target", SourceGContacts),
	}))
}

func TestMeetingNote_NeedsAHandleAndReferencesNothing(t *testing.T) {
	assert.Error(t, validateEntityOrder([]Entity{MeetingNote("")}))
	assert.Empty(t, MeetingNote("orphan-a").refs())
	assert.NoError(t, validateEntityOrder([]Entity{MeetingNote("orphan-a"), MeetingNote("orphan-b")}))
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

// The clamp, pinned as a NON-PANICKING substitution. This proves ONLY the
// crash-safety half of the placeholder-year gap: the composed world executes
// every declaration on every reseed, on every calendar day, and February 29 has
// no placeholder-year representation at all, so a panic here would break SEEDING
// rather than fail one assertion. It says nothing about how the app CLASSIFIES
// the clamped date, which is a rendering concern the birthdays spec owns.
func TestBirthdayPlaceholderToday_ClampsFebruary29WithoutPanicking(t *testing.T) {
	leapAnchor := time.Date(2028, time.February, 29, 12, 0, 0, 0, time.UTC)
	plan := placeholderPlanFor(t, BirthdayPlaceholderToday())

	month, day := plan.placeholderMonthDay(leapAnchor)
	assert.Equal(t, time.February, month)
	assert.Equal(t, 28, day, "1900 is not a leap year, so February 29 has to become February 28")

	require.NotPanics(t, func() { factory.WithBirthday1900Sentinel(month, day) },
		"the clamped date must be one the sentinel builder accepts")
	assert.Equal(t, "1900-02-28", plan.resolvePlaceholder(leapAnchor).Format("2006-01-02"))
}

func TestBirthdayPlaceholderToday_UsesTheAnchorsOwnDayOtherwise(t *testing.T) {
	anchor := time.Date(2026, time.June, 15, 8, 0, 0, 0, time.UTC)
	plan := placeholderPlanFor(t, BirthdayPlaceholderToday())

	month, day := plan.placeholderMonthDay(anchor)
	assert.Equal(t, time.June, month)
	assert.Equal(t, 15, day)
	assert.Equal(t, "1900-06-15", plan.resolvePlaceholder(anchor).Format("2006-01-02"))
}

// The declared month/day bounds check must not reject the placeholder's
// zero-valued month/day struct fields: they are never read for it.
func TestBirthdayPlaceholderToday_PassesValidation(t *testing.T) {
	assert.NoError(t, validateEntityOrder([]Entity{Contact("a", BirthdayPlaceholderToday())}))
}

// --- locations --------------------------------------------------------------

// The prefix is the whole point of the helper: the auto-created place node
// carries this label, and the entity teardown's label-prefix sweep is the only
// thing that deletes it. It gets its own executable seam because the end-to-end
// postcondition check cannot tell "correctly prefixed" from "the expectation
// computed the same wrong prefix".
func TestPrefixedLabel(t *testing.T) {
	cases := []struct {
		namespace string
		label     string
		want      string
	}{
		{"loc-ns", "New York", "synth-loc-ns-New York"},
		{"other", "San Francisco", "synth-other-San Francisco"},
	}
	for _, tc := range cases {
		gen := factory.NewGeneratorAt(factory.DefaultSeed, tc.namespace, vocabularyAnchor)
		assert.Equal(t, tc.want, prefixedLabel(gen, tc.label))
	}
}

func TestLocation_RejectsABlankLabel(t *testing.T) {
	for _, blank := range []string{"", "   ", "\t"} {
		label := blank
		assert.Error(t, (&contactPlan{name: "x", location: &label}).validate(),
			"the service normalizes a blank location away, so the postcondition could never hold: %q", blank)
	}
	assert.NoError(t, validateEntityOrder([]Entity{Contact("x", Location("New York"))}))
}

// --- explicit names ---------------------------------------------------------

func TestExplicitName_RequiresBothComponents(t *testing.T) {
	// Written through the PROP, not the struct: a wholly blank pair sets no
	// component at all, so a field-presence check would miss it and the contact
	// would silently fall back to a drawn name — the exact silent degradation
	// pinning an exact literal exists to prevent.
	for _, blank := range [][2]string{{"Cadence", ""}, {"", "Sort Yankee"}, {" ", "Sort Yankee"}, {"", ""}} {
		assert.Error(t, validateEntityOrder([]Entity{Contact("x", ExplicitName(blank[0], blank[1]))}),
			"ExplicitName(%q, %q) must be rejected", blank[0], blank[1])
	}
	assert.Error(t, validateEntityOrder([]Entity{
		Contact("src"),
		Contact("x", ExplicitName("Cadence", "Sort Yankee"), SameNameAs("src")),
	}), "an explicit literal and a twin both state what the rendered name is")
	assert.NoError(t, validateEntityOrder([]Entity{Contact("x", ExplicitName("Cadence", "Sort Yankee"))}))
}

func TestExplicitName_RejectsTwoEntitiesPinningTheSameLiteral(t *testing.T) {
	err := validateEntityOrder([]Entity{
		Contact("a", ExplicitName("Kbd", "Move Alpha")),
		Contact("b", ExplicitName("Kbd", "Move Alpha")),
	})
	require.Error(t, err, "ExplicitName skips the dedupe, so a repeated literal renders one ambiguous name")
	assert.Contains(t, err.Error(), "Kbd Move Alpha")

	assert.NoError(t, validateEntityOrder([]Entity{
		Contact("a", ExplicitName("Kbd", "Move Alpha")),
		Contact("b", ExplicitName("Kbd", "Move Bravo")),
	}))
}

func TestExplicitName_RejectsANameEdge(t *testing.T) {
	err := validateEntityOrder([]Entity{Contact("x", ExplicitName("Kbd", "Move Alpha"), NameEdge(NameEdgeRTL))})
	require.Error(t, err, "a name edge splices its token INTO the pinned pair, so the rendered name is not the literal")
	assert.Contains(t, err.Error(), "NameEdge")
}

// --- shared helpers ---------------------------------------------------------

var vocabularyAnchor = time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)

func placeholderPlanFor(t *testing.T, prop ContactProp) *birthdayPlan {
	t.Helper()
	p := &contactPlan{name: "x"}
	prop(p)
	require.NotNil(t, p.birthday)
	require.True(t, p.birthday.placeholder)
	return p.birthday
}

func intPtr(n int) *int { return &n }
