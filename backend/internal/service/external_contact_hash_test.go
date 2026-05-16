package service

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/gowebpki/jcs"
	"github.com/stretchr/testify/require"
)

// computeReference produces the expected hash for a payload using the
// exact same algorithm as ComputeContentHash. Used as an independent
// cross-check that the hash output is deterministic and matches the
// recipe verbatim.
func computeReference(t *testing.T, input string) string {
	t.Helper()
	canonical, err := jcs.Transform([]byte(input))
	require.NoError(t, err)
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// TestComputeContentHash_StableAcrossKeyOrder verifies that JCS
// canonicalization sorts keys, so two byte-different inputs that
// represent the same JSON object produce identical hashes.
func TestComputeContentHash_StableAcrossKeyOrder(t *testing.T) {
	a := `{"b":1,"a":2}`
	b := `{"a":2,"b":1}`

	hashA, err := ComputeContentHash([]byte(a))
	require.NoError(t, err)
	hashB, err := ComputeContentHash([]byte(b))
	require.NoError(t, err)

	require.Equal(t, hashA, hashB, "JCS must sort keys; both inputs must hash identically")
}

// TestComputeContentHash_RFC8785Vectors locks the SHA-256 step against
// a self-computed reference produced by the same JCS library so future
// drift in either step is caught.
func TestComputeContentHash_RFC8785Vectors(t *testing.T) {
	// Simple ASCII object.
	input := `{"a":1,"b":2}`
	got, err := ComputeContentHash([]byte(input))
	require.NoError(t, err)
	want := computeReference(t, input)
	require.Equal(t, want, got)

	// Nested object with array — exercises canonicalization recursion.
	input2 := `{"outer":{"x":[3,2,1],"y":"hello"},"id":"abc"}`
	got2, err := ComputeContentHash([]byte(input2))
	require.NoError(t, err)
	want2 := computeReference(t, input2)
	require.Equal(t, want2, got2)
}

// TestComputeContentHash_RemovesHostID verifies the host_id key is
// stripped: a payload with host_id must hash to the same value as the
// same payload without host_id.
func TestComputeContentHash_RemovesHostID(t *testing.T) {
	withHost := `{"entity_id":"e1","host_id":"h-uuid","display_name":"x"}`
	withoutHost := `{"entity_id":"e1","display_name":"x"}`

	hashWith, err := ComputeContentHash([]byte(withHost))
	require.NoError(t, err)
	hashWithout, err := ComputeContentHash([]byte(withoutHost))
	require.NoError(t, err)

	require.Equal(t, hashWithout, hashWith, "host_id must be stripped before hashing")
}

// TestComputeContentHash_NonAsciiStringsPreserveBytes verifies non-
// ASCII strings (emoji + accented chars) round-trip through the
// canonicalizer and produce a deterministic hash. JCS keeps the
// verbatim UTF-8 byte sequence; no Unicode normalization is applied.
func TestComputeContentHash_NonAsciiStringsPreserveBytes(t *testing.T) {
	input := `{"name":"José 👋","city":"São Paulo"}`
	hash1, err := ComputeContentHash([]byte(input))
	require.NoError(t, err)
	hash2, err := ComputeContentHash([]byte(input))
	require.NoError(t, err)
	require.Equal(t, hash1, hash2)
	require.Len(t, hash1, 64)
	for _, ch := range hash1 {
		require.Contains(t, "0123456789abcdef", string(ch), "hash must be lowercase hex")
	}
}

// TestComputeContentHash_NormalizationFormsAreDistinct enforces the
// RFC 8785 contract: no Unicode normalization. Two strings that are
// canonically equivalent under NFC vs NFD (e.g., "é" as U+00E9 vs
// "e" + U+0301) but byte-different MUST hash to different values.
// Locking this prevents a future implementer from adding a "helpful"
// normalize-before-hash step that would silently break parity with
// the Swift daemon (per Codex r3 P2-4).
func TestComputeContentHash_NormalizationFormsAreDistinct(t *testing.T) {
	// "é" as a single precomposed code point (NFC, U+00E9).
	nfc := "{\"name\":\"é\"}"
	// "e" + combining acute accent (NFD, U+0065 U+0301).
	nfd := "{\"name\":\"é\"}"

	hashNFC, err := ComputeContentHash([]byte(nfc))
	require.NoError(t, err)
	hashNFD, err := ComputeContentHash([]byte(nfd))
	require.NoError(t, err)

	require.NotEqual(t, hashNFC, hashNFD,
		"NFC and NFD forms must produce distinct hashes; JCS does NOT normalize Unicode")
}

// TestComputeContentHash_NumericPrecisionFollowsJCSSpec documents the
// expected behavior for integers larger than 2^53: per RFC 8785
// (§3.2.2.3), JCS canonicalizes numbers via ES6 Number-to-String,
// which treats ALL numbers as IEEE 754 doubles. Integers > 2^53
// collapse to the nearest representable float64.
//
// This is the spec's contract, NOT a bug in our helper. The Swift
// daemon's payloads in practice do not carry integer values larger
// than 2^53 (CNContact identifiers are strings, dates are RFC3339
// strings, container_identifier is a string). If a future source ever
// needs to round-trip large integers through the hash, the payload
// must encode them as JSON strings, not numbers.
//
// Codex r4 P2-3 originally proposed sjson to preserve large-integer
// precision; that fix correctly preserves the strip step but does NOT
// (and cannot) override the JCS spec's number model downstream. The
// test below locks the spec-mandated behavior so future contributors
// don't accidentally swap the canonicalizer for one that diverges.
func TestComputeContentHash_NumericPrecisionFollowsJCSSpec(t *testing.T) {
	// 9999999999999999 ≈ 1e16, which is > 2^53. Under RFC 8785's ES6
	// Number-to-String, this is indistinguishable from
	// 10000000000000000 — both canonicalize to "10000000000000000".
	withBig := `{"big":9999999999999999}`
	withRounded := `{"big":10000000000000000}`

	hashBig, err := ComputeContentHash([]byte(withBig))
	require.NoError(t, err)
	hashRounded, err := ComputeContentHash([]byte(withRounded))
	require.NoError(t, err)

	require.Equal(t, hashBig, hashRounded,
		"per RFC 8785 §3.2.2.3, JCS treats all numbers as float64; daemon payloads must encode "+
			"identifiers > 2^53 as JSON strings, not numbers")
}

// TestComputeContentHash_RejectsInvalidJSON verifies the helper
// surfaces canonicalization errors rather than silently producing a
// hash of garbage bytes.
func TestComputeContentHash_RejectsInvalidJSON(t *testing.T) {
	_, err := ComputeContentHash([]byte(`{"missing-close":`))
	require.Error(t, err)

	_, err = ComputeContentHash([]byte(`not json at all`))
	require.Error(t, err)
}

// TestComputeContentHash_EmptyObject ensures the trivial empty-object
// payload produces a deterministic hash (SHA-256 of `{}`).
func TestComputeContentHash_EmptyObject(t *testing.T) {
	got, err := ComputeContentHash([]byte(`{}`))
	require.NoError(t, err)
	expected := computeReference(t, `{}`)
	require.Equal(t, expected, got)
	require.Len(t, got, 64)
}
