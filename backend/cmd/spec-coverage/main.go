// spec-coverage is the behavior-SSOT traceability scanner: it cross-references
// // spec: citations in the deterministic test surfaces (backend *_test.go,
// frontend/tests/e2e *.spec.ts) against the behavior SSOT (spec/*.yaml) and
// reports per-then-item coverage keyed on surface: ui behaviors via E2E
// citations, api behaviors via Go-test citations. Operator/CI tool; not
// deployed with the production service.
//
// Usage:
//
//	spec-coverage <repo-root>
//
// Exactly one positional argument: the repository root, which must contain
// spec/, backend/, and frontend/tests/e2e/ in the standard layout.
//
// Exit codes:
//
//	0 — no invalid citations, and no orphan on a settled surface. Orphans on
//	    an UNSETTLED surface warn without failing; since every domain
//	    currently declares settled: [ui, api], today that means exit 0
//	    implies zero orphans
//	1 — invalid citations found (dead IDs, a key the behavior does not carry,
//	    the retired positional ID[n] form, a keyed cite of a statement
//	    behavior, the reserved @ suffix, intent/proposed/retired cites,
//	    malformed markers) — always fatal — or orphaned then-items on a
//	    surface that a domain lists in its settled: [...] list
//	2 — operational error (bad usage, unreadable tree, corpus lint failure)
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"personal-crm/backend/internal/spec"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the scanner against the repo root named by args. Split from
// main so tests can drive the argument/exit-code contract without a
// subprocess (spec-lint precedent). A stdout write failure is an operational
// error (exit 2): a truncated report must not pass for a complete one.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: spec-coverage <repo-root>")
		return 2
	}
	root := args[0]
	specDir := filepath.Join(root, "spec")
	backendDir := filepath.Join(root, "backend")
	e2eDir := filepath.Join(root, "frontend", "tests", "e2e")

	// Coverage semantics depend on a schema-valid corpus (surface tags,
	// then-item keys, waiver keys resolving to them), so a corpus that does
	// not lint clean is an operational error here — spec-lint owns reporting
	// the violations.
	files, lintViol, err := spec.Lint(specDir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "spec-coverage: %v\n", err)
		return 2
	}
	if len(lintViol) > 0 {
		_, _ = fmt.Fprintf(stderr, "spec-coverage: corpus has %d lint violations; run make spec-lint\n", len(lintViol))
		return 2
	}

	cites, citeProbs, err := spec.CollectCitations(backendDir, e2eDir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "spec-coverage: %v\n", err)
		return 2
	}

	cov := spec.ComputeCoverage(files, cites, citeProbs)

	if err := report(stdout, cov); err != nil {
		return 2
	}

	blocked := false
	for _, d := range cov.Domains {
		for _, s := range d.Settled {
			if _, _, orphans := d.SurfaceCounts(s); orphans > 0 {
				blocked = true
			}
		}
	}
	if len(cov.Problems) > 0 || blocked {
		return 1
	}
	return 0
}

// report renders the per-domain summary, orphan/waiver detail, and any
// problems/warnings. Any write error aborts (caller exits 2).
func report(w io.Writer, cov *spec.Coverage) error {
	p := func(format string, a ...any) error {
		_, err := fmt.Fprintf(w, format, a...)
		return err
	}

	totalUIO, totalAPIO := 0, 0
	for _, d := range cov.Domains {
		uiC, uiW, uiO := d.SurfaceCounts("ui")
		apiC, apiW, apiO := d.SurfaceCounts("api")
		totalUIO += uiO
		totalAPIO += apiO
		if err := p("%-18s [settled: %s]  surface ui/api/none: %d/%d/%d  ui: %d covered, %d waived, %d orphaned  api: %d covered, %d waived, %d orphaned\n",
			d.Domain, settledLabel(d.Settled), d.UI, d.API, d.None, uiC, uiW, uiO, apiC, apiW, apiO); err != nil {
			return err
		}
		for _, it := range d.Items {
			switch it.State {
			case spec.ItemOrphan:
				marker := "ORPHAN"
				if contains(d.Settled, it.Surface) {
					marker = "ORPHAN (blocking)"
				}
				if err := p("  %s %s: %s\n", marker, it.Ref(), it.Text); err != nil {
					return err
				}
			case spec.ItemWaived:
				if err := p("  waived %s: %s\n", it.Ref(), it.Reason); err != nil {
					return err
				}
			}
		}
	}

	for _, v := range cov.Warnings {
		if err := p("WARNING: %s\n", v.String()); err != nil {
			return err
		}
	}
	for _, v := range cov.Problems {
		if err := p("INVALID: %s\n", v.String()); err != nil {
			return err
		}
	}
	if totalUIO+totalAPIO > 0 || len(cov.Problems) > 0 {
		return p("spec-coverage: %d orphaned then-items (ui %d, api %d), %d invalid citations\n",
			totalUIO+totalAPIO, totalUIO, totalAPIO, len(cov.Problems))
	}
	return p("spec-coverage: all ui- and api-surface then-items covered or waived\n")
}

// settledLabel renders a domain's settled surface list in canonical enum order
// (ui before api), comma-joined, so the byte-stable report has one
// representation per set. An empty list renders "-".
func settledLabel(settled []string) string {
	var parts []string
	for _, s := range []string{"ui", "api"} {
		if contains(settled, s) {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ",")
}

// contains reports whether s appears in list.
func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
