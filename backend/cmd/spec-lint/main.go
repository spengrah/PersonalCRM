// spec-lint validates the behavior SSOT corpus (spec/*.yaml) against the
// schema and check list defined in spec/README.md, using the parser/validator
// in internal/spec. Operator/CI tool; not deployed with the production
// service.
//
// Usage:
//
//	spec-lint <spec-directory>
//
// Exactly one positional argument: the directory holding the spec corpus
// (top-level *.yaml files; README.md, subdirectories, and .yml are ignored).
// A directory with no spec files lints clean (the corpus starts empty).
//
// Exit codes:
//
//	0 — corpus is clean
//	1 — violations found (all printed to stdout, aggregated; never fail-fast)
//	2 — operational error (bad usage, unreadable directory) on stderr
package main

import (
	"fmt"
	"io"
	"os"

	"personal-crm/backend/internal/spec"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the linter against the directory named by args. Split from
// main so tests can drive the argument/exit-code contract without a
// subprocess (crm-admin precedent). A stdout write failure is an operational
// error (exit 2): a truncated violation listing must not pass for a clean or
// fully-reported run. The stderr writes are best-effort — those paths already
// return 2, so the exit code carries the signal even if the message is lost.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: spec-lint <spec-directory>")
		return 2
	}
	dir := args[0]

	files, violations, err := spec.Lint(dir)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "spec-lint: %v\n", err)
		return 2
	}

	behaviors := 0
	for _, f := range files {
		behaviors += len(f.Behaviors)
	}

	for _, v := range violations {
		if _, err := fmt.Fprintln(stdout, v.String()); err != nil {
			return 2
		}
	}

	summary := fmt.Sprintf("spec-lint: %d files, %d behaviors — OK\n", len(files), behaviors)
	exit := 0
	switch {
	case len(violations) > 0:
		summary = fmt.Sprintf("spec-lint: %d files, %d behaviors — %d violations\n", len(files), behaviors, len(violations))
		exit = 1
	case len(files) == 0:
		summary = fmt.Sprintf("spec-lint: no spec files found in %s — OK\n", dir)
	}
	if _, err := fmt.Fprint(stdout, summary); err != nil {
		return 2
	}
	return exit
}
