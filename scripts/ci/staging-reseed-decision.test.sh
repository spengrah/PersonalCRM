#!/usr/bin/env bash
# Tests for staging-reseed-decision.sh — the seed/migration two-dot-diff decision.
#
# Builds a throwaway git fixture repo (commits touching a synthetic file, a
# migration file, and an unrelated file), runs the decision against various
# BASE..HEAD ranges, and asserts the three flags (seed_changed / migrations_changed
# / base_known) written to a per-fixture $GITHUB_ENV. The fixture diff runs in the
# fixture CWD; the seed-group definition comes from the REAL path-filters.yml (the
# script re-points FILTERS_FILE there), so no fixture copy of the filters is needed.
#
# The fixture sets a LOCAL git identity (Ubuntu CI may have no global one).
# Portable: no network, no BSD-only flags.

set -uo pipefail

# Sanitize hook-inherited git env BEFORE any git command. Git pre-push hooks export
# GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE into the environment; without unsetting them,
# the fixture's `git init`/`git add`/`git commit` below (and the decision script's
# own `git cat-file`/`git diff`, run in the fixture CWD) operate against the hook's
# repo instead of the throwaway fixture and fail with "fatal: this operation must be
# run in a work tree". Unsetting restores cwd-based discovery so the fixture is
# isolated. (Run directly the vars are absent, so this is a no-op outside the hook.)
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="$REPO_ROOT/scripts/ci/staging-reseed-decision.sh"
ABSENT_SHA="0123456789abcdef0123456789abcdef01234567"  # 40-hex, not in the fixture

PASS=0
FAIL=0
fail() { echo "  FAIL: $1" >&2; FAIL=$((FAIL + 1)); }
ok()   { PASS=$((PASS + 1)); }

make_fixture() {
    FIXTURE="$(mktemp -d)"
    GHENV="$FIXTURE/ghenv"
    (
        cd "$FIXTURE" || exit 1
        git init -q
        git config user.email "ci@example.com"
        git config user.name "CI"
        git config commit.gpgsign false

        echo "root" > README.md
        git add README.md
        git commit -q -m "root"
        C0="$(git rev-parse HEAD)"

        mkdir -p backend/internal/synthetic
        echo "package synthetic" > backend/internal/synthetic/profiles.go
        git add backend/internal/synthetic/profiles.go
        git commit -q -m "seed change"
        C1="$(git rev-parse HEAD)"

        mkdir -p backend/migrations
        echo "-- up" > backend/migrations/074_x.up.sql
        git add backend/migrations/074_x.up.sql
        git commit -q -m "migration change"
        C2="$(git rev-parse HEAD)"

        echo "changed" >> README.md
        git add README.md
        git commit -q -m "unrelated change"
        C3="$(git rev-parse HEAD)"

        # Test-ONLY synthetic change (a *_test.go under synthetic/) — in the seed
        # path-group but NOT the built seed surface, so it must NOT trigger a reseed.
        echo "package synthetic" > backend/internal/synthetic/profiles_test.go
        git add backend/internal/synthetic/profiles_test.go
        git commit -q -m "synthetic test-only change"
        C4="$(git rev-parse HEAD)"

        # Synthetic testdata fixture — also not the built seed surface.
        mkdir -p backend/internal/synthetic/testdata
        echo "{}" > backend/internal/synthetic/testdata/seed.json
        git add backend/internal/synthetic/testdata/seed.json
        git commit -q -m "synthetic testdata change"
        C5="$(git rev-parse HEAD)"

        # RUNTIME synthetic change (no test file) — the built seed surface.
        mkdir -p backend/internal/synthetic/factory
        echo "package factory" > backend/internal/synthetic/factory/foo.go
        git add backend/internal/synthetic/factory/foo.go
        git commit -q -m "synthetic runtime change"
        C6="$(git rev-parse HEAD)"

        # NESTED migration path — proves the `migrations` group's ** glob matches at
        # depth (equivalence with the old backend/migrations/* prefix case + depth).
        mkdir -p backend/migrations/subdir
        echo "-- up" > backend/migrations/subdir/075_y.up.sql
        git add backend/migrations/subdir/075_y.up.sql
        git commit -q -m "nested migration change"
        C7="$(git rev-parse HEAD)"

        printf '%s\n%s\n%s\n%s\n%s\n%s\n%s\n%s\n' "$C0" "$C1" "$C2" "$C3" "$C4" "$C5" "$C6" "$C7" > shas.txt
    )
    # Portable read (no bash-4 mapfile): the commit SHAs are set in the subshell,
    # so read them back from the fixture file, one per line.
    { read -r C0; read -r C1; read -r C2; read -r C3; read -r C4; read -r C5; read -r C6; read -r C7; } < "$FIXTURE/shas.txt"
}

cleanup_fixture() { [ -n "${FIXTURE:-}" ] && rm -rf "$FIXTURE"; }

# run_decision <base> <head>: runs the script in the fixture CWD with an isolated
# GITHUB_ENV (never the real CI one); sets RC + populates $GHENV.
run_decision() {
    : > "$GHENV"
    ( cd "$FIXTURE" && GITHUB_ENV="$GHENV" bash "$SCRIPT" "$1" "$2" ) >/dev/null 2>&1
    RC=$?
}

# assert_flag <key> <expected>: the GITHUB_ENV file must carry exactly key=expected.
assert_flag() {
    local key="$1" want="$2" got
    got="$(grep -E "^${key}=" "$GHENV" | tail -1 | cut -d= -f2-)"
    if [ "$got" = "$want" ]; then ok; else fail "$key: want '$want', got '$got'"; fi
}

# ===========================================================================
test_seed_only() {
    echo "test: seed-only range -> seed_changed=true, migrations_changed=false, base_known=true"
    make_fixture
    run_decision "$C0" "$C1"
    if [ "$RC" -eq 0 ]; then ok; else fail "decision must exit 0, got $RC"; fi
    assert_flag seed_changed true
    assert_flag migrations_changed false
    assert_flag base_known true
    cleanup_fixture
}

test_migration_only() {
    echo "test: migration-only range -> seed_changed=false, migrations_changed=true"
    make_fixture
    run_decision "$C1" "$C2"
    if [ "$RC" -eq 0 ]; then ok; else fail "decision must exit 0, got $RC"; fi
    assert_flag seed_changed false
    assert_flag migrations_changed true
    assert_flag base_known true
    cleanup_fixture
}

test_unrelated_only() {
    echo "test: unrelated-only range -> both false, base_known=true"
    make_fixture
    run_decision "$C2" "$C3"
    assert_flag seed_changed false
    assert_flag migrations_changed false
    assert_flag base_known true
    cleanup_fixture
}

test_both_changed() {
    echo "test: range spanning both -> seed_changed=true AND migrations_changed=true"
    make_fixture
    run_decision "$C0" "$C2"
    assert_flag seed_changed true
    assert_flag migrations_changed true
    assert_flag base_known true
    cleanup_fixture
}

test_test_only_synthetic_no_reseed() {
    echo "test: test-only synthetic change (*_test.go) -> seed_changed=false (not the built seed surface)"
    make_fixture
    run_decision "$C3" "$C4"
    if [ "$RC" -eq 0 ]; then ok; else fail "must exit 0, got $RC"; fi
    assert_flag seed_changed false
    assert_flag base_known true
    cleanup_fixture
}

test_testdata_synthetic_no_reseed() {
    echo "test: synthetic testdata change -> seed_changed=false"
    make_fixture
    run_decision "$C4" "$C5"
    assert_flag seed_changed false
    assert_flag base_known true
    cleanup_fixture
}

test_test_and_testdata_only_no_reseed() {
    echo "test: combined test+testdata-only range (no runtime) -> seed_changed=false"
    make_fixture
    run_decision "$C3" "$C5"
    assert_flag seed_changed false
    assert_flag base_known true
    cleanup_fixture
}

test_runtime_synthetic_reseed() {
    echo "test: runtime synthetic change (factory/foo.go) -> seed_changed=true"
    make_fixture
    run_decision "$C5" "$C6"
    assert_flag seed_changed true
    assert_flag base_known true
    cleanup_fixture
}

test_mixed_runtime_and_test_reseed() {
    echo "test: mixed range (test + testdata + runtime synthetic) -> seed_changed=true (runtime present)"
    make_fixture
    run_decision "$C3" "$C6"   # spans profiles_test.go + testdata/seed.json + factory/foo.go
    assert_flag seed_changed true
    assert_flag base_known true
    cleanup_fixture
}

test_migration_nested_path() {
    echo "test: nested migration path (backend/migrations/subdir/...) -> migrations_changed=true (** depth)"
    make_fixture
    run_decision "$C6" "$C7"
    if [ "$RC" -eq 0 ]; then ok; else fail "must exit 0, got $RC"; fi
    assert_flag migrations_changed true
    assert_flag seed_changed false
    assert_flag base_known true
    cleanup_fixture
}

test_empty_base() {
    echo "test: empty BASE -> base_known=false, seed_changed=false, exit 0"
    make_fixture
    run_decision "" "$C3"
    if [ "$RC" -eq 0 ]; then ok; else fail "empty BASE must exit 0, got $RC"; fi
    assert_flag base_known false
    assert_flag seed_changed false
    assert_flag migrations_changed false
    cleanup_fixture
}

test_base_not_in_history() {
    echo "test: BASE not in local history -> base_known=false, seed_changed=false, exit 0"
    make_fixture
    run_decision "$ABSENT_SHA" "$C3"
    if [ "$RC" -eq 0 ]; then ok; else fail "absent BASE must exit 0, got $RC"; fi
    assert_flag base_known false
    assert_flag seed_changed false
    cleanup_fixture
}

# ---------------------------------------------------------------------------
main() {
    test_seed_only
    test_migration_only
    test_unrelated_only
    test_both_changed
    test_test_only_synthetic_no_reseed
    test_testdata_synthetic_no_reseed
    test_test_and_testdata_only_no_reseed
    test_runtime_synthetic_reseed
    test_mixed_runtime_and_test_reseed
    test_migration_nested_path
    test_empty_base
    test_base_not_in_history

    echo ""
    echo "===================="
    echo "PASS=$PASS FAIL=$FAIL"
    echo "===================="
    [ "$FAIL" -eq 0 ]
}

main "$@"
