// spec-coverage is the Piece 3 traceability scanner: it cross-references
// // spec: citations in the deterministic test surfaces (backend *_test.go,
// frontend/tests/e2e *.spec.ts) against the behavior SSOT (spec/*.yaml) and
// reports per-then-item E2E coverage for ui-surface behaviors. Operator/CI
// tool; not deployed with the production service.
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
//	0 — no invalid citations; orphans (if any) are warnings only
//	1 — invalid citations found (dead IDs, bad indexes, intent/proposed/
//	    retired cites, malformed markers) — always fatal — or orphaned
//	    ui-surface then-items in a domain whose spec file declares
//	    e2e_settled: true
//	2 — operational error (bad usage, unreadable tree, corpus lint failure)
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

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

	// Coverage semantics depend on a schema-valid corpus (surface tags, waiver
	// indexes), so a corpus that does not lint clean is an operational error
	// here — spec-lint owns reporting the violations themselves.
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

	cov := spec.ComputeCoverage(files, cites)
	cov.Problems = append(citeProbs, cov.Problems...)

	if err := report(stdout, cov); err != nil {
		return 2
	}

	blocked := false
	for _, d := range cov.Domains {
		if _, _, orphans := d.Counts(); d.Settled && orphans > 0 {
			blocked = true
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

	totalOrphans := 0
	for _, d := range cov.Domains {
		covered, waived, orphans := d.Counts()
		totalOrphans += orphans
		state := "warn"
		if d.Settled {
			state = "settled"
		}
		if err := p("%-18s [%s]  surface ui/api/none: %d/%d/%d  ui then-items: %d covered, %d waived, %d orphaned\n",
			d.Domain, state, d.UI, d.API, d.None, covered, waived, orphans); err != nil {
			return err
		}
		for _, it := range d.Items {
			switch it.State {
			case spec.ItemOrphan:
				marker := "ORPHAN"
				if d.Settled {
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
	if totalOrphans > 0 || len(cov.Problems) > 0 {
		return p("spec-coverage: %d orphaned ui then-items, %d invalid citations\n", totalOrphans, len(cov.Problems))
	}
	return p("spec-coverage: all ui-surface then-items covered or waived\n")
}
