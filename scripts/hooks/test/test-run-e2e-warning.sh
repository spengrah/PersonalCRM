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
# Sanitize hook-inherited git env BEFORE any git command. git exports GIT_DIR to
# hooks (absolute in linked worktrees), which silently redirects every
# `git -C "$tmp" ...` below at the REAL repo with work-tree=$tmp: `add -A` wipes
# the real index down to the temp fixture files, `commit` strands fixture commits
# on the pushed branch, and `update-ref -d` deletes the real origin/develop
# tracking ref. Unsetting here restores cwd-based discovery, so the throwaway
# repo is genuinely isolated (and the node child below inherits the clean env).
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE
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

mkdir -p "$tmp/frontend/tests/e2e" "$tmp/frontend/src/app/contacts" "$tmp/backend/internal" "$tmp/backend/internal/spec"
cat > "$tmp/frontend/tests/e2e/test-map.json" <<'JSON'
[
  { "pattern": "^frontend/src/app/contacts/", "tags": ["@area:contacts"] },
  { "pattern": "^backend/internal/spec/", "tags": [] }
]
JSON
# A tracked baseline file so the repo has an initial commit to diff against.
echo "seed" > "$tmp/README.md"
git -C "$tmp" add -A
git -C "$tmp" commit -qm "base"
BASE="$(git -C "$tmp" rev-parse HEAD)"

# --- controlled diff on top of the base ---
# Mapped change (frontend/src -> @area:contacts), unmapped change (backend/internal),
# and an EMPTY-TAGS-mapped change (backend/internal/spec -> tags: []) pinning the
# deliberate empty-tags mapping behavior: matching a rule silences the unmapped-file
# warning even when the rule contributes no tags.
echo "export const x = 1" > "$tmp/frontend/src/app/contacts/x.ts"
echo "package internal" > "$tmp/backend/internal/foo.go"
echo "package spec" > "$tmp/backend/internal/spec/bar.go"
git -C "$tmp" add -A
git -C "$tmp" commit -qm "change"

# --- run the real selector in print-only mode against the pinned base ---
stderr_file="$tmp/stderr.txt"
stdout="$(cd "$tmp" && E2E_PRINT_ONLY=1 E2E_BASE_REF="$BASE" node "$SCRIPT" 2>"$stderr_file")"
stderr="$(cat "$stderr_file")"

assert_contains     "warns on unmapped backend/internal file"        "$stderr" "backend/internal/foo.go"
assert_not_contains "does NOT warn on mapped frontend/src file"      "$stderr" "frontend/src/app/contacts/x.ts"
assert_not_contains "does NOT warn on empty-tags-mapped spec file"   "$stderr" "backend/internal/spec/bar.go"
assert_contains     "stdout grep pattern carries the mapped tag"     "$stdout" "@area:contacts"
assert_not_contains "stdout is pure (no warning text leaked to it)"  "$stdout" "WARNING"

# --- base-ref resolver fallback (NO E2E_BASE_REF) ---
# With E2E_BASE_REF unset and no tracked upstream, the resolver falls back to the
# first existing ref in origin/develop -> origin/main. Create an origin/develop ref
# pointing at BASE so the resolver picks it up; the diff over BASE...HEAD then sees
# the same controlled changes, so the warning still fires on the unmapped file.
git -C "$tmp" update-ref refs/remotes/origin/develop "$BASE"
stderr2_file="$tmp/stderr2.txt"
stdout2="$(cd "$tmp" && E2E_PRINT_ONLY=1 node "$SCRIPT" 2>"$stderr2_file")"
stderr2="$(cat "$stderr2_file")"
assert_contains "resolver falls back to origin/develop -> warns on unmapped file" "$stderr2" "backend/internal/foo.go"
assert_contains "resolver fallback still selects mapped tag"                       "$stdout2" "@area:contacts"

# --- no base ref at all (bare clone): must NOT crash, degrades to empty diff,
#     AND must WARN that the base diff failed ---
# Remove origin/develop; with no upstream and no origin/develop/origin/main, the
# resolver returns origin/main (nonexistent) and the git diff against it must be
# swallowed to an empty changed set rather than crashing the script — but the
# catch-block must announce the degradation on stderr (this pins that warning
# against a silent-deletion regression).
git -C "$tmp" update-ref -d refs/remotes/origin/develop
stderr3_file="$tmp/stderr3.txt"
stdout3="$(cd "$tmp" && E2E_PRINT_ONLY=1 node "$SCRIPT" 2>"$stderr3_file")"; rc3=$?
stderr3="$(cat "$stderr3_file")"
assert_eq() { if [[ "$2" == "$3" ]]; then echo "ok: $1"; else echo "FAIL: $1 (expected '$2', got '$3')"; fail=1; fi; }
assert_eq       "no base ref -> script exits 0 (no crash)" "0" "$rc3"
assert_contains "no base ref -> still emits a grep pattern (at least @smoke)" "$stdout3" "@smoke"
assert_contains "no base ref -> WARNS that the base diff failed" "$stderr3" "diff-selection is treating it as no changes"

[[ "$fail" -eq 0 ]] && { echo "ALL PASS"; exit 0; } || { echo "FAILURES"; exit 1; }
