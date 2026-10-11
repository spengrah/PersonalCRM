#!/usr/bin/env bash
# repo-hygiene-check.sh - always-on, lightweight pre-push check.
# A home for cheap "don't commit X" checks that catch mistakes deterministically
# but don't warrant a CI job.
#
# Checks:
#   - No prototype *.html committed at the repo root. Prototypes belong in temp/
#     (gitignored); a stray root-level .html is almost always an accidental commit.
#   - No file-wide `/* eslint-disable` block comment under frontend/. It hides every
#     later violation of that rule in the file; use `eslint-disable-next-line` or
#     `-line` with a reason instead.
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

# eslint_filewide_disable_offenders reads file paths on stdin and prints path:line
# for each file-wide `/* eslint-disable` block comment, with any whitespace ESLint
# accepts between `/*` and the directive, newlines included. `-next-line` and
# `-line` directives have a `-` after "disable", so they never match.
eslint_filewide_disable_offenders() {
  while IFS= read -r file; do
    [ -f "$file" ] || continue
    perl -0777 -ne 'while (m{/\*\s*eslint-disable(?=\s|\*/)}g) { my $line = 1 + (substr($_, 0, $-[0]) =~ tr/\n//); print "$ARGV:$line\n" }' "$file"
  done
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

  # Check: no file-wide `/* eslint-disable` under frontend/.
  offenders="$(cd "$repo_root" && git grep -l -e 'eslint-disable' -- frontend | eslint_filewide_disable_offenders)"
  if [ -n "$offenders" ]; then
    echo "repo-hygiene: file-wide '/* eslint-disable' under frontend/ — use eslint-disable-next-line with a reason:" >&2
    echo "$offenders" | sed 's/^/  - /' >&2
    fail=1
  fi

  return "$fail"
}

# Source-guard: run the gate only when executed directly, not when sourced by tests.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  run_repo_hygiene
fi
