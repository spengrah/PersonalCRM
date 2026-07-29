package factory

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// maxStoredNameRunes is the contact API's own full_name limit (the create/update
// DTOs validate max=255, and go-playground/validator counts RUNES for a string).
// A generated name longer than this is a state the product itself refuses, so
// seeding one would break the toolkit's honesty rule rather than test anything.
const maxStoredNameRunes = 255

// worstCaseNamespace is the longest EFFECTIVE namespace token the seeding
// grammar can produce (60 characters). The requested-namespace grammar caps at
// 57 and reserves the rest for the -sN re-salt suffix, so this is the widest
// prefix a stored name can ever carry.
var worstCaseNamespace = strings.Repeat("n", 60)

func TestNameEdge_RenderedNamesStayInsideTheStoredLimit(t *testing.T) {
	for _, kind := range NameEdgeKinds() {
		gen := NewGeneratorAt(DefaultSeed, worstCaseNamespace, nameAnchor)
		// Draw enough contacts that some collide and pick up the disambiguation
		// suffix, so the bound is checked on the LONGEST shape the generator can
		// actually emit, not just the first draw.
		for i := 0; i < 200; i++ {
			name := gen.Contact(WithNameEdge(kind)).FullName
			if n := utf8.RuneCountInString(name); n > maxStoredNameRunes {
				t.Fatalf("name edge %q draw %d renders %d runes, over the %d-rune API limit: %q",
					kind, i, n, maxStoredNameRunes, name)
			}
		}
	}
}

func TestNameEdge_EachKindInjectsItsToken(t *testing.T) {
	for _, kind := range NameEdgeKinds() {
		token, ok := NameEdgeToken(kind)
		if !ok {
			t.Fatalf("NameEdgeToken(%q) reports unknown", kind)
		}
		gen := NewGeneratorAt(DefaultSeed, "edge-ns", nameAnchor)
		name := gen.Contact(WithNameEdge(kind)).FullName
		if !strings.Contains(name, token) {
			t.Errorf("name edge %q rendered %q, which does not carry its token %q", kind, name, token)
		}
	}
}

// The long edge has to be long enough to be a truncation hazard at all —
// otherwise it is a differently-spelled ordinary name.
func TestNameEdge_LongIsActuallyLong(t *testing.T) {
	token, _ := NameEdgeToken(NameEdgeLong)
	if n := utf8.RuneCountInString(token); n < 100 {
		t.Errorf("the long name token is %d runes, too short to exercise truncation", n)
	}
}

// The RTL and emoji edges must carry non-ASCII code points, which is the whole
// hazard: a naive byte or UTF-16 truncation splits them.
func TestNameEdge_RTLAndEmojiAreNonASCII(t *testing.T) {
	for _, kind := range []NameEdge{NameEdgeRTL, NameEdgeEmoji} {
		token, _ := NameEdgeToken(kind)
		if utf8.RuneCountInString(token) == len(token) {
			t.Errorf("name edge %q token %q is pure ASCII — it exercises nothing", kind, token)
		}
	}
}

func TestWithNameEdge_PanicsOnAnUnknownKind(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("WithNameEdge on an unknown kind must panic — an ordinary name would silently stop testing the edge")
		}
	}()
	WithNameEdge(NameEdge("no-such-edge"))
}

// The retained wrappers must keep producing exactly what they always produced,
// so the catalog profiles are untouched by the refactor.
func TestNameEdge_LegacyWrappersAreUnchanged(t *testing.T) {
	gen := NewGeneratorAt(DefaultSeed, "legacy-ns", nameAnchor)
	uni := gen.Contact(WithUnicodeName()).FullName
	desc := gen.Contact(WithDescenderName()).FullName
	if !strings.Contains(uni, "Ünïcödé-") {
		t.Errorf("WithUnicodeName rendered %q", uni)
	}
	if !strings.Contains(desc, "Gregory-") {
		t.Errorf("WithDescenderName rendered %q", desc)
	}
}

// --- WithNameTwinOf ---------------------------------------------------------

// The twin option is the ONLY route to a duplicate display name. Every other
// path stays covered by the dedupe, which the existing uniqueness tests pin.
func TestWithNameTwinOf_IsTheOnlyRouteToADuplicate(t *testing.T) {
	gen := NewGeneratorAt(DefaultSeed, "twin-ns", nameAnchor)
	source := gen.Contact()
	twin := gen.Contact(WithNameTwinOf(source))

	if twin.FullName != source.FullName {
		t.Fatalf("twin rendered %q, want the source's %q", twin.FullName, source.FullName)
	}
	if twin.Email == source.Email {
		t.Error("the twin must share only the NAME — a shared email would make it the same person to the matcher")
	}
}

func TestWithNameTwinOf_RejectsAnotherNamespace(t *testing.T) {
	source := NewGeneratorAt(DefaultSeed, "source-ns", nameAnchor).Contact()
	target := NewGeneratorAt(DefaultSeed, "target-ns", nameAnchor)

	defer func() {
		if recover() == nil {
			t.Fatal("a cross-namespace twin would be invisible to the target namespace's cleanup")
		}
	}()
	target.Contact(WithNameTwinOf(source))
}

// The bypass must not shift the PRNG stream: every contact drawn AFTER a twin
// must be byte-identical to what it would have been without one. The twin still
// consumes its sequence number, given name and surname; only the rendered result
// is overridden.
func TestWithNameTwinOf_PreservesTheStreamForLaterDraws(t *testing.T) {
	const n = 12

	plain := NewGeneratorAt(DefaultSeed, "stream-ns", nameAnchor)
	source := plain.Contact()
	_ = plain.Contact() // the draw the twin will replace
	var withoutTwin []string
	for i := 0; i < n; i++ {
		withoutTwin = append(withoutTwin, plain.Contact().FullName)
	}

	twinned := NewGeneratorAt(DefaultSeed, "stream-ns", nameAnchor)
	twinSource := twinned.Contact()
	_ = twinned.Contact(WithNameTwinOf(twinSource))
	var withTwin []string
	for i := 0; i < n; i++ {
		withTwin = append(withTwin, twinned.Contact().FullName)
	}

	if source.FullName != twinSource.FullName {
		t.Fatalf("the two runs diverged before the twin: %q vs %q", source.FullName, twinSource.FullName)
	}
	for i := range withoutTwin {
		if withoutTwin[i] != withTwin[i] {
			t.Fatalf("draw %d after the twin: got %q, want %q — the bypass shifted the stream",
				i, withTwin[i], withoutTwin[i])
		}
	}
}

// Everything except the deliberate pair must still be unique: the twin's name is
// recorded in the dedupe set, so a later ordinary draw landing on the same base
// is disambiguated exactly as before.
func TestWithNameTwinOf_LeavesEveryOtherNameUnique(t *testing.T) {
	gen := NewGeneratorAt(DefaultSeed, "twin-uniq-ns", nameAnchor)
	source := gen.Contact()
	twin := gen.Contact(WithNameTwinOf(source))

	if twin.FullName != source.FullName {
		t.Fatalf("the twin did not copy the source name: %q vs %q", twin.FullName, source.FullName)
	}
	// The deliberate pair, and nothing else, may repeat.
	counts := map[string]int{source.FullName: 2}

	for i := 0; i < 200; i++ {
		counts[gen.Contact().FullName]++
	}
	for name, n := range counts {
		want := 1
		if name == source.FullName {
			want = 2 // source + twin
		}
		if n != want {
			t.Errorf("name %q appears %d times, want %d", name, n, want)
		}
	}
}
