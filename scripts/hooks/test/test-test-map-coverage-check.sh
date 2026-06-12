#!/usr/bin/env bash
# Unit tests for scripts/hooks/test-map-coverage-check.sh and its matcher module
# scripts/hooks/lib/test-map-coverage.mjs. Drives the module directly with temp
# fixtures (a fixture test-map.json + fixture spec paths on argv) so the matching
# semantics and the fail-closed exit codes are pinned, then confirms the live repo
# passes the guard. No real push.
set -u
cd "$(dirname "${BASH_SOURCE[0]}")/../../.." || exit 1   # repo root
source scripts/hooks/test-map-coverage-check.sh          # source-guard prevents the gate body

MODULE="scripts/hooks/lib/test-map-coverage.mjs"

fail=0
assert_eq() {
  # assert_eq <desc> <expected> <actual>
  if [[ "$2" == "$3" ]]; then echo "ok: $1"; else echo "FAIL: $1 (expected '$2', got '$3')"; fail=1; fi
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# A representative fixture map mirroring the real conventions: single-file anchored
# entries plus the imports glob whose JS-RegExp semantics the guard relies on.
cat > "$tmp/map.json" <<'JSON'
[
  { "pattern": "^frontend/tests/e2e/contacts\\.spec\\.ts$", "tags": ["@area:contacts"] },
  { "pattern": "^frontend/tests/e2e/imports(-[a-z]+)*\\.spec\\.ts$", "tags": ["@area:imports"] }
]
JSON

# (a) a spec WITH a self-entry -> not an offender, exit 0.
out="$(node "$MODULE" "$tmp/map.json" "frontend/tests/e2e/contacts.spec.ts")"; rc=$?
assert_eq "matched spec -> exit 0" "0" "$rc"
assert_eq "matched spec -> no offenders printed" "" "$out"

# (b) a spec with NO entry -> printed as an offender, exit 1.
out="$(node "$MODULE" "$tmp/map.json" "frontend/tests/e2e/gchat-contact-signal.spec.ts")"; rc=$?
assert_eq "unmatched spec -> exit 1" "1" "$rc"
assert_eq "unmatched spec -> printed as offender" "frontend/tests/e2e/gchat-contact-signal.spec.ts" "$out"

# (c) imports-foo.spec.ts against the imports(-[a-z]+)* glob -> matched (pins the
# JS-RegExp glob semantics the guard depends on; bash grep -E would differ).
out="$(node "$MODULE" "$tmp/map.json" "frontend/tests/e2e/imports-correspondence.spec.ts")"; rc=$?
assert_eq "imports-foo glob -> matched, exit 0" "0" "$rc"

# Mixed list: one matched + one not -> only the unmatched one is an offender, exit 1.
out="$(node "$MODULE" "$tmp/map.json" "frontend/tests/e2e/contacts.spec.ts" "frontend/tests/e2e/orphan.spec.ts")"; rc=$?
assert_eq "mixed list -> exit 1" "1" "$rc"
assert_eq "mixed list -> only unmatched offender printed" "frontend/tests/e2e/orphan.spec.ts" "$out"

# (d) fail-closed: invalid-JSON map -> module exit 2 (NOT 1), stderr non-empty.
echo '{ this is not json' > "$tmp/bad-json.json"
out="$(node "$MODULE" "$tmp/bad-json.json" "frontend/tests/e2e/contacts.spec.ts" 2>/dev/null)"; rc=$?
assert_eq "invalid-JSON map -> exit 2 (fail-closed)" "2" "$rc"

# (d) fail-closed: a syntactically-bad regex pattern -> module exit 2.
cat > "$tmp/bad-regex.json" <<'JSON'
[
  { "pattern": "(", "tags": ["@area:contacts"] }
]
JSON
node "$MODULE" "$tmp/bad-regex.json" "frontend/tests/e2e/contacts.spec.ts" >/dev/null 2>&1; rc=$?
assert_eq "bad regex pattern -> exit 2 (fail-closed)" "2" "$rc"

# (d) fail-closed: a map that parses but is not an array -> module exit 2.
echo '{ "pattern": "x" }' > "$tmp/not-array.json"
node "$MODULE" "$tmp/not-array.json" "frontend/tests/e2e/contacts.spec.ts" >/dev/null 2>&1; rc=$?
assert_eq "non-array map -> exit 2 (fail-closed)" "2" "$rc"

# (e) empty spec list (no spec argv) -> module exit 2.
node "$MODULE" "$tmp/map.json" >/dev/null 2>&1; rc=$?
assert_eq "empty spec list -> exit 2 (fail-closed)" "2" "$rc"

# Missing map file -> module exit 2.
node "$MODULE" "$tmp/does-not-exist.json" "frontend/tests/e2e/contacts.spec.ts" >/dev/null 2>&1; rc=$?
assert_eq "missing map file -> exit 2 (fail-closed)" "2" "$rc"

# Wrapper fail-closed: the bash guard must map a non-{0,1} module rc to a non-zero
# return (never treat an internal error as a pass). We invoke the guard's exact
# module-call shape against the malformed map and assert the rc-branch verdict.
# This mirrors run_test_map_coverage's `node ... ; rc=$?; case rc` logic.
guard_verdict_for_map() {
  # guard_verdict_for_map <map> <spec...> -> returns the verdict rc the wrapper would return
  local map="$1"; shift
  local rc
  node "$MODULE" "$map" "$@" >/dev/null 2>&1; rc=$?
  case "$rc" in
    0) return 0 ;;
    1) return 1 ;;
    *) return 1 ;;  # fail-closed: any other rc blocks the push
  esac
}
guard_verdict_for_map "$tmp/bad-json.json" "frontend/tests/e2e/contacts.spec.ts" >/dev/null 2>&1; rc=$?
assert_eq "wrapper: malformed map -> non-zero verdict (blocks push)" "1" "$rc"
guard_verdict_for_map "$tmp/map.json" "frontend/tests/e2e/contacts.spec.ts" >/dev/null 2>&1; rc=$?
assert_eq "wrapper: clean map+matched spec -> zero verdict (passes)" "0" "$rc"

# Live-repo regression guard: after the Step-1 backfill the real guard exits 0.
if run_test_map_coverage >/dev/null 2>&1; then
  echo "ok: live repo passes test-map coverage (all specs self-mapped)"
else
  echo "FAIL: live repo has an e2e spec with no test-map.json self-entry"
  fail=1
fi

[[ "$fail" -eq 0 ]] && { echo "ALL PASS"; exit 0; } || { echo "FAILURES"; exit 1; }
