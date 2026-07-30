#!/usr/bin/env bash
# Falsification suite for check-retired-seed-profiles.sh.
#
# The guard's whole job is to tell an INTENTIONAL mention of a retired seed
# profile (the refusal test; history that says it is history) from a stale
# operator instruction. A guard that cannot make that distinction is
# indistinguishable from one that always passes, so every rejection it exists to
# make is driven here against a throwaway git repo: a retired flag in a Makefile,
# the retired world's prose name, the retired world name in a script header, an
# allowlist entry whose file is gone, an allowlist entry whose file no longer
# matches, and a dated doc that dropped its supersession note. Exit 0 is asserted
# only for the clean tree, for an untracked stale file (the scan is scoped to the
# tracked tree, deliberately), and for the real repository.
#
# The sandbox's allowlisted fixtures are derived from the guard's own allowlist,
# so a new entry there does not silently un-test the stale-entry checks.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$REPO_ROOT/scripts/check-retired-seed-profiles.sh"

# A git-in-temp-repo test must not inherit the caller's git plumbing env, or
# `git init`/`git add` operate on the REAL repository.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_OBJECT_DIRECTORY \
    GIT_ALTERNATE_OBJECT_DIRECTORIES GIT_COMMON_DIR GIT_PREFIX

PASS=0
FAIL=0
fail() { echo "  FAIL: $1" >&2; FAIL=$((FAIL + 1)); }
ok() { PASS=$((PASS + 1)); }

# The paths the guard permits, read out of the guard itself.
allow_paths() {
    sed -n "/^ALLOWLIST='/,/'\$/p" "$SCRIPT" \
        | sed -e "s/^ALLOWLIST='//" -e "s/'\$//" \
        | cut -d'|' -f1
}

# make_sandbox : a tracked tree that is CLEAN — every allowlisted path exists and
# names a retired profile, and nothing else mentions one.
make_sandbox() {
    SANDBOX="$(mktemp -d)"
    (
        cd "$SANDBOX"
        git -c init.defaultBranch=main init -q .
    )
    while IFS= read -r path; do
        [ -n "$path" ] || continue
        mkdir -p "$SANDBOX/$(dirname "$path")"
        printf 'fixture: names the retired prod-shaped profile\n' >"$SANDBOX/$path"
        case "$path" in
            .ai/spec/*)
                printf 'Superseded on the seed profile — kept as history.\n' >>"$SANDBOX/$path"
                ;;
        esac
    done < <(allow_paths)
    printf 'seed:\n\tbash scripts/dev-seed.sh   # crm-admin --seed --profile standard --yes\n' \
        >"$SANDBOX/Makefile"
    ( cd "$SANDBOX" && git add -A )
}

cleanup_sandbox() { rm -rf "$SANDBOX"; }

# track : (re)stage the sandbox so a fixture edit is visible to `git ls-files`.
track() { ( cd "$SANDBOX" && git add -A ); }

# run_guard [root] : run the guard; sets RC and OUT.
run_guard() {
    OUT="$(bash "$SCRIPT" "${1:-$SANDBOX}" 2>&1)"
    RC=$?
}

assert_rc() {
    if [ "$RC" -eq "$1" ]; then ok; else fail "$2: exit $RC, want $1 (output: $OUT)"; fi
}

assert_names() {
    if printf '%s' "$OUT" | grep -qF "$1"; then ok; else fail "$2: output did not name '$1': $OUT"; fi
}

# ---------------------------------------------------------------------------

test_clean_tree_passes() {
    echo "test: exit 0 when every match is allowlisted"
    make_sandbox
    run_guard
    assert_rc 0 "clean tree"
    assert_names "no stale" "clean tree"
    cleanup_sandbox
}

test_retired_flag_fails() {
    echo "test: exit 1 on a documented --profile dev command"
    make_sandbox
    printf 'seed:\n\tcrm-admin --seed --profile dev --yes\n' >"$SANDBOX/Makefile"
    track
    run_guard
    assert_rc 1 "retired flag"
    assert_names "Makefile" "retired flag"
    cleanup_sandbox
}

test_retired_prose_fails() {
    echo "test: exit 1 on the retired world's prose name, quoted or bare"
    make_sandbox
    printf 'Seed the `dev` synthetic world, then start the servers.\n' >"$SANDBOX/README.md"
    track
    run_guard
    assert_rc 1 "retired prose"
    assert_names "README.md" "retired prose"
    cleanup_sandbox
}

test_retired_world_name_fails() {
    echo "test: exit 1 on a script that still claims the prod-shaped world"
    make_sandbox
    mkdir -p "$SANDBOX/scripts"
    printf '#!/bin/bash\n# reseed staging to the prod-shaped synthetic world.\n' \
        >"$SANDBOX/scripts/staging-reset.sh"
    track
    run_guard
    assert_rc 1 "retired world name"
    assert_names "scripts/staging-reset.sh" "retired world name"
    cleanup_sandbox
}

test_untracked_stale_file_is_out_of_scope() {
    echo "test: exit 0 for an UNTRACKED stale file (the scan is the tracked tree)"
    make_sandbox
    printf 'crm-admin --seed --profile prod-shaped --yes\n' >"$SANDBOX/scratch-notes.md"
    run_guard
    assert_rc 0 "untracked stale file"
    cleanup_sandbox
}

test_missing_allowlisted_path_fails() {
    echo "test: exit 1 when an allowlisted path no longer exists"
    make_sandbox
    victim="$(allow_paths | head -n 1)"
    rm -f "$SANDBOX/$victim"
    track
    run_guard
    assert_rc 1 "missing allowlisted path"
    assert_names "no longer exists" "missing allowlisted path"
    assert_names "$victim" "missing allowlisted path"
    cleanup_sandbox
}

test_stale_allowlist_entry_fails() {
    echo "test: exit 1 when an allowlisted path stops naming a retired profile"
    make_sandbox
    victim="$(allow_paths | head -n 1)"
    printf 'nothing retired in here any more\n' >"$SANDBOX/$victim"
    track
    run_guard
    assert_rc 1 "stale allowlist entry"
    assert_names "no longer names a retired profile" "stale allowlist entry"
    assert_names "$victim" "stale allowlist entry"
    cleanup_sandbox
}

test_unmarked_history_fails() {
    echo "test: exit 1 when an allowlisted dated doc drops its supersession note"
    make_sandbox
    victim="$(allow_paths | grep '^\.ai/spec/' | head -n 1)"
    if [ -z "$victim" ]; then
        fail "unmarked history: the allowlist has no .ai/spec/ entry to falsify"
        cleanup_sandbox
        return
    fi
    printf 'The prod-shaped profile seeds hundreds of contacts.\n' >"$SANDBOX/$victim"
    track
    run_guard
    assert_rc 1 "unmarked history"
    assert_names "supersession note" "unmarked history"
    cleanup_sandbox
}

test_real_repository_is_clean() {
    echo "test: the committed tree passes, with the allowlist actually exercised"
    run_guard "$REPO_ROOT"
    assert_rc 0 "real repository"
    if printf '%s' "$OUT" | grep -qE '\([1-9][0-9]* allowlisted occurrence'; then ok
    else fail "real repository: the allowlist matched nothing, so it gates nothing: $OUT"; fi
}

main() {
    test_clean_tree_passes
    test_retired_flag_fails
    test_retired_prose_fails
    test_retired_world_name_fails
    test_untracked_stale_file_is_out_of_scope
    test_missing_allowlisted_path_fails
    test_stale_allowlist_entry_fails
    test_unmarked_history_fails
    test_real_repository_is_clean

    echo ""
    echo "===================="
    echo "PASS=$PASS FAIL=$FAIL"
    echo "===================="
    [ "$FAIL" -eq 0 ]
}

main "$@"
