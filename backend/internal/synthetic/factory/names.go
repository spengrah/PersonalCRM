package factory

import (
	"fmt"
	"strings"
)

// Curated, OBVIOUSLY-synthetic name components. None resemble real contacts;
// the surnames are coined ("Testwell", "Synthington", "Fakeman", ...). Combined
// with the namespace prefix on the persisted full_name, no generated contact can
// be mistaken for a real person.
var (
	syntheticGivenNames = []string{
		"Zeta", "Quux", "Nyx", "Vex", "Onyx", "Wren", "Glyph", "Plex",
		"Cyon", "Brux", "Dax", "Fenn", "Jovi", "Kestrel", "Lumen", "Mox",
	}
	syntheticSurnames = []string{
		"Testwell", "Synthington", "Fakeman", "Mockford", "Stubbings",
		"Placeholdt", "Fixtura", "Sampleby", "Dummond", "Probeworth",
	}
)

// givenName picks a deterministic given name component.
func (g *Generator) givenName() string {
	return syntheticGivenNames[g.rng.IntN(len(syntheticGivenNames))]
}

// surname picks a deterministic surname component.
func (g *Generator) surname() string {
	return syntheticSurnames[g.rng.IntN(len(syntheticSurnames))]
}

// slug builds a lowercase, hyphen-free token from a given+surname pair, suitable
// for an email local-part or handle segment.
func slug(given, surname string) string {
	return strings.ToLower(given) + "." + strings.ToLower(surname)
}

// emailFor builds a namespace-prefixed email on the RFC-2606 reserved .example
// TLD (guaranteed un-routable). Shape: <ns>-<slug>-<n>@synthetic.example.
func (g *Generator) emailFor(given, surname string, n int) string {
	return fmt.Sprintf("%s%s-%d@synthetic.example", g.Prefix(), slug(given, surname), n)
}

// telegramHandle builds a namespace-prefixed handle: synth_<ns>_<n>.
func (g *Generator) telegramHandle(n int) string {
	return fmt.Sprintf("synth_%s_%d", sanitizeHandle(g.namespace), n)
}

// phoneFor returns the next synthetic phone for this namespace as a VALID 10-digit
// NANP number in the reserved 555-01XX fictional range: +1-<area>-555-01<idx2>
// (e.g. +1-204-555-0107). The per-namespace area code makes each namespace's 100
// numbers disjoint from every other namespace's (so identity matching, which keys
// on the exact normalized value DB-wide, can never cross namespaces); the
// 555-01XX line keeps every number obviously fictional. Panics if the 100-number
// block is exhausted — ample for tests.
func (g *Generator) phoneFor() string {
	if g.phoneSeq >= phoneLinesPerNS {
		panic(fmt.Sprintf("synthetic: phone block (555-01XX) exhausted for namespace %q", g.namespace))
	}
	idx := g.phoneSeq
	g.phoneSeq++
	return fmt.Sprintf("+1-%d-555-01%02d", g.nsPhoneArea, idx)
}

// sanitizeHandle strips characters not allowed in a telegram-style handle.
func sanitizeHandle(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
