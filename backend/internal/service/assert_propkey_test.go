package service

import (
	"testing"
	"time"

	"personal-crm/backend/internal/repository"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// strPtr / boolPtr / numPtr / datePtr are tiny pointer helpers for building
// AssertRequests in the pure-function tests.
func strPtr(s string) *string        { return &s }
func boolPtr(b bool) *bool           { return &b }
func numPtr(f float64) *float64      { return &f }
func datePtr(t time.Time) *time.Time { return &t }

// edgePredicate builds a minimal edge predicate for the key/canonicalization
// tests. symmetric + inverse are the dimensions that matter for canonicalEdge.
func edgePredicate(key, objectType string, symmetric bool, inverse *string, bucket string) *repository.Predicate {
	return &repository.Predicate{
		Key:               key,
		Kind:              repository.PredicateKindEdge,
		SubjectType:       "person",
		ObjectType:        &objectType,
		Cardinality:       repository.PredicateCardinalitySingle,
		Symmetric:         symmetric,
		InversePredicate:  inverse,
		PropositionBucket: bucket,
	}
}

func factPredicate(key, valueType, bucket string) *repository.Predicate {
	vt := valueType
	return &repository.Predicate{
		Key:               key,
		Kind:              repository.PredicateKindFact,
		SubjectType:       "person",
		ValueType:         &vt,
		Cardinality:       repository.PredicateCardinalitySingle,
		PropositionBucket: bucket,
	}
}

// TestComputePropositionKey_SymmetricPairOrdering asserts a symmetric edge keys
// identically regardless of which way the caller orders the pair (A→B == B→A).
func TestComputePropositionKey_SymmetricPairOrdering(t *testing.T) {
	a := uuid.MustParse("00000000-0000-0000-0000-0000000000aa")
	b := uuid.MustParse("00000000-0000-0000-0000-0000000000bb")
	pred := edgePredicate("partner_of", "person", true, nil, repository.PredicateBucketNone)

	keyAB := propKeyForEdge(t, pred, a, b)
	keyBA := propKeyForEdge(t, pred, b, a)
	assert.Equal(t, keyAB, keyBA, "symmetric edge keys identically in either order")
}

// TestComputePropositionKey_InverseCanonicalization asserts parent_of(A,B) and
// child_of(B,A) collapse to the same key (canonical token = min(key, inverse)).
func TestComputePropositionKey_InverseCanonicalization(t *testing.T) {
	a := uuid.MustParse("00000000-0000-0000-0000-0000000000aa")
	b := uuid.MustParse("00000000-0000-0000-0000-0000000000bb")
	parentOf := edgePredicate("parent_of", "person", false, strPtr("child_of"), repository.PredicateBucketNone)
	childOf := edgePredicate("child_of", "person", false, strPtr("parent_of"), repository.PredicateBucketNone)

	// parent_of(A,B): A is the parent of B.
	keyParent := propKeyForEdge(t, parentOf, a, b)
	// child_of(B,A): B is the child of A — the SAME relationship.
	keyChild := propKeyForEdge(t, childOf, b, a)
	assert.Equal(t, keyParent, keyChild, "inverse pair collapses to one canonical key")

	// Sanity: parent_of(A,B) and parent_of(B,A) are DIFFERENT relationships.
	keyParentReversed := propKeyForEdge(t, parentOf, b, a)
	assert.NotEqual(t, keyParent, keyParentReversed)
}

// TestComputePropositionKey_BucketGranularity asserts the valid-time bucket
// component truncates valid_from per the predicate's proposition_bucket (UTC).
func TestComputePropositionKey_BucketGranularity(t *testing.T) {
	subject := uuid.MustParse("00000000-0000-0000-0000-0000000000aa")
	jan := time.Date(2024, 1, 15, 10, 0, 0, 0, time.UTC)
	feb := time.Date(2024, 2, 20, 10, 0, 0, 0, time.UTC)
	nextYear := time.Date(2025, 3, 1, 10, 0, 0, 0, time.UTC)

	value := "nyc"

	t.Run("none ignores valid_from entirely", func(t *testing.T) {
		pred := factPredicate("how_met", "text", repository.PredicateBucketNone)
		k1 := propKeyForFact(t, pred, subject, value, &jan)
		k2 := propKeyForFact(t, pred, subject, value, &feb)
		assert.Equal(t, k1, k2, "none bucket dedups regardless of valid_from")
	})

	t.Run("year buckets by calendar year", func(t *testing.T) {
		pred := factPredicate("home_address", "text", repository.PredicateBucketYear)
		kJan := propKeyForFact(t, pred, subject, value, &jan)
		kFeb := propKeyForFact(t, pred, subject, value, &feb)
		kNext := propKeyForFact(t, pred, subject, value, &nextYear)
		assert.Equal(t, kJan, kFeb, "same year → same key")
		assert.NotEqual(t, kJan, kNext, "different year → different key")
	})

	t.Run("month buckets by calendar month", func(t *testing.T) {
		pred := factPredicate("home_address", "text", repository.PredicateBucketMonth)
		kJan := propKeyForFact(t, pred, subject, value, &jan)
		kFeb := propKeyForFact(t, pred, subject, value, &feb)
		assert.NotEqual(t, kJan, kFeb, "different month → different key")
	})

	t.Run("day buckets by calendar day; open valid_from", func(t *testing.T) {
		pred := factPredicate("traveling", "text", repository.PredicateBucketDay)
		sameDayMorning := time.Date(2024, 1, 15, 1, 0, 0, 0, time.UTC)
		sameDayEvening := time.Date(2024, 1, 15, 23, 0, 0, 0, time.UTC)
		kMorning := propKeyForFact(t, pred, subject, value, &sameDayMorning)
		kEvening := propKeyForFact(t, pred, subject, value, &sameDayEvening)
		assert.Equal(t, kMorning, kEvening, "same day → same key")
		// Open valid_from gets the literal "open" token, distinct from a dated one.
		kOpen := propKeyForFact(t, pred, subject, value, nil)
		assert.NotEqual(t, kMorning, kOpen)
	})
}

// TestComputePropositionKey_TextNormalization asserts text values normalize via
// lower(trim()) before keying.
func TestComputePropositionKey_TextNormalization(t *testing.T) {
	subject := uuid.MustParse("00000000-0000-0000-0000-0000000000aa")
	pred := factPredicate("how_met", "text", repository.PredicateBucketNone)
	k1 := propKeyForFact(t, pred, subject, "  New York  ", nil)
	k2 := propKeyForFact(t, pred, subject, "new york", nil)
	assert.Equal(t, k1, k2, "text normalizes via lower(trim())")
}

// TestComputePropositionKey_LengthPrefixNoAlias asserts the length-prefixed
// encoding prevents a delimiter inside a value from aliasing a different tuple.
func TestComputePropositionKey_LengthPrefixNoAlias(t *testing.T) {
	subject := uuid.MustParse("00000000-0000-0000-0000-0000000000aa")
	pred := factPredicate("how_met", "text", repository.PredicateBucketNone)
	// "a:b" vs "a" + "b" must not collide under length-prefix encoding.
	k1 := propKeyForFact(t, pred, subject, "a:5:b", nil)
	k2 := propKeyForFact(t, pred, subject, "a", nil)
	assert.NotEqual(t, k1, k2)
}

// TestComputeLocatorHash_Determinism asserts the same locator hashes identically
// and a different span/version hashes differently.
func TestComputeLocatorHash_Determinism(t *testing.T) {
	base := ProvenanceLocator{
		SourceKind:      repository.SourceKindCommsMessage,
		SourceID:        uuid.NewString(),
		ProducerKind:    repository.ProducerKindExtractor,
		ProducerVersion: "v1",
		Field:           strPtr("body"),
		StartOffset:     i32(10),
		EndOffset:       i32(20),
		InputHash:       "abc",
	}
	h1 := computeLocatorHash(base)
	h2 := computeLocatorHash(base)
	require.Equal(t, h1, h2, "same locator → same hash")

	// A different span.
	span := base
	span.StartOffset = i32(30)
	span.EndOffset = i32(40)
	assert.NotEqual(t, h1, computeLocatorHash(span), "different span → different hash")

	// A different producer version (extractor v2 reassertion).
	ver := base
	ver.ProducerVersion = "v2"
	assert.NotEqual(t, h1, computeLocatorHash(ver), "different version → different hash")

	// A different source row.
	src := base
	src.SourceID = uuid.NewString()
	assert.NotEqual(t, h1, computeLocatorHash(src), "different source → different hash")
}

// TestComputeLocatorHash_NilSpanFields asserts nil offset/chunk fields hash
// stably (the encoding treats nil as empty, deterministically).
func TestComputeLocatorHash_NilSpanFields(t *testing.T) {
	loc := ProvenanceLocator{
		SourceKind:   repository.SourceKindUser,
		SourceID:     "edit:contact-1:lives_in",
		ProducerKind: repository.ProducerKindUser,
	}
	assert.Equal(t, computeLocatorHash(loc), computeLocatorHash(loc))
}

// --- test plumbing ---------------------------------------------------------

func i32(v int32) *int32 { return &v }

// propKeyForEdge canonicalizes + keys an edge for the given subject/object.
func propKeyForEdge(t *testing.T, pred *repository.Predicate, subject, object uuid.UUID) string {
	t.Helper()
	req := &AssertRequest{SubjectNodeID: subject, PredicateKey: pred.Key, ObjectNodeID: &object}
	canonKey, canonSubject, canonObject := canonicalEdge(pred, subject, &object)
	require.NotNil(t, canonObject)
	return computePropositionKey(pred, canonKey, canonSubject, canonObject, req)
}

// propKeyForFact canonicalizes (no-op for facts) + keys a text fact.
func propKeyForFact(t *testing.T, pred *repository.Predicate, subject uuid.UUID, value string, validFrom *time.Time) string {
	t.Helper()
	req := &AssertRequest{SubjectNodeID: subject, PredicateKey: pred.Key, ValueText: strPtr(value), ValidFrom: validFrom}
	canonKey, canonSubject, canonObject := canonicalEdge(pred, subject, nil)
	require.Nil(t, canonObject)
	return computePropositionKey(pred, canonKey, canonSubject, canonObject, req)
}
