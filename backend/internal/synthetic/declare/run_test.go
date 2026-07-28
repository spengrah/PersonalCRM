package declare

import (
	"context"
	"testing"
	"time"

	"personal-crm/backend/internal/synthetic/factory"
	"personal-crm/backend/internal/synthetic/replay"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateRequestedNamespace(t *testing.T) {
	valid := []string{"w3-1753700000000-c1", "a", "dev", "prodshaped", "w0-1-c12"}
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
	want := anchor.Add(-overdueMessageAge(p)).Add(-2 * time.Hour).Truncate(time.Millisecond)
	assert.Equal(t, want, sentAt,
		"the Gmail factory's fixed pre-anchor safety lag is additive with the requested age")
	// The floor holds regardless: the message is at least one period + the
	// declared amount before the anchor.
	assert.True(t, sentAt.Before(anchor.Add(-overdueMessageAge(p))))
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
	assert.Nil(t, overdue.CreatedAgo)
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

// --- test seams -------------------------------------------------------------

func TestTestSeamsRequireATestEnvironment(t *testing.T) {
	t.Setenv("CRM_ENV", "production")
	for name, fn := range map[string]func(){
		"SetFailpointForTest":       func() { SetFailpointForTest("") },
		"SetTestHookForTest":        func() { SetTestHookForTest(HookAfterReplayBeforeDrain, nil) },
		"SetCleanupFailStepForTest": func() { SetCleanupFailStepForTest("") },
		"SetBudgetsForTest":         func() { SetBudgetsForTest(time.Second, time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				r := recover()
				require.NotNil(t, r, "%s must refuse to arm outside a test environment", name)
				assert.Contains(t, r, "CRM_ENV=test|testing")
			}()
			fn()
		})
	}
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
