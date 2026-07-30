package factory

import (
	"strings"
	"testing"
	"time"
)

// nameAnchor is a fixed anchor; these tests compare names, never timestamps.
var nameAnchor = time.Date(2024, time.January, 2, 3, 4, 5, 0, time.UTC)

func namesFor(t *testing.T, namespace string, n int) []string {
	t.Helper()
	gen := NewGeneratorAt(DefaultSeed, namespace, nameAnchor)
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, gen.Contact(WithCadence("weekly")).FullName)
	}
	return out
}

// Names are drawn WITH REPLACEMENT from a 16×10 pool, so a small fixture repeats
// a display name often enough to be a flake rather than a curiosity (measured
// ~1.8% over three contacts). A duplicate is not cosmetic: any selector that
// resolves a contact BY NAME breaks on it — Playwright's strict mode fails
// outright on two matching headings — and a manifest keyed by handle becomes
// ambiguous to read.
//
// The namespace below is a REAL collision found by the integration suite: its
// first and third draws are both "Kestrel Dummond".
func TestContact_DisplayNamesAreUniqueWithinANamespace(t *testing.T) {
	names := namesFor(t, "d382da5be-c1", 3)

	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			t.Fatalf("duplicate display name %q in %v", name, names)
		}
		seen[name] = true
	}
	// The disambiguated one carries this contact's sequence number.
	if !strings.HasSuffix(names[2], " 3") {
		t.Fatalf("expected the repeated draw to carry its sequence number, got %q", names[2])
	}
}

// Exhausting the pool must not produce duplicates either — the suffix is the
// sequence number, which is unique within a generator by construction.
func TestContact_DisplayNamesStayUniquePastThePool(t *testing.T) {
	beyondPool := len(syntheticGivenNames)*len(syntheticSurnames) + 25

	seen := map[string]bool{}
	for _, name := range namesFor(t, "pool-exhaustion", beyondPool) {
		if seen[name] {
			t.Fatalf("duplicate display name %q after %d contacts", name, beyondPool)
		}
		seen[name] = true
	}
}

// The disambiguation must be free for the worlds that do NOT collide: it draws
// no extra rng values, so those namespaces are byte-identical to what the
// generator produced before it existed. A namespace whose first two draws differ
// exercises exactly that.
func TestContact_NonCollidingNamesAreUnchanged(t *testing.T) {
	names := namesFor(t, "d382da5be-c2", 3)
	want := []string{
		"synth-d382da5be-c2-Glyph Fakeman",
		"synth-d382da5be-c2-Jovi Mockford",
		"synth-d382da5be-c2-Wren Stubbings",
	}
	for i, name := range names {
		if name != want[i] {
			t.Fatalf("draw %d: got %q, want %q — a non-colliding namespace must be unaffected", i, name, want[i])
		}
	}
}
