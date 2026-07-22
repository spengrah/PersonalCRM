package replay

import (
	"context"
	"regexp"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestPaddedBase62UUID pins the injective, fixed-width, alphanumeric external-id
// encoding the visible-task spread relies on to keep a contact's multiple manual
// tasks distinct on unique_external_task_id.
func TestPaddedBase62UUID(t *testing.T) {
	alnum := regexp.MustCompile(`^[A-Za-z0-9]+$`)
	wantLen := base62UUIDWidth + visibleTaskOrdinalWidth

	// A low-valued UUID (all-zero) exercises the left-padding boundary: base62 of 0
	// is a single "0", so the UUID component must pad out to base62UUIDWidth. Random
	// fixtures essentially never hit this, so assert it explicitly.
	var zero uuid.UUID
	if got := paddedBase62UUID(zero, 0); got != "000000000000000000000000" {
		t.Fatalf("all-zero UUID ordinal 0: got %q, want 24 zeros", got)
	}

	// A max-valued UUID (all 0xFF) is the widest base62 a 128-bit value produces
	// (base62UUIDWidth chars), so the pad adds nothing and the total width holds.
	var maxU uuid.UUID
	for i := range maxU {
		maxU[i] = 0xFF
	}
	if got := paddedBase62UUID(maxU, 0); len(got) != wantLen {
		t.Fatalf("max UUID: got length %d, want %d", len(got), wantLen)
	}

	// A representative UUID: fixed 24-char alphanumeric shape, split into a 22-char
	// padded id and a 2-char padded ordinal.
	id := uuid.MustParse("12345678-90ab-cdef-1234-567890abcdef")
	got := paddedBase62UUID(id, 0)
	if len(got) != wantLen {
		t.Fatalf("representative UUID: got length %d, want %d", len(got), wantLen)
	}
	if !alnum.MatchString(got) {
		t.Fatalf("representative UUID: %q is not alphanumeric", got)
	}
	if suffix := got[base62UUIDWidth:]; suffix != "00" {
		t.Fatalf("ordinal 0 suffix: got %q, want %q", suffix, "00")
	}
	if suffix := paddedBase62UUID(id, 1)[base62UUIDWidth:]; suffix != "01" {
		t.Fatalf("ordinal 1 suffix: got %q, want %q", suffix, "01")
	}

	// Distinct ordinals on the SAME contact produce distinct ids (the second-task
	// uniqueness the spread depends on).
	if a, b := paddedBase62UUID(id, 0), paddedBase62UUID(id, 1); a == b {
		t.Fatalf("ordinals 0 and 1 collided: both %q", a)
	}

	// Documented ordinal domain [0, 62*62): the top of the range still yields a
	// fixed-width alphanumeric id, and out-of-range ordinals panic rather than emit a
	// malformed ('-' or over-width) id.
	if top := paddedBase62UUID(id, 62*62-1); len(top) != wantLen || !alnum.MatchString(top) {
		t.Fatalf("ordinal 62*62-1: got %q (len %d)", top, len(top))
	}
	require.Panics(t, func() { paddedBase62UUID(id, -1) }, "negative ordinal must panic")
	require.Panics(t, func() { paddedBase62UUID(id, 62*62) }, "over-wide ordinal must panic")

	// Injectivity across (contact, ordinal): every combination in a small grid maps
	// to a distinct id, including the padding-boundary low-valued UUID beside others.
	ids := []uuid.UUID{zero, id, maxU, uuid.MustParse("00000000-0000-0000-0000-000000000001")}
	seen := make(map[string]struct{})
	for _, u := range ids {
		for ord := 0; ord < 3; ord++ {
			enc := paddedBase62UUID(u, ord)
			if len(enc) != wantLen {
				t.Fatalf("(%s, %d): got length %d, want %d", u, ord, len(enc), wantLen)
			}
			if !alnum.MatchString(enc) {
				t.Fatalf("(%s, %d): %q is not alphanumeric", u, ord, enc)
			}
			if _, dup := seen[enc]; dup {
				t.Fatalf("(%s, %d) collided on %q", u, ord, enc)
			}
			seen[enc] = struct{}{}
		}
	}
}

// TestSeedVisibleTaskSpread_NoOpBelowMinCatalog is the regression guard for the
// visibleSpreadMinCatalog precondition: below the threshold the spread must be a
// documented no-op (protecting a custom short profile from the fixed-index access).
// The guard returns before touching the harness's DB/generator, so a bare Harness
// suffices; removing the guard makes these calls dereference a nil database and
// panic, failing the test loudly.
func TestSeedVisibleTaskSpread_NoOpBelowMinCatalog(t *testing.T) {
	h := &Harness{}
	for k := 0; k < visibleSpreadMinCatalog; k++ {
		ids := make([]uuid.UUID, k)
		for i := range ids {
			ids[i] = uuid.New()
		}
		res, err := h.SeedVisibleTaskSpread(context.Background(), ids)
		require.NoError(t, err, "no-op for %d ids", k)
		require.Equal(t, SpreadResult{}, res, "spread is a no-op below %d ids (got %d)", visibleSpreadMinCatalog, k)
		require.Nil(t, h.ManualCohortIDs(), "no manual cohort recorded for %d ids", k)
	}
}
