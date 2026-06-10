#!/usr/bin/env bash
# repo-hygiene-check.sh — lightweight repo-hygiene gate for the pre-push
# CONCURRENT lane (DB-free, port-free). A home for cheap "don't commit X" checks
# that catch mistakes deterministically but don't warrant a CI job.
#
# Checks:
#   - No prototype *.html committed at the repo root. Prototypes belong in temp/
#     (gitignored); a stray root-level .html is almost always an accidental commit.
#
# Sourceable: when sourced (BASH_SOURCE != $0) it only defines functions, so the
# logic can be unit-tested with injected input (see test/test-repo-hygiene-check.sh).
set -uo pipefail

# root_html_offenders reads a newline-separated file list on stdin and prints the
# entries that are HTML files at the repo root (path has no slash). Pure: no git,
# no filesystem — so it is trivially testable.
root_html_offenders() {
  grep -E '^[^/]+\.html$' || true
}

run_repo_hygiene() {
  local repo_root fail=0 offenders
  repo_root="$(git rev-parse --show-toplevel)"

  # Check: no tracked *.html at the repo root.
  offenders="$(git -C "$repo_root" ls-files -- '*.html' | root_html_offenders)"
  if [ -n "$offenders" ]; then
    echo "repo-hygiene: prototype HTML committed at repo root — move to temp/ (gitignored):" >&2
    echo "$offenders" | sed 's/^/  - /' >&2
    fail=1
  fi

  return "$fail"
}

# Source-guard: run the gate only when executed directly, not when sourced by tests.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  run_repo_hygiene
fi
