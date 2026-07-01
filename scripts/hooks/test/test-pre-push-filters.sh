#!/bin/bash
# Unit tests for the group-aware path matcher in scripts/hooks/pre-push.
# Run: bash scripts/hooks/test/test-pre-push-filters.sh
#
# Sources the hook (the source-guard prevents the hook body from running) and
# asserts the pure path-matching functions directly. No real `swift test`, no
# real push, no daemon change required. Dependency-free (no bats).
set -u
cd "$(dirname "${BASH_SOURCE[0]}")/../../.." || exit 1   # repo root
source scripts/hooks/pre-push                            # source-guard prevents hook body

fail=0

assert_in_group() {
  if file_in_group "$1" "$2"; then echo "ok: $1 in $2"; else echo "FAIL: $1 should be in $2"; fail=1; fi
}
assert_not_in_group() {
  if file_in_group "$1" "$2"; then echo "FAIL: $1 should NOT be in $2"; fail=1; else echo "ok: $1 not in $2"; fi
}
assert_true() {
  # assert_true <description> <cmd...>
  local desc="$1"; shift
  if "$@"; then echo "ok: $desc"; else echo "FAIL: $desc (expected true)"; fail=1; fi
}
assert_false() {
  local desc="$1"; shift
  if "$@"; then echo "FAIL: $desc (expected false)"; fail=1; else echo "ok: $desc"; fi
}
assert_grep_count() {
  # assert_grep_count <expected> <pattern> <file>
  local expected="$1" pattern="$2" file="$3" actual
  actual=$(grep -c -- "$pattern" "$file")
  if [[ "$actual" -eq "$expected" ]]; then
    echo "ok: grep '$pattern' count=$actual"
  else
    echo "FAIL: grep '$pattern' count=$actual, expected $expected"
    fail=1
  fi
}

# --- file_in_group: group membership ---
# Daemon-only file triggers Swift gate, NOT Go suite.
assert_in_group     "mac-daemon/Sources/foo.swift" mac_daemon
assert_not_in_group "mac-daemon/Sources/foo.swift" backend
assert_not_in_group "mac-daemon/Sources/foo.swift" frontend
# Backend-only file triggers Go suite, NOT Swift.
assert_in_group     "backend/internal/x.go" backend
assert_not_in_group "backend/internal/x.go" mac_daemon
assert_not_in_group "backend/internal/x.go" frontend
# Frontend-only file.
assert_in_group     "frontend/src/app/page.tsx" frontend
assert_not_in_group "frontend/src/app/page.tsx" mac_daemon
# go.sum re-added-line regression guard.
assert_in_group     "go.sum" backend
# Gained scripts/** trigger.
assert_in_group     "scripts/hooks/pre-push" backend
# ci.yml is intentionally in BOTH backend and mac_daemon.
assert_in_group     ".github/workflows/ci.yml" backend
assert_in_group     ".github/workflows/ci.yml" mac_daemon
# Docs-only push runs NEITHER gate (no over-triggering).
assert_not_in_group "README.md" backend
assert_not_in_group "README.md" frontend
assert_not_in_group "README.md" mac_daemon
# Glob edge: broadened frontend/** superset check.
assert_in_group     "frontend/playwright.config.ts" frontend
# Anchor exclusion: frontend/** must not over-reach into a sibling dir like frontendx/.
assert_not_in_group "frontendx/y.ts" frontend
# Self-gating regression guard: the filter file routes to validation.
assert_in_group     "path-filters.yml" backend

# --- seed group: the staging auto-reseed surface (orthogonal to test selection) ---
# Synthetic toolkit + profiles ARE the seed surface.
assert_in_group     "backend/internal/synthetic/profiles.go" seed
assert_in_group     "backend/internal/synthetic/factory/foo.go" seed
# Orthogonality: a synthetic file is ALSO in backend, so the seed group changes no
# Go/frontend test selection (it is a separate classification consumed only by the
# reseed decision).
assert_in_group     "backend/internal/synthetic/profiles.go" backend
# Non-seed backend code and migrations are NOT in the seed group.
assert_not_in_group "backend/internal/api/handlers/contact.go" seed
assert_not_in_group "backend/migrations/074_x.up.sql" seed

# --- migrations group: the staging reseed-decision schema-change surface ---
# Migrations at the top level and at depth are in the migrations group (** depth).
assert_in_group     "backend/migrations/074_x.up.sql" migrations
assert_in_group     "backend/migrations/subdir/075_y.up.sql" migrations
# Orthogonality: a migration is ALSO in backend, so the migrations group changes no
# Go/frontend test selection (it is a separate classification consumed only by the
# reseed decision).
assert_in_group     "backend/migrations/074_x.up.sql" backend
# Non-migration backend code is NOT in the migrations group.
assert_not_in_group "backend/internal/api/handlers/contact.go" migrations

# --- any_file_in_groups: the Go/frontend gate's decision boundary ---
# Daemon-only push does NOT run Go suite (headline invariant).
assert_false "any_file_in_groups daemon-only -> Go suite NOT run" \
  any_file_in_groups "mac-daemon/x.swift" backend frontend
# Mixed push runs Go suite.
assert_true "any_file_in_groups mixed -> Go suite run" \
  any_file_in_groups $'backend/x.go\nmac-daemon/y.swift' backend frontend
# Composed-helper assertions mirroring should_skip_tests' two call sites.
assert_true  "any_file_in_groups backend-only -> Go suite run" \
  any_file_in_groups "backend/x.go" backend frontend
assert_true  "any_file_in_groups frontend-only -> Go suite run" \
  any_file_in_groups "frontend/src/x.tsx" backend frontend
assert_false "any_file_in_groups empty range -> no Go run" \
  any_file_in_groups "" backend frontend

# --- any_file_under_macdaemon: the stricter local Swift predicate ---
# Daemon source fires the Swift gate.
assert_true  "any_file_under_macdaemon daemon source -> fires" \
  any_file_under_macdaemon "mac-daemon/Sources/x.swift"
# ci.yml-only push is in the mac_daemon GROUP but must NOT fire the LOCAL gate.
assert_false "any_file_under_macdaemon ci.yml-only -> does NOT fire (ci.yml is in the mac_daemon group, but the local predicate is literal mac-daemon/)" \
  any_file_under_macdaemon ".github/workflows/ci.yml"
# Backend-only and empty range do not fire.
assert_false "any_file_under_macdaemon backend-only -> does NOT fire" \
  any_file_under_macdaemon "backend/x.go"
assert_false "any_file_under_macdaemon empty range -> does NOT fire" \
  any_file_under_macdaemon ""
# Mixed push fires Swift gate.
assert_true  "any_file_under_macdaemon mixed -> fires" \
  any_file_under_macdaemon $'backend/x.go\nmac-daemon/y.swift'

# --- structural guard: both should_skip_tests call sites repointed to "backend frontend" ---
assert_grep_count 1 'any_file_in_groups "$pushed_files" backend frontend' scripts/hooks/pre-push
assert_grep_count 1 'any_file_in_groups "$files_since_test" backend frontend' scripts/hooks/pre-push

# --- Chain the sibling guard suites so the FILTER phase covers them all in one
# command (the pre-push classifier routes this single test-pre-push-filters
# command to the CONCURRENT lane). ---
echo "--- pre-push phase classifier / failure-propagation guard ---"
bash scripts/hooks/test/test-pre-push-phases.sh || fail=1
echo "--- Makefile adaptive -p / CI-pin render guard ---"
bash scripts/ci/test-parallelism-render-guard.sh || fail=1
echo "--- per-worktree test-pg resolver unit (shim-only, DB/port-free) ---"
bash scripts/test/test-worktree-test-pg.sh || fail=1
echo "--- worktree env-link resolver + post-checkout gate (fs/git-only) ---"
bash scripts/test/test-link-worktree-env.sh || fail=1
echo "--- worktree dep-install + preflight + post-checkout/pre-push wiring (stubbed bun, fs/git-only) ---"
bash scripts/test/test-install-worktree-deps.sh || fail=1
echo "--- repo-hygiene check guard ---"
bash scripts/hooks/test/test-repo-hygiene-check.sh || fail=1
echo "--- e2e test-map coverage guard ---"
bash scripts/hooks/test/test-test-map-coverage-check.sh || fail=1
echo "--- run-e2e-local warning behavior ---"
bash scripts/hooks/test/test-run-e2e-warning.sh || fail=1

[[ "$fail" -eq 0 ]] && { echo "ALL PASS"; exit 0; } || { echo "FAILURES"; exit 1; }
