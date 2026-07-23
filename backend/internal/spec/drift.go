package spec

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// This file is the behavior-drift core. SpecDrift compares the assertable
// content (given/when/then/statement) of each behavior between a base corpus
// and the HEAD corpus and warns when a behavior's assertions changed but none
// of its citing test files were touched — the classic silent-drift case. It is
// pure (no git, no IO), so every drift case is table-testable with in-memory
// fixtures; cmd/spec-drift wraps it with the git materialization, changed-file
// diff, and path normalization.

// assertablesEqual reports whether two behaviors carry identical assertable
// content: when, statement, and the given/then lists compared order-sensitively
// (a then-item reorder counts as a change). Title, notes, provenance, type,
// status, surface, serves, and waivers are deliberately excluded — only the
// given/when/then/statement assertions are drift-relevant.
func assertablesEqual(a, b *Behavior) bool {
	return a.When == b.When &&
		a.Statement == b.Statement &&
		slices.Equal(a.Given, b.Given) &&
		slices.Equal(a.Then, b.Then)
}

// SpecDrift compares the assertable content of each HEAD behavior against its
// base counterpart (matched by ID) and returns a warning Violation for every
// behavior whose assertions changed while none of its citing test files were
// touched.
//
// base is the PRESENT base set — the CLI drops behaviors from a base file that
// did not lint clean, so a base fix never reads as drift. head retains its
// *File so a violation can carry the behavior's file path. cites are the
// HEAD-tree citations (paths already normalized to repo-relative by the CLI);
// changedFiles is the set of repo-relative paths that differ between base and
// HEAD.
//
// Warn rule for a behavior whose assertable content changed: resolve its
// distinct citing files; warn only when it has >=1 citing file and none is in
// changedFiles. A zero-citation change is the coverage scanner's orphan job
// (silent here); a newly-added ID (base miss) and a change whose citing file
// was touched are silent. Output is sorted by (Path, Line, Ref).
func SpecDrift(base, head []*File, cites []Citation, changedFiles map[string]bool) []Violation {
	baseByID := map[string]*Behavior{}
	for _, f := range base {
		for i := range f.Behaviors {
			baseByID[f.Behaviors[i].ID] = &f.Behaviors[i]
		}
	}

	citesByID := map[string][]Citation{}
	for _, c := range cites {
		citesByID[c.ID] = append(citesByID[c.ID], c)
	}

	type headEntry struct {
		b *Behavior
		f *File
	}
	var heads []headEntry
	for _, f := range head {
		for i := range f.Behaviors {
			heads = append(heads, headEntry{&f.Behaviors[i], f})
		}
	}
	sort.SliceStable(heads, func(i, j int) bool { return heads[i].b.ID < heads[j].b.ID })

	type pair struct {
		path string
		line int
	}

	var viol []Violation
	for _, h := range heads {
		bb, ok := baseByID[h.b.ID]
		if !ok {
			continue // newly-added ID — nothing to drift from
		}
		if assertablesEqual(bb, h.b) {
			continue // assertions unchanged
		}

		// Assertable content changed: collect distinct citing files and distinct
		// (file, line) citation pairs.
		fileSet := map[string]bool{}
		pairSet := map[pair]bool{}
		for _, c := range citesByID[h.b.ID] {
			fileSet[c.Path] = true
			pairSet[pair{c.Path, c.Line}] = true
		}
		if len(fileSet) == 0 {
			continue // zero-citation change — the coverage scanner's orphan job
		}
		touched := false
		for f := range fileSet {
			if changedFiles[f] {
				touched = true
				break
			}
		}
		if touched {
			continue // a citing file was edited alongside the behavior
		}

		pairs := make([]string, 0, len(pairSet))
		for p := range pairSet {
			pairs = append(pairs, fmt.Sprintf("%s:%d", p.path, p.line))
		}
		sort.Strings(pairs)
		viol = append(viol, Violation{
			Path: h.f.Path,
			Line: h.b.Line,
			Ref:  h.b.ID,
			Msg: fmt.Sprintf("behavior assertions changed but none of its %d citing test file(s) were touched (%s)",
				len(fileSet), strings.Join(pairs, ", ")),
		})
	}

	sort.Slice(viol, func(i, j int) bool {
		if viol[i].Path != viol[j].Path {
			return viol[i].Path < viol[j].Path
		}
		if viol[i].Line != viol[j].Line {
			return viol[i].Line < viol[j].Line
		}
		return viol[i].Ref < viol[j].Ref
	})
	return viol
}
