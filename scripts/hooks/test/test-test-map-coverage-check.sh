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

# (d) fail-closed: a rule with NO "pattern" key would otherwise become
# new RegExp(undefined) — a match-ALL regex that marks every spec covered and
# fails OPEN. It must exit 2 instead.
cat > "$tmp/missing-pattern.json" <<'JSON'
[
  { "tags": ["@area:contacts"] }
]
JSON
node "$MODULE" "$tmp/missing-pattern.json" "frontend/tests/e2e/contacts.spec.ts" >/dev/null 2>&1; rc=$?
assert_eq "rule missing pattern -> exit 2 (fail-closed, no match-all)" "2" "$rc"

# (d) fail-closed: a rule whose "pattern" is a non-string -> exit 2.
cat > "$tmp/nonstring-pattern.json" <<'JSON'
[
  { "pattern": 123, "tags": ["@area:contacts"] }
]
JSON
node "$MODULE" "$tmp/nonstring-pattern.json" "frontend/tests/e2e/contacts.spec.ts" >/dev/null 2>&1; rc=$?
assert_eq "rule non-string pattern -> exit 2 (fail-closed)" "2" "$rc"

# (e) empty spec list (no spec argv) -> module exit 2.
node "$MODULE" "$tmp/map.json" >/dev/null 2>&1; rc=$?
assert_eq "empty spec list -> exit 2 (fail-closed)" "2" "$rc"

# Missing map file -> module exit 2.
node "$MODULE" "$tmp/does-not-exist.json" "frontend/tests/e2e/contacts.spec.ts" >/dev/null 2>&1; rc=$?
assert_eq "missing map file -> exit 2 (fail-closed)" "2" "$rc"

# Wrapper fail-closed: drive the REAL run_test_map_coverage (sourced above) against
# an injected map passed as its optional $1. This exercises the actual command-
# substitution rc-capture (`offenders="$(node ...)"; rc=$?; case "$rc"`) in the guard
# — not a reimplementation — so a future regression in that path is caught here. The
# guard still reads the live tracked spec list via git ls-files, which all map
# cleanly, so the verdict is driven entirely by the injected map's validity.
run_test_map_coverage "$tmp/bad-json.json" >/dev/null 2>&1; rc=$?
assert_eq "wrapper: malformed-JSON map -> non-zero verdict (blocks push)" "1" "$rc"
run_test_map_coverage "$tmp/missing-pattern.json" >/dev/null 2>&1; rc=$?
assert_eq "wrapper: map with patternless rule -> non-zero verdict (blocks push)" "1" "$rc"

# Wrapper offenders path (rc=1 — the case the guard EXISTS to catch): drive the
# real wrapper against a VALID map that is the live map MINUS the gchat self-entry,
# so the live git-ls-files spec list contains one spec the map no longer covers.
# This exercises the wrapper's offenders branch end-to-end (not just the module),
# proving an unmapped live spec actually blocks the push — a regression making the
# rc=1 branch fall through to 0 would be caught here, not just by the all-pass guard.
node -e "
const fs=require('fs');
const m=JSON.parse(fs.readFileSync('frontend/tests/e2e/test-map.json','utf8'));
const dropped=m.filter(r=>r.pattern!=='^frontend/tests/e2e/gchat-contact-signal\\\\.spec\\\\.ts\$');
if (dropped.length !== m.length-1) { console.error('fixture setup error: expected to drop exactly 1 entry'); process.exit(3); }
fs.writeFileSync('$tmp/map-missing-gchat.json', JSON.stringify(dropped));
"
run_test_map_coverage "$tmp/map-missing-gchat.json" >/dev/null 2>&1; rc=$?
assert_eq "wrapper: valid map missing a live spec entry -> rc=1 (offenders block push)" "1" "$rc"

# Live-repo regression guard: after the Step-1 backfill the real guard exits 0.
if run_test_map_coverage >/dev/null 2>&1; then
  echo "ok: live repo passes test-map coverage (all specs self-mapped)"
else
  echo "FAIL: live repo has an e2e spec with no test-map.json self-entry"
  fail=1
fi

[[ "$fail" -eq 0 ]] && { echo "ALL PASS"; exit 0; } || { echo "FAILURES"; exit 1; }
