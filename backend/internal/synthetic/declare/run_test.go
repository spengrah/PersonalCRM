package declare

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"personal-crm/backend/internal/synthetic/factory"

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
