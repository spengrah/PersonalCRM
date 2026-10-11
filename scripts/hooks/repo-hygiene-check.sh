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
#   - No history-narrating comments in Go, TS/TSX, Swift or SQL source. A comment
#     states current behavior or the reason for it. Delivery ids ("PR 7", "DD 3",
#     "arc §4", "pre-cutover", "Codex P1", "retired with") name a change that git
#     already records, and they rot. Spec ids such as CON-001 never match.
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

# A comment marker followed later on the same line by a delivery id (ERE, matched
# case-insensitively).
NB='(^|[^[:alnum:]_])'
HISTORY_COMMENT_RE="(//|/\\*|^[[:space:]]*\\*|--).*((pre|post)-?PR-?[[:space:]]*#?[0-9]|${NB}PR[[:space:]]*#?[0-9]|${NB}DD[[:space:]]?[0-9]|${NB}arc[[:space:]]*§|${NB}(pre|post)-cutover([^[:alnum:]_]|\$)|${NB}Codex P[0-9]|${NB}retired (with|in)([^[:alnum:]_]|\$))"

# history_comment_offenders prints path:line:text for each history comment in
# tracked Go, TS/TSX, Swift and SQL source. Generated files (a "Code generated",
# "DO NOT EDIT" or "@generated" header) are skipped; their source is checked.
# Run from the repo root.
history_comment_offenders() {
  local globs=('*.go' '*.ts' '*.tsx' '*.swift' '*.sql') generated
  generated="$(git grep -lE '^(//|/\*|--) ?(Code generated|DO NOT EDIT|@generated)' -- "${globs[@]}" | sed 's/$/:/')"
  git grep -nIEi -e "$HISTORY_COMMENT_RE" -- "${globs[@]}" |
    grep -vF -f <(printf '%s' "$generated") || true
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

  # Check: no history-narrating comments in source.
  offenders="$(cd "$repo_root" && history_comment_offenders)"
  if [ -n "$offenders" ]; then
    echo "repo-hygiene: history-narrating comments — state the current behavior or the reason, not the PR or phase that changed it:" >&2
    echo "$offenders" | cut -c1-200 | sed 's/^/  - /' >&2
    fail=1
  fi

  return "$fail"
}

# Source-guard: run the gate only when executed directly, not when sourced by tests.
if [ "${BASH_SOURCE[0]}" = "${0}" ]; then
  run_repo_hygiene
fi
