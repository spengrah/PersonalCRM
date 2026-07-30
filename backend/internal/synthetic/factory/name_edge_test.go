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

// The retained wrappers must keep producing exactly what they always produced:
// they are byte-stable generic factory API, so any caller drawing through them
// keeps getting the same names.
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

func TestWithNameTwinOf_RejectsGeneratorNamespaceSiblings(t *testing.T) {
	tests := []struct {
		name     string
		targetNS string
		sourceNS string
	}{
		{
			name:     "hierarchical namespace",
			targetNS: "family",
			sourceNS: "family-child",
		},
		{
			name:     "re-salted namespace",
			targetNS: "family",
			sourceNS: "family-s1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := NewGeneratorAt(DefaultSeed, tt.sourceNS, nameAnchor).Contact()
			target := NewGeneratorAt(DefaultSeed, tt.targetNS, nameAnchor)

			defer func() {
				if recover() == nil {
					t.Fatalf("generator namespace sibling %q was accepted by %q", tt.sourceNS, tt.targetNS)
				}
			}()
			target.Contact(WithNameTwinOf(source))
		})
	}
}

func TestWithNameTwinOf_RejectsAnotherGeneratorWithSameNamespace(t *testing.T) {
	source := NewGeneratorAt(DefaultSeed, "same-ns", nameAnchor).Contact()
	target := NewGeneratorAt(DefaultSeed, "same-ns", nameAnchor)

	defer func() {
		if recover() == nil {
			t.Fatal("a different generator instance with the same namespace was accepted")
		}
	}()
	target.Contact(WithNameTwinOf(source))
}

func TestWithNameTwinOf_RejectsForgedSpec(t *testing.T) {
	gen := NewGeneratorAt(DefaultSeed, "forged-ns", nameAnchor)
	source := gen.Contact()
	forged := ContactSpec{FullName: source.FullName}

	defer func() {
		if recover() == nil {
			t.Fatal("a spec carrying only a generator-shaped full name was accepted")
		}
	}()
	gen.Contact(WithNameTwinOf(forged))
}

func TestWithNameTwinOf_AcceptsSameGeneratorSpecCopy(t *testing.T) {
	gen := NewGeneratorAt(DefaultSeed, "copy-ns", nameAnchor)
	source := gen.Contact()
	sourceCopy := source

	twin := gen.Contact(WithNameTwinOf(sourceCopy))
	if twin.FullName != source.FullName {
		t.Fatalf("twin rendered %q, want copied source name %q", twin.FullName, source.FullName)
	}
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

// --- WithExplicitName -------------------------------------------------------

func TestWithExplicitName_PinsTheRenderedNameVerbatim(t *testing.T) {
	gen := NewGeneratorAt(DefaultSeed, "explicit-ns", nameAnchor)
	spec := gen.Contact(WithExplicitName("Cadence", "Sort Yankee"))

	if want := gen.Prefix() + "Cadence Sort Yankee"; spec.FullName != want {
		t.Fatalf("explicit name rendered %q, want %q", spec.FullName, want)
	}
}

// The pin must not shift the PRNG stream: every contact drawn AFTER an explicit
// name must be byte-identical to what it would have been without one. The
// explicit contact still consumes its sequence number, given name and surname;
// only the rendered result is overridden — the same contract WithNameTwinOf
// carries, verified the same way.
func TestWithExplicitName_PreservesTheStreamForLaterDraws(t *testing.T) {
	const n = 12

	plain := NewGeneratorAt(DefaultSeed, "explicit-stream-ns", nameAnchor)
	_ = plain.Contact() // the draw the explicit name will replace
	var withoutExplicit []string
	for i := 0; i < n; i++ {
		withoutExplicit = append(withoutExplicit, plain.Contact().FullName)
	}

	explicit := NewGeneratorAt(DefaultSeed, "explicit-stream-ns", nameAnchor)
	_ = explicit.Contact(WithExplicitName("Kbd", "Move Alpha"))
	var withExplicit []string
	for i := 0; i < n; i++ {
		withExplicit = append(withExplicit, explicit.Contact().FullName)
	}

	for i := range withoutExplicit {
		if withoutExplicit[i] != withExplicit[i] {
			t.Fatalf("draw %d after the explicit name: got %q, want %q — the pin shifted the stream",
				i, withExplicit[i], withoutExplicit[i])
		}
	}
}

// The dedupe exemption: an explicit name that collides with an EARLIER contact's
// drawn name is NOT auto-disambiguated, because the caller asked for that exact
// literal. Every ordinary draw stays covered by the dedupe.
func TestWithExplicitName_ExemptFromDedupe(t *testing.T) {
	gen := NewGeneratorAt(DefaultSeed, "explicit-dedupe-ns", nameAnchor)
	drawn := gen.Contact()
	base := strings.TrimPrefix(drawn.FullName, gen.Prefix())
	given, sur, ok := strings.Cut(base, " ")
	if !ok {
		t.Fatalf("generated display name %q has no given/surname split", base)
	}

	collider := gen.Contact(WithExplicitName(given, sur))
	if collider.FullName != drawn.FullName {
		t.Fatalf("explicit name rendered %q, want the un-disambiguated %q", collider.FullName, drawn.FullName)
	}
	if collider.Email == drawn.Email {
		t.Error("only the NAME may repeat — a shared email would make it the same person to the matcher")
	}
}

// The identifier derivation, not just the display name: a multi-word literal
// must not put a raw space (or any other invalid character) inside the generated
// email's local part, and a single-word generator-drawn pair must be byte-identical
// to what it always was.
func TestWithExplicitName_ProducesAValidEmailLocalPart(t *testing.T) {
	gen := NewGeneratorAt(DefaultSeed, "explicit-email-ns", nameAnchor)
	spec := gen.Contact(WithExplicitName("Cadence", "Sort Yankee"))

	local, _, ok := strings.Cut(spec.Email, "@")
	if !ok {
		t.Fatalf("generated email %q has no domain", spec.Email)
	}
	if !strings.Contains(local, "cadence.sort-yankee") {
		t.Errorf("email local part %q does not carry the sanitized literal", local)
	}
	for _, r := range local {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
		default:
			t.Errorf("email local part %q carries %q, which an unquoted local part may not hold", local, r)
		}
	}

	// The ordinary path is unchanged: a single-word drawn pair slugs exactly as
	// lowercase(given) + "." + lowercase(surname), as it always did.
	if got, want := slug("Zeta", "Testwell"), "zeta.testwell"; got != want {
		t.Errorf("slug(%q, %q) = %q, want the unchanged %q", "Zeta", "Testwell", got, want)
	}
}

// slug()'s sanitization is a safe no-op only while every pool entry is plain
// alphanumeric ASCII. That has held by accident, not by any enforced constraint:
// a future entry carrying an apostrophe or a hyphen ("O'Malley", "Smith-Jones")
// would silently change the generated email identifier on EVERY synthetic path
// that draws through slug/emailFor, and neither the sanitization spot-check above
// nor any golden-stream test exercises more than a couple of pairs. This turns
// the assumption into one that holds by construction and fails loudly, naming the
// offending entry, the moment it stops being true.
func TestNamePools_AreEmailSafeByConstruction(t *testing.T) {
	pools := map[string][]string{
		"syntheticGivenNames": syntheticGivenNames,
		"syntheticSurnames":   syntheticSurnames,
	}
	for pool, entries := range pools {
		if len(entries) == 0 {
			t.Fatalf("%s is empty", pool)
		}
		for _, entry := range entries {
			if entry == "" {
				t.Errorf("%s carries an empty entry", pool)
				continue
			}
			for _, r := range entry {
				switch {
				case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
				default:
					t.Errorf("%s entry %q carries %q — pool entries must be plain alphanumeric ASCII, "+
						"because slug() only sanitizes and cannot preserve a punctuation-bearing identifier",
						pool, entry, r)
				}
			}
		}
		// No entry may be a PREFIX of another. This is what makes two distinct
		// drawn display names unable to nest: both are rendered as
		// prefix+given+" "+surname, so containment would need one given name to be
		// a prefix of another (with the following space still lining up) or one
		// surname to be a prefix of another. A substring pair breaks any selector
		// that resolves a fixture by name on substring, which is how the E2E specs
		// address them.
		for i, a := range entries {
			for _, b := range entries[i+1:] {
				if strings.HasPrefix(a, b) || strings.HasPrefix(b, a) {
					t.Errorf("%s entries %q and %q — one is a prefix of the other, so two rendered names could nest", pool, a, b)
				}
			}
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
