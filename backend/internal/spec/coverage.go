package spec

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// This file is the traceability scanner: it extracts // spec: citation
// markers from the deterministic test surfaces (Go tests and Playwright E2E
// specs), validates them against the parsed corpus, and derives per-then-item
// coverage keyed on surface — ui behaviors via E2E citations, api behaviors
// via Go-test citations. The CLI wrapper lives in cmd/spec-coverage.

// refString renders a coverage item's reference: ID.key when the item carries a
// key, the bare ID for then < 0 (a statement behavior's implicit item), else
// ID[n].
//
// Having a key and being cited are INDEPENDENT: a key is minted once and never
// withdrawn, so an item whose citation was deleted or left dangling is orphaned
// while still rendering as ID.key — and that reference is a citable handle an
// author can paste straight back into a marker. Only an item that has never
// been cited or waived falls through to ID[n], which is a LOCATION rather than
// a handle: the positional form is no longer accepted in a // spec: marker, so
// that author mints the key first (see the authoring recipe in
// spec/README.md).
func refString(id string, then int, key string) string {
	switch {
	case key != "":
		return id + "." + key
	case then < 0:
		return id
	default:
		return fmt.Sprintf("%s[%d]", id, then)
	}
}

// Citation is one parsed reference from a // spec: marker line in a test file.
type Citation struct {
	Path string
	Line int
	ID   string
	Key  string // then-item key; empty for a bare (whole-behavior) citation
	E2E  bool   // from a Playwright E2E spec; E2E cites credit ui, Go cites credit api
}

// Ref renders the citation reference as written (ID or ID.key). A citation
// carries a key or it is bare — there is no third form.
func (c Citation) Ref() string {
	if c.Key == "" {
		return c.ID
	}
	return c.ID + "." + c.Key
}

// Item coverage states.
const (
	ItemCovered = "covered"
	ItemWaived  = "waived"
	ItemOrphan  = "orphan"
)

// ItemCoverage is the coverage verdict for one then-item of a ui- or
// api-surface, status-current behavior. A statement behavior (invariant) has a
// single implicit item with Then = -1.
type ItemCoverage struct {
	ID      string
	Key     string // the item's then-item key; empty when it carries none
	Then    int
	Text    string // the then-item text (or the statement)
	State   string // covered | waived | orphan
	Reason  string // waiver reason when waived
	Surface string // the owning behavior's surface: ui | api
}

// Ref renders the item as ID.key when it has a key, ID[n] when it does not, and
// the bare ID for a statement behavior's implicit item. Orphan lines therefore
// read either way: ID.key for an item that carries a key but nothing currently
// cites, ID[n] for one that has never been referenced at all. See refString for
// why the difference matters to the author reading the report.
func (ic ItemCoverage) Ref() string { return refString(ic.ID, ic.Then, ic.Key) }

// DomainCoverage is one domain's slice of the coverage report.
type DomainCoverage struct {
	Domain  string
	Prefix  string
	Settled []string // the file's settled surfaces: their orphans block instead of warn
	UI      int      // behavior counts by surface classification (non-retired)
	API     int
	None    int
	Intents int
	Retired int
	Items   []ItemCoverage // per then-item of ui- or api-surface current behaviors
}

// Counts returns the number of covered, waived, and orphaned items across all
// surfaces.
func (d DomainCoverage) Counts() (covered, waived, orphans int) {
	for _, it := range d.Items {
		switch it.State {
		case ItemCovered:
			covered++
		case ItemWaived:
			waived++
		case ItemOrphan:
			orphans++
		}
	}
	return
}

// SurfaceCounts returns the covered/waived/orphaned item counts for one
// surface (ui or api), so the two orphan populations are counted and gated
// independently.
func (d DomainCoverage) SurfaceCounts(surface string) (covered, waived, orphans int) {
	for _, it := range d.Items {
		if it.Surface != surface {
			continue
		}
		switch it.State {
		case ItemCovered:
			covered++
		case ItemWaived:
			waived++
		case ItemOrphan:
			orphans++
		}
	}
	return
}

// Coverage is the full scanner result.
type Coverage struct {
	Domains  []DomainCoverage
	Problems []Violation // invalid citations — always a failure (rotted references)
	Warnings []Violation // non-fatal signals (stale waivers, E2E cites of non-ui behaviors)
}

// citationLine matches a whole-line // spec: marker. The marker must be the
// only content on its line (the repo convention) — this deliberately does not
// match prose that merely mentions a marker (e.g. "(see `// spec: CON-038`)").
var citationLine = regexp.MustCompile(`^\s*// spec: (.+?)\s*$`)

// citationRef parses one comma-separated reference: <ID> or
// <ID>.<then-item-key>. The key alternative is built from thenKeyCharset, the
// single definition it shares with validate.go's thenKeyRegex.
var citationRef = regexp.MustCompile(`^([A-Z][A-Z0-9]*-\d+)(?:\.(` + thenKeyCharset + `))?$`)

// legacyIndexedRef RECOGNIZES the retired positional form; it never parses it.
// It exists only so a stale ID[n] gets a targeted "cite by key" message instead
// of degrading into the generic malformed one — the same reason the @
// reservation is checked before the grammar.
var legacyIndexedRef = regexp.MustCompile(`^[A-Z][A-Z0-9]*-\d+\[\d+\]$`)

// CollectCitations walks backendDir for *_test.go files and e2eDir for
// *.spec.ts files, extracting every // spec: marker. Malformed references are
// returned as violations (they are rotted pointers, same class as a dead ID).
func CollectCitations(backendDir, e2eDir string) ([]Citation, []Violation, error) {
	var cites []Citation
	var probs []Violation

	collect := func(root, suffix string, e2e bool) error {
		return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				// testdata trees hold fixture files (including this
				// scanner's own), never real citations — same exclusion the
				// Go toolchain applies.
				if d.Name() == "node_modules" || d.Name() == ".git" || d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(d.Name(), suffix) {
				return nil
			}
			fileCites, fileProbs, err := scanFile(path, e2e)
			if err != nil {
				return err
			}
			cites = append(cites, fileCites...)
			probs = append(probs, fileProbs...)
			return nil
		})
	}

	if err := collect(backendDir, "_test.go", false); err != nil {
		return nil, nil, fmt.Errorf("scan backend tests: %w", err)
	}
	if err := collect(e2eDir, ".spec.ts", true); err != nil {
		return nil, nil, fmt.Errorf("scan e2e specs: %w", err)
	}
	return cites, probs, nil
}

func scanFile(path string, e2e bool) ([]Citation, []Violation, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	var cites []Citation
	var probs []Violation
	for i, line := range strings.Split(string(data), "\n") {
		m := citationLine.FindStringSubmatch(line)
		if m == nil {
			// A marker that is not the only content on its line (e.g.
			// trailing an assertion) would otherwise be silently invisible —
			// neither counted as coverage nor validated for deadness. Fail
			// loudly instead so the author moves it to its own line.
			if strings.Contains(line, "// spec:") {
				probs = append(probs, Violation{Path: path, Line: i + 1,
					Msg: "spec citation marker must be the only content on its line"})
			}
			continue
		}
		for _, raw := range strings.Split(m[1], ",") {
			ref := strings.TrimSpace(raw)
			if ref == "" {
				continue
			}
			// @ is RESERVED for a future content-hash suffix and is checked
			// before the grammar, so a citation that carries one gets a
			// forward-looking message instead of degrading into the generic
			// malformed one. Nothing may squat the character meanwhile.
			if strings.Contains(ref, "@") {
				probs = append(probs, Violation{Path: path, Line: i + 1,
					Msg: fmt.Sprintf("spec citation %q uses the reserved @hash suffix, which is not yet supported", ref)})
				continue
			}
			// The retired positional form is recognized before the grammar so
			// a stale ID[n] is told what to do instead of being called
			// malformed — the reference names a real behavior and a real item,
			// it is only addressed the one way that can silently re-point.
			if legacyIndexedRef.MatchString(ref) {
				probs = append(probs, Violation{Path: path, Line: i + 1,
					Msg: fmt.Sprintf("spec citation %q uses the retired positional form; cite the then-item by key (<ID>.<then-item-key>)", ref)})
				continue
			}
			rm := citationRef.FindStringSubmatch(ref)
			if rm == nil {
				probs = append(probs, Violation{Path: path, Line: i + 1,
					Msg: fmt.Sprintf("malformed spec citation %q (want <ID> or <ID>.<then-item-key>)", ref)})
				continue
			}
			cites = append(cites, Citation{Path: path, Line: i + 1, ID: rm[1], Key: rm[2], E2E: e2e})
		}
	}
	return cites, probs, nil
}

// hasThenKey reports whether the behavior carries a then item with this key.
func hasThenKey(b *Behavior, key string) bool {
	for _, it := range b.Then {
		if it.Key != "" && it.Key == key {
			return true
		}
	}
	return false
}

// availableKeys renders the behavior's then-item keys for a dangling-citation
// message, in ITEM order so the output mirrors the YAML. Listing the candidates
// is what turns "this key is wrong" into a fix; a behavior with no keyed items
// says so rather than rendering an empty list.
func availableKeys(b *Behavior) string {
	var keys []string
	for _, it := range b.Then {
		if it.Key != "" {
			keys = append(keys, it.Key)
		}
	}
	if len(keys) == 0 {
		return "behavior has no keyed then items"
	}
	return "behavior has: " + strings.Join(keys, ", ")
}

// ComputeCoverage validates every citation against the parsed corpus and
// derives per-then-item coverage for status-current behaviors keyed on
// surface: ui behaviors are credited by E2E citations, api behaviors by
// Go-test citations. Invalid citations (unknown ID, a key the behavior does not
// carry, a keyed cite of a statement behavior, cites of intent / proposed /
// retired behaviors) land in Problems and never count
// toward coverage. citeProblems are CollectCitations' marker-level findings —
// the same failure class, folded into Problems here so no caller can drop them.
func ComputeCoverage(files []*File, cites []Citation, citeProblems []Violation) *Coverage {
	cov := &Coverage{Problems: append([]Violation(nil), citeProblems...)}

	type behaviorRef struct {
		b *Behavior
		f *File
	}
	byID := map[string]behaviorRef{}
	for _, f := range files {
		for i := range f.Behaviors {
			byID[f.Behaviors[i].ID] = behaviorRef{&f.Behaviors[i], f}
		}
	}

	// Validate citations; index the valid ones for the coverage pass, keyed on
	// harness: E2E cites feed ui coverage, Go cites feed api coverage.
	e2eBare := map[string]bool{}           // ID cited bare by an E2E spec
	e2eKey := map[string]map[string]bool{} // ID -> then-item keys cited by E2E specs
	goBare := map[string]bool{}            // ID cited bare by a Go test
	goKey := map[string]map[string]bool{}  // ID -> then-item keys cited by Go tests
	markKey := func(m map[string]map[string]bool, id, key string) {
		if m[id] == nil {
			m[id] = map[string]bool{}
		}
		m[id][key] = true
	}
	for _, c := range cites {
		ref, ok := byID[c.ID]
		if !ok {
			cov.Problems = append(cov.Problems, Violation{Path: c.Path, Line: c.Line,
				Msg: fmt.Sprintf("citation %s names an unknown behavior ID", c.Ref())})
			continue
		}
		b := ref.b
		switch {
		case b.Type == "intent":
			cov.Problems = append(cov.Problems, Violation{Path: c.Path, Line: c.Line,
				Msg: fmt.Sprintf("citation %s names an intent behavior (intents are judge-only)", c.Ref())})
			continue
		case b.Status == "retired":
			cov.Problems = append(cov.Problems, Violation{Path: c.Path, Line: c.Line,
				Msg: fmt.Sprintf("citation %s names a retired behavior", c.Ref())})
			continue
		case b.Status == "proposed":
			cov.Problems = append(cov.Problems, Violation{Path: c.Path, Line: c.Line,
				Msg: fmt.Sprintf("citation %s names a proposed behavior (a citation asserts the behavior holds today)", c.Ref())})
			continue
		}
		if c.Key != "" {
			if b.Statement != "" {
				cov.Problems = append(cov.Problems, Violation{Path: c.Path, Line: c.Line,
					Msg: fmt.Sprintf("citation %s names a then-item key on a statement behavior (no then items)", c.Ref())})
				continue
			}
			if !hasThenKey(b, c.Key) {
				cov.Problems = append(cov.Problems, Violation{Path: c.Path, Line: c.Line,
					Msg: fmt.Sprintf("citation %s names an unknown then-item key (%s)", c.Ref(), availableKeys(b))})
				continue
			}
		}
		if c.E2E {
			if b.Surface != "ui" {
				cov.Warnings = append(cov.Warnings, Violation{Path: c.Path, Line: c.Line,
					Msg: fmt.Sprintf("E2E citation %s names a %s-surface behavior (should this be surface: ui?)", c.Ref(), b.Surface)})
			}
			if c.Key != "" {
				markKey(e2eKey, c.ID, c.Key)
			} else {
				e2eBare[c.ID] = true
			}
		} else {
			// Go cites credit api coverage. There is deliberately NO
			// Go-cites-non-api warning (the mirror of the E2E-cites-non-ui
			// warning): Go tests legitimately cite ui and none behaviors all
			// over the tree, and the none cites are the future none-coverage
			// corpus.
			if c.Key != "" {
				markKey(goKey, c.ID, c.Key)
			} else {
				goBare[c.ID] = true
			}
		}
	}

	// Per-domain coverage over ui- and api-surface current behaviors. Path is
	// the tie-breaker: domain uniqueness is not linted, and an unstable sort on
	// equal keys would make the report order input-dependent.
	sorted := append([]*File(nil), files...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Domain != sorted[j].Domain {
			return sorted[i].Domain < sorted[j].Domain
		}
		return sorted[i].Path < sorted[j].Path
	})
	for _, f := range sorted {
		dc := DomainCoverage{Domain: f.Domain, Prefix: f.Prefix, Settled: append([]string(nil), f.Settled...)}
		for i := range f.Behaviors {
			b := &f.Behaviors[i]
			switch {
			case b.Status == "retired":
				dc.Retired++
				continue
			case b.Type == "intent":
				dc.Intents++
				continue
			}
			switch b.Surface {
			case "ui":
				dc.UI++
			case "api":
				dc.API++
			case "none":
				dc.None++
			}
			if (b.Surface != "ui" && b.Surface != "api") || b.Status != "current" {
				continue
			}
			// Coverage credit is keyed on surface: ui behaviors read the E2E
			// citation maps, api behaviors read the Go-test maps. The two paths
			// are otherwise identical (bare covers all items, a keyed cite
			// covers the item carrying that key, a statement is one implicit
			// item coverable bare).
			bareMap, keyMap, citeKind := e2eBare, e2eKey, "E2E"
			if b.Surface == "api" {
				bareMap, keyMap, citeKind = goBare, goKey, "Go"
			}
			// Waivers are resolved to an item index here so the rest of the
			// pass stays index-keyed. An unresolvable waiver waives nothing
			// (spec-lint reports it).
			waived := map[int]string{}
			for _, w := range b.Waivers {
				if n, ok := waiverItemIndex(b, w); ok {
					waived[n] = w.Reason
				}
			}
			if b.Statement != "" {
				// A statement behavior has one implicit item, coverable only
				// by a bare citation and waivable as index 0.
				dc.Items = append(dc.Items, itemState(f.Path, b.ID, "", -1, b.Statement, b.Surface, citeKind, bareMap[b.ID], waived[0], cov))
				continue
			}
			for n, item := range b.Then {
				covered := bareMap[b.ID] || (item.Key != "" && keyMap[b.ID][item.Key])
				dc.Items = append(dc.Items, itemState(f.Path, b.ID, item.Key, n, item.Text, b.Surface, citeKind, covered, waived[n], cov))
			}
		}
		cov.Domains = append(cov.Domains, dc)
	}
	return cov
}

// itemState resolves one item's verdict, recording a stale-waiver warning when
// an item is both cited and waived (the waiver has been overtaken by a test).
// The stale-waiver message names the citing harness by citeKind ("E2E" → "an
// E2E test", "Go" → "a Go test") so the ui path stays byte-identical to the
// original message while the api path reads correctly.
func itemState(path, id, key string, then int, text, surface, citeKind string, covered bool, waiverReason string, cov *Coverage) ItemCoverage {
	ic := ItemCoverage{ID: id, Key: key, Then: then, Text: text, Surface: surface}
	switch {
	case covered && waiverReason != "":
		ic.State = ItemCovered
		phrase := "an E2E test"
		if citeKind == "Go" {
			phrase = "a Go test"
		}
		cov.Warnings = append(cov.Warnings, Violation{Path: path, Ref: ic.Ref(),
			Msg: fmt.Sprintf("stale waiver: the item is waived but cited by %s — drop the waiver", phrase)})
	case covered:
		ic.State = ItemCovered
	case waiverReason != "":
		ic.State = ItemWaived
		ic.Reason = waiverReason
	default:
		ic.State = ItemOrphan
	}
	return ic
}
