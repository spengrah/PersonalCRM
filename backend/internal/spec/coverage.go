package spec

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// This file is the Piece 3 traceability scanner: it extracts // spec: citation
// markers from the deterministic test surfaces (Go tests and Playwright E2E
// specs), validates them against the parsed corpus, and derives per-then-item
// coverage for ui-surface behaviors. The CLI wrapper lives in
// cmd/spec-coverage.

// Citation is one parsed reference from a // spec: marker line in a test file.
type Citation struct {
	Path string
	Line int
	ID   string
	Then int  // 0-based then-item index; -1 = bare (whole-behavior) citation
	E2E  bool // from a Playwright E2E spec; only these count toward ui coverage
}

// Ref renders the citation reference as written (ID or ID[n]).
func (c Citation) Ref() string {
	if c.Then < 0 {
		return c.ID
	}
	return fmt.Sprintf("%s[%d]", c.ID, c.Then)
}

// Item coverage states.
const (
	ItemCovered = "covered"
	ItemWaived  = "waived"
	ItemOrphan  = "orphan"
)

// ItemCoverage is the coverage verdict for one then-item of a ui-surface,
// status-current behavior. A statement behavior (invariant) has a single
// implicit item with Then = -1.
type ItemCoverage struct {
	ID     string
	Then   int
	Text   string // the then-item text (or the statement)
	State  string // covered | waived | orphan
	Reason string // waiver reason when waived
}

// Ref renders the item as ID[n] (or the bare ID for a statement behavior).
func (ic ItemCoverage) Ref() string {
	if ic.Then < 0 {
		return ic.ID
	}
	return fmt.Sprintf("%s[%d]", ic.ID, ic.Then)
}

// DomainCoverage is one domain's slice of the coverage report.
type DomainCoverage struct {
	Domain  string
	Prefix  string
	Settled bool // the file's e2e_settled flag: orphans block instead of warn
	UI      int  // behavior counts by surface classification (non-retired)
	API     int
	None    int
	Intents int
	Retired int
	Items   []ItemCoverage // per then-item of ui-surface current behaviors
}

// Counts returns the number of covered, waived, and orphaned items.
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

// citationRef parses one comma-separated reference: <ID> or <ID>[<then-index>].
var citationRef = regexp.MustCompile(`^([A-Z][A-Z0-9]*-\d+)(?:\[(\d+)\])?$`)

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
			continue
		}
		for _, raw := range strings.Split(m[1], ",") {
			ref := strings.TrimSpace(raw)
			if ref == "" {
				continue
			}
			rm := citationRef.FindStringSubmatch(ref)
			if rm == nil {
				probs = append(probs, Violation{Path: path, Line: i + 1,
					Msg: fmt.Sprintf("malformed spec citation %q (want <ID> or <ID>[<then-index>])", ref)})
				continue
			}
			then := -1
			if rm[2] != "" {
				n, err := strconv.Atoi(rm[2])
				if err != nil {
					probs = append(probs, Violation{Path: path, Line: i + 1,
						Msg: fmt.Sprintf("malformed spec citation %q (bad then-index)", ref)})
					continue
				}
				then = n
			}
			cites = append(cites, Citation{Path: path, Line: i + 1, ID: rm[1], Then: then, E2E: e2e})
		}
	}
	return cites, probs, nil
}

// ComputeCoverage validates every citation against the parsed corpus and
// derives per-then-item coverage for ui-surface, status-current behaviors.
// Invalid citations (unknown ID, out-of-range index, cites of intent /
// proposed / retired behaviors) land in Problems and never count toward
// coverage. citeProblems from CollectCitations should be appended by the
// caller — they are the same failure class.
func ComputeCoverage(files []*File, cites []Citation) *Coverage {
	cov := &Coverage{}

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

	// Validate citations; index the valid E2E ones for the coverage pass.
	e2eBare := map[string]bool{}         // ID cited bare by an E2E spec
	e2eItem := map[string]map[int]bool{} // ID -> then indexes cited by E2E specs
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
		if c.Then >= 0 {
			if b.Statement != "" {
				cov.Problems = append(cov.Problems, Violation{Path: c.Path, Line: c.Line,
					Msg: fmt.Sprintf("citation %s indexes a statement behavior (no then items)", c.Ref())})
				continue
			}
			if c.Then >= len(b.Then) {
				cov.Problems = append(cov.Problems, Violation{Path: c.Path, Line: c.Line,
					Msg: fmt.Sprintf("citation %s is out of range (%d then items)", c.Ref(), len(b.Then))})
				continue
			}
		}
		if c.E2E {
			if b.Surface != "ui" {
				cov.Warnings = append(cov.Warnings, Violation{Path: c.Path, Line: c.Line,
					Msg: fmt.Sprintf("E2E citation %s names a %s-surface behavior (should this be surface: ui?)", c.Ref(), b.Surface)})
			}
			if c.Then < 0 {
				e2eBare[c.ID] = true
			} else {
				if e2eItem[c.ID] == nil {
					e2eItem[c.ID] = map[int]bool{}
				}
				e2eItem[c.ID][c.Then] = true
			}
		}
	}

	// Per-domain coverage over ui-surface current behaviors.
	sorted := append([]*File(nil), files...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Domain < sorted[j].Domain })
	for _, f := range sorted {
		dc := DomainCoverage{Domain: f.Domain, Prefix: f.Prefix, Settled: f.E2ESettled}
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
			if b.Surface != "ui" || b.Status != "current" {
				continue
			}
			waived := map[int]string{}
			for _, w := range b.Waivers {
				waived[w.Then] = w.Reason
			}
			if b.Statement != "" {
				// A statement behavior has one implicit item, coverable only
				// by a bare citation.
				dc.Items = append(dc.Items, itemState(b.ID, -1, b.Statement, e2eBare[b.ID], "", cov))
				continue
			}
			for n, text := range b.Then {
				covered := e2eBare[b.ID] || e2eItem[b.ID][n]
				dc.Items = append(dc.Items, itemState(b.ID, n, text, covered, waived[n], cov))
			}
		}
		cov.Domains = append(cov.Domains, dc)
	}
	return cov
}

// itemState resolves one item's verdict, recording a stale-waiver warning when
// an item is both cited and waived (the waiver has been overtaken by a test).
func itemState(id string, then int, text string, covered bool, waiverReason string, cov *Coverage) ItemCoverage {
	ic := ItemCoverage{ID: id, Then: then, Text: text}
	switch {
	case covered && waiverReason != "":
		ic.State = ItemCovered
		cov.Warnings = append(cov.Warnings, Violation{Ref: id,
			Msg: fmt.Sprintf("stale waiver: %s is waived but cited by an E2E test — drop the waiver", ic.Ref())})
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
