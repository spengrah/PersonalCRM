#!/usr/bin/env bash
# Unit test for the warn-on-unmatched-files behavior in scripts/run-e2e-local.mjs.
#
# Stands up a throwaway git repo (mktemp -d) with a minimal test-map.json, a base
# commit, and a controlled diff (one MAPPED frontend/src file + one UNMAPPED
# backend/internal file), then runs the real run-e2e-local.mjs with E2E_PRINT_ONLY=1
# (so it computes selection + warning and exits WITHOUT spawning make/Playwright) and
# E2E_BASE_REF pinned to the base commit (so the diff is deterministic, independent of
# the live repo's git state). Asserts:
#   - stderr lists the unmapped backend/internal file (warned)
#   - stderr does NOT list the mapped frontend/src file (not warned)
#   - stdout (the grep pattern) carries the mapped file's tag
#   - stdout contains NO warning text (stdout-purity guard for the unconditional
#     warning-under-print-only decision: the warning must live on stderr only)
set -u
cd "$(dirname "${BASH_SOURCE[0]}")/../../.." || exit 1   # repo root of THIS repo
SCRIPT="$(pwd)/scripts/run-e2e-local.mjs"

fail=0
assert_contains() {
  # assert_contains <desc> <haystack> <needle>
  if [[ "$2" == *"$3"* ]]; then echo "ok: $1"; else echo "FAIL: $1 (missing '$3')"; fail=1; fi
}
assert_not_contains() {
  # assert_not_contains <desc> <haystack> <needle>
  if [[ "$2" != *"$3"* ]]; then echo "ok: $1"; else echo "FAIL: $1 (unexpectedly found '$3')"; fail=1; fi
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# --- seed a throwaway repo ---
git -C "$tmp" init -q
git -C "$tmp" config user.email test@example.com
git -C "$tmp" config user.name "test"
git -C "$tmp" config commit.gpgsign false

mkdir -p "$tmp/frontend/tests/e2e" "$tmp/frontend/src/app/contacts" "$tmp/backend/internal"
cat > "$tmp/frontend/tests/e2e/test-map.json" <<'JSON'
[
  { "pattern": "^frontend/src/app/contacts/", "tags": ["@area:contacts"] }
]
JSON
# A tracked baseline file so the repo has an initial commit to diff against.
echo "seed" > "$tmp/README.md"
git -C "$tmp" add -A
git -C "$tmp" commit -qm "base"
BASE="$(git -C "$tmp" rev-parse HEAD)"

# --- controlled diff on top of the base ---
# Mapped change (frontend/src -> @area:contacts) and unmapped change (backend/internal).
echo "export const x = 1" > "$tmp/frontend/src/app/contacts/x.ts"
echo "package internal" > "$tmp/backend/internal/foo.go"
git -C "$tmp" add -A
git -C "$tmp" commit -qm "change"

# --- run the real selector in print-only mode against the pinned base ---
stderr_file="$tmp/stderr.txt"
stdout="$(cd "$tmp" && E2E_PRINT_ONLY=1 E2E_BASE_REF="$BASE" node "$SCRIPT" 2>"$stderr_file")"
stderr="$(cat "$stderr_file")"

assert_contains     "warns on unmapped backend/internal file"        "$stderr" "backend/internal/foo.go"
assert_not_contains "does NOT warn on mapped frontend/src file"      "$stderr" "frontend/src/app/contacts/x.ts"
assert_contains     "stdout grep pattern carries the mapped tag"     "$stdout" "@area:contacts"
assert_not_contains "stdout is pure (no warning text leaked to it)"  "$stdout" "WARNING"

[[ "$fail" -eq 0 ]] && { echo "ALL PASS"; exit 0; } || { echo "FAILURES"; exit 1; }
