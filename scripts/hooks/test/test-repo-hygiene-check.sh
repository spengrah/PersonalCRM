#!/usr/bin/env bash
# Unit tests for scripts/hooks/repo-hygiene-check.sh. Sources the gate
# (source-guard prevents it from running) and asserts the pure logic with
# injected input, then confirms the live repo passes. No real push.
set -u
cd "$(dirname "${BASH_SOURCE[0]}")/../../.." || exit 1   # repo root
source scripts/hooks/repo-hygiene-check.sh               # source-guard prevents the gate body

fail=0
assert_eq() {
  # assert_eq <desc> <expected> <actual>
  if [[ "$2" == "$3" ]]; then echo "ok: $1"; else echo "FAIL: $1 (expected '$2', got '$3')"; fail=1; fi
}

# root_html_offenders flags only root-level *.html — nested paths, non-.html
# extensions, and temp/ prototypes are all ignored.
got="$(printf 'index.html\nfrontend/public/app.html\nREADME.md\nproto.htm\ntemp/x.html\n' | root_html_offenders | tr '\n' ',')"
assert_eq "flags only root *.html (nested/temp/ext ignored)" "index.html," "$got"

# Two root-level offenders both reported.
got="$(printf 'a.html\nb.html\nsub/c.html\n' | root_html_offenders | tr '\n' ',')"
assert_eq "reports every root offender" "a.html,b.html," "$got"

# Empty input → no offenders.
assert_eq "empty input → no offenders" "" "$(printf '' | root_html_offenders)"

# The live repo is currently clean — the gate must exit 0.
if run_repo_hygiene >/dev/null 2>&1; then echo "ok: live repo clean (no root *.html)"; else echo "FAIL: live repo has a root-level *.html"; fail=1; fi

[[ "$fail" -eq 0 ]] && { echo "ALL PASS"; exit 0; } || { echo "FAILURES"; exit 1; }
