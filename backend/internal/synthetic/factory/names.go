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

// phoneFor returns the next synthetic phone within this namespace's DISJOINT
// 1000-value phone sub-block: +1-555-<bucket7>-<index3>. The 555 exchange keeps
// it obviously fictional; the per-namespace 1e7-wide hash bucket guarantees two
// namespaces never share a phone value (so identity matching, which keys on the
// exact normalized value DB-wide, can never cross namespaces). Panics if the
// 1000-id sub-block is exhausted — far beyond any realistic per-namespace count.
func (g *Generator) phoneFor() string {
	if g.phoneSeq >= phoneBucketWidth {
		panic(fmt.Sprintf("synthetic: phone sub-block exhausted for namespace %q", g.namespace))
	}
	bucket := g.nsPhoneBucket / phoneBucketWidth
	idx := g.phoneSeq
	g.phoneSeq++
	return fmt.Sprintf("+1-555-%07d-%03d", bucket, idx)
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
