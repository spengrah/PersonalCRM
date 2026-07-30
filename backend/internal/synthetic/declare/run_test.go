package declare

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/synthetic/replay"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateRequestedNamespace(t *testing.T) {
	valid := []string{"w3-1753700000000-c1", "a", "standard", "seedall", "w0-1-c12"}
	for _, ns := range valid {
		assert.NoError(t, ValidateRequestedNamespace(ns), "%q should be valid", ns)
	}

	invalid := map[string]string{
		"":               "length 0",
		"UPPER":          "charset",
		"has_underscore": "LIKE metacharacter",
		"has%percent":    "LIKE metacharacter",
		"w3-1700-s1":     "reserved salt suffix",
		"s3":             "reserved salt suffix (single segment)",
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa": "too long",
	}
	for ns, why := range invalid {
		assert.ErrorIs(t, ValidateRequestedNamespace(ns), ErrInvalidNamespace, "%q should be rejected (%s)", ns, why)
	}
}

// Cleanup legitimately receives EFFECTIVE namespaces, which DO end in -sN.
func TestValidateNamespaceTokenAcceptsSaltedValues(t *testing.T) {
	assert.NoError(t, ValidateNamespaceToken("w3-1700-c1-s1"))
	assert.Error(t, ValidateNamespaceToken("w3-1700-c1-s1_"))
}

// The grammars of the two paths must MEET: every namespace a seed can produce
// has to be a namespace cleanup accepts. Re-salting appends a suffix the caller
// never asked for and the effective token is never revalidated, so a requested
// namespace accepted right up against the token limit would seed a world under
// an over-long token — which the client then hands back to cleanup, whose
// validator rejects the whole request and leaves the rows in the shared
// database. The longest ACCEPTED requested namespace is derived here rather
// than read from the constant, so the property is proven about the validator's
// actual behavior.
func TestSaltVariantsOfTheLongestRequestedNamespaceStayValidTokens(t *testing.T) {
	longest := ""
	for n := 1; n <= maxNamespaceLen+maxSaltSuffixLen; n++ {
		candidate := strings.Repeat("a", n)
		if ValidateRequestedNamespace(candidate) == nil {
			longest = candidate
		}
	}
	require.NotEmpty(t, longest, "the validator accepted no namespace at all")

	for _, variant := range saltVariants(longest) {
		assert.NoError(t, ValidateNamespaceToken(variant),
			"re-salting the longest accepted requested namespace (%d chars) minted %q, which cleanup rejects",
			len(longest), variant)
		assert.NoError(t, ValidateCleanupNamespaces([]string{variant}),
			"a cleanup request naming %q must be accepted", variant)
	}
}

// The reserved room is a literal (a const cannot format one), so hold it to the
// widest suffix re-salting can actually mint.
func TestSaltSuffixFitsItsReservedRoom(t *testing.T) {
	assert.Equal(t, maxSaltSuffixLen, len(fmt.Sprintf("-s%d", maxSaltAttempt)))
}

func TestHyphenAncestors(t *testing.T) {
	assert.Equal(t, []string{"w3", "w3-1700"}, hyphenAncestors("w3-1700-c1"))
	assert.Nil(t, hyphenAncestors("single"))
	assert.Equal(t, []string{"foo"}, hyphenAncestors("foo-bar"))
	// A leading hyphen has no proper prefix before it.
	assert.Nil(t, hyphenAncestors("-lead"))
}

func TestIsSaltVariantOf(t *testing.T) {
	assert.True(t, isSaltVariantOf("foo-s1", "foo"))
	assert.True(t, isSaltVariantOf("foo-s7", "foo"))
	assert.False(t, isSaltVariantOf("foo-bar", "foo"))
	assert.False(t, isSaltVariantOf("foo-s1-more", "foo"))
	assert.False(t, isSaltVariantOf("foo", "foo"))
	assert.False(t, isSaltVariantOf("foobar-s1", "foo"))
}

func TestAdvisoryKeyIsStableAndScoped(t *testing.T) {
	assert.Equal(t, advisoryKey("declare:abc"), advisoryKey("declare:abc"))
	assert.NotEqual(t, advisoryKey("declare:abc"), advisoryKey("declare:abd"))
	// The scoping prefixes keep the namespace reservation and the band claims in
	// disjoint key domains even for identical tokens.
	assert.NotEqual(t, advisoryKey("declare:204"), advisoryKey("declare-band:phone:204"))
}

func TestBandKeysAreSorted(t *testing.T) {
	keys := bandKeys(factory.NewGeneratorAt(factory.DefaultSeed, "band-order-check", time.Unix(0, 0).UTC()))
	require.Len(t, keys, 2)
	assert.Less(t, keys[0], keys[1])
}

func TestSeedOrDefault(t *testing.T) {
	assert.Equal(t, factory.DefaultSeed, seedOrDefault(0))
	assert.Equal(t, uint64(42), seedOrDefault(42))
}

// --- lowering ---------------------------------------------------------------

// The requested message age is exactly the distance the bespoke overdue seeder
// backdated last_contacted by: cadence_duration + days × (weekly / 7).
func TestOverdueMessageAgeMatchesTheLegacyBackdateDistance(t *testing.T) {
	t.Setenv("CRM_ENV", "testing")
	p := Contact("x", Cadence("weekly"), OverdueBy(Days(3))).(*contactPlan)
	legacy := 2*time.Minute + 3*(2*time.Minute/7) // cadence duration + 3 scaled days
	assert.Equal(t, legacy, overdueMessageAge(p))

	monthly := Contact("y", Cadence("monthly"), OverdueBy(Days(5))).(*contactPlan)
	assert.Equal(t, 10*time.Minute+5*(2*time.Minute/7), overdueMessageAge(monthly))
}

// ...and the REALIZED interaction instant carries the Gmail factory's fixed
// safety lag ON TOP of that age, because the provider only scans already-closed
// windows. The lag is additive and environment-independent, so in a compressed
// environment it dominates the domain amount and the RENDERED day count is much
// larger than the declared one. The declared semantics is a FLOOR ("overdue by
// at least"), which still holds — this test pins the gap so it stays a known,
// asserted property instead of a surprise, and will fail loudly if the factory
// ever changes the lag.
func TestOverdueLoweringCarriesTheSourceSafetyLag(t *testing.T) {
	t.Setenv("CRM_ENV", "testing")
	anchor := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	gen := factory.NewGeneratorAt(factory.DefaultSeed, "lowering-check", anchor)

	p := Contact("x", Cadence("weekly"), OverdueBy(Days(3))).(*contactPlan)
	spec := gen.Contact(factory.WithCadence("weekly"))
	msg := gen.GmailMessage(spec, factory.MatchSeeded, factory.WithMessageAge(overdueMessageAge(p)))

	// The wire field is epoch MILLIS, so compare at that resolution.
	sentAt := time.UnixMilli(msg.Message.InternalDate).UTC()
	want := anchor.Add(-overdueMessageAge(p)).Add(-sourceHistoryLag).Truncate(time.Millisecond)
	assert.Equal(t, want, sentAt,
		"declare's locally stated source safety lag drifted from the factory's actual pre-anchor offset")
	// The floor holds regardless: the message is at least one period + the
	// declared amount before the anchor.
	assert.True(t, sentAt.Before(anchor.Add(-overdueMessageAge(p))))

	// ...and creation precedes that instant by a FULL PERIOD. That margin is
	// what makes the app's forward-only, DATE-granular due-date update a strict
	// move: the create-time due date is created_at + period, the derived one is
	// last_contacted + period, and with this margin they are a whole period
	// apart. A smaller margin can land both on the same calendar date, in which
	// case the update is a no-op and the contact is never overdue.
	createdAge, ok := creationAge(p)
	require.True(t, ok, "OverdueBy must derive a creation age")
	assert.Equal(t, overdueMessageAge(p)+sourceHistoryLag+period("weekly"), createdAge)
	assert.True(t, anchor.Add(-createdAge).Before(sentAt), "a contact must exist before the history it carries")
}

// --- postconditions ---------------------------------------------------------

func TestPostconditionsDerivedPerProperty(t *testing.T) {
	t.Setenv("CRM_ENV", "testing")

	d := Declaration{Behavior: "ZZZ-900", Entities: []Entity{
		Contact("overdue", Cadence("weekly"), OverdueBy(Days(3))),
		Contact("stale", Cadence("weekly"), NeverContacted(), CreatedAgo(Periods(2))),
		Contact("fresh", Cadence("weekly"), NeverContacted(), CreatedAgo(Days(1))),
		Contact("bare", NoMethods()),
		Contact("multi", Methods("telegram", "phone", "email")),
	}}
	pcs := d.Postconditions()
	require.Len(t, pcs, 5)
	byHandle := map[string]Postcondition{}
	for _, pc := range pcs {
		byHandle[pc.Handle] = pc
	}

	overdue := byHandle["overdue"]
	assert.True(t, overdue.Listed)
	require.NotNil(t, overdue.OverdueMember)
	assert.True(t, *overdue.OverdueMember)
	require.NotNil(t, overdue.LastContacted)
	assert.True(t, *overdue.LastContacted)
	require.NotNil(t, overdue.Cadence)
	assert.Equal(t, "weekly", *overdue.Cadence)
	require.NotNil(t, overdue.CreatedAgo, "the derived creation age is a checkable fact too")
	assert.Equal(t, overdueMessageAge(d.Entities[0].(*contactPlan))+sourceHistoryLag+period("weekly"), *overdue.CreatedAgo)
	assert.Equal(t, []string{MethodEmail}, overdue.MethodKinds)

	// Overdue via created_at: two periods back is past one period, so it IS
	// overdue, and it has no history so last_contacted must be null.
	stale := byHandle["stale"]
	require.NotNil(t, stale.OverdueMember)
	assert.True(t, *stale.OverdueMember)
	require.NotNil(t, stale.LastContacted)
	assert.False(t, *stale.LastContacted)
	require.NotNil(t, stale.CreatedAgo)
	assert.Equal(t, 2*period("weekly"), *stale.CreatedAgo)

	// One day back is INSIDE a weekly period, so this one is NOT overdue — the
	// derived false branch, which a presence-only assertion would never catch.
	fresh := byHandle["fresh"]
	require.NotNil(t, fresh.OverdueMember)
	assert.False(t, *fresh.OverdueMember)

	bare := byHandle["bare"]
	assert.Equal(t, []string{}, bare.MethodKinds)
	assert.Nil(t, bare.Cadence)
	require.NotNil(t, bare.OverdueMember)
	assert.False(t, *bare.OverdueMember, "a contact with no cadence can never be overdue")

	assert.Equal(t, []string{"email", "phone", "telegram"}, byHandle["multi"].MethodKinds)
}

// A removed contact's detail read is a 404, so its postcondition must carry NO
// fact that is checked against that read — the two NAME facts included, since
// both are asserted against the detail read's full_name. Every consumer today
// branches on Present before it reads anything else, which is exactly why the
// derivation itself is pinned here: the next one that reads a name fact first
// would assert a display name against a 404.
func TestPostconditionsForRemovedContactDropEveryDetailReadFact(t *testing.T) {
	t.Setenv("CRM_ENV", "testing")

	anchor := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	d := Declaration{Behavior: "ZZZ-901", Entities: []Entity{
		Contact("gone",
			Cadence("weekly"),
			OverdueBy(Days(3)),
			ExplicitName("Kbd", "Move Alpha"),
			NameMarker("cadflt"),
			Location("Placeholderton"),
			BirthdayInDays(3),
			Methods("email", "phone"),
		),
		Contact("survivor"),
		SoftDelete("gone"),
	}}

	// The fixture must be a LEGAL declaration, or the derivation it exercises
	// describes a world that could never be registered.
	require.NoError(t, validateEntityOrder(d.Entities))

	byHandle := map[string]Postcondition{}
	for _, pc := range d.PostconditionsAt(anchor) {
		byHandle[pc.Handle] = pc
	}

	gone := byHandle["gone"]
	require.NotNil(t, gone.Present)
	assert.False(t, *gone.Present)
	assert.False(t, gone.Listed)
	assert.Nil(t, gone.Cadence)
	assert.Nil(t, gone.LastContacted)
	assert.Nil(t, gone.CreatedAgo)
	assert.Nil(t, gone.MethodKinds)
	assert.Nil(t, gone.Birthday)
	assert.Nil(t, gone.Location)
	assert.Nil(t, gone.InteractionCount)
	assert.Nil(t, gone.ExplicitName, "a pinned name is read off the 404ing detail response")
	assert.Nil(t, gone.NameMarker, "so is the resolution marker")
	assert.False(t, gone.CreatedBeforeOldestInteraction)
	require.NotNil(t, gone.OverdueMember)
	assert.False(t, *gone.OverdueMember,
		"leaving the overdue read is the one observable consequence that stays assertable")

	// The survivor proves the nil-out is SCOPED to the removed handle — a blanket
	// one would satisfy every assertion above.
	survivor := byHandle["survivor"]
	assert.Nil(t, survivor.Present)
	assert.True(t, survivor.Listed)
	assert.NotNil(t, survivor.MethodKinds)
}

// --- test seams -------------------------------------------------------------

// The misuse guard cannot be observed end-to-end from inside a test binary —
// testing.Testing() is true here by construction — so the PREDICATE is tested
// directly and the panic path is tested against it. Between them they cover the
// only case that matters: a non-test binary outside the test environment.
func TestTestSeamsPredicate(t *testing.T) {
	assert.True(t, testSeamsAllowed("production", true), "a go test binary may always arm a seam")
	assert.True(t, testSeamsAllowed("", true))
	assert.True(t, testSeamsAllowed("test", false))
	assert.True(t, testSeamsAllowed("testing", false))
	assert.False(t, testSeamsAllowed("production", false), "a production binary in production must not arm a seam")
	assert.False(t, testSeamsAllowed("", false))
	assert.False(t, testSeamsAllowed("staging", false))
}

func TestTestSeamsPanicWhenDisallowed(t *testing.T) {
	assert.NotPanics(t, func() { requireSeamsAllowed("declare.Whatever", true) })
	defer func() {
		r := recover()
		require.NotNil(t, r, "a disallowed seam must panic")
		assert.Contains(t, r, "declare.Whatever is test-only support")
	}()
	requireSeamsAllowed("declare.Whatever", false)
}

func TestTestSeamsRejectUnknownNames(t *testing.T) {
	t.Setenv("CRM_ENV", "testing")
	assert.PanicsWithValue(t, `declare: unknown failpoint "nope"`, func() { SetFailpointForTest("nope") })
	assert.PanicsWithValue(t, `declare: unknown test-hook point "nope"`, func() { SetTestHookForTest("nope", nil) })
}

func TestSeamsRestoreCleanly(t *testing.T) {
	t.Setenv("CRM_ENV", "testing")

	restoreFailpoint := SetFailpointForTest(FailpointAfterFirstEntity)
	assert.Equal(t, FailpointAfterFirstEntity, currentFailpoint())
	restoreFailpoint()
	assert.Equal(t, "", currentFailpoint())

	hook := func(context.Context, *replay.Harness) error { return nil }
	restoreHook := SetTestHookForTest(HookAfterReplayBeforeDrain, hook)
	assert.NotNil(t, currentHook(HookAfterReplayBeforeDrain))
	restoreHook()
	assert.Nil(t, currentHook(HookAfterReplayBeforeDrain))

	restoreStep := SetCleanupFailStepForTest("contacts")
	assert.Equal(t, "contacts", currentCleanupFailStep())
	restoreStep()
	assert.Equal(t, "", currentCleanupFailStep())
}

func TestWorstCaseRunResidenceIsFiniteAndSumsTheRealTimers(t *testing.T) {
	t.Setenv("CRM_ENV", "testing")
	restore := SetBudgetsForTest(2*time.Second, 3*time.Second)
	defer restore()
	bound := WorstCaseRunResidence()
	assert.Greater(t, bound, 5*time.Second, "the bound must include the toolkit's own fixed settle timers")
	assert.Less(t, bound, 5*time.Minute, "the bound must be finite and small enough to be meaningful")
}
