#!/usr/bin/env bash
# Falsification suite for check-retired-seed-profiles.sh.
#
# The guard's whole job is to tell an INTENTIONAL mention of a retired seed
# profile (the refusal test; history that says it is history) from a stale
# operator instruction. A guard that cannot make that distinction is
# indistinguishable from one that always passes, so every rejection it exists to
# make is driven here against a throwaway git repo: a retired flag value bare and
# quoted, a retired profile assignment in YAML and shell-default form, the
# retired world's prose name, the retired world name in a script header, a NEW
# occurrence added inside an ALLOWLISTED file, an expected occurrence dropped from
# one, an allowlist entry whose file is gone, an allowlist entry whose file no
# longer matches at all, and a dated doc that dropped its supersession note.
#
# Precision in the other direction is a property too, so the innocent lookalikes
# (`dev-seed`, `make dev`, `development`, `devDependencies`, a /home/dev path, a
# `dev` database user, `--profile standard`) are asserted to keep the tree GREEN —
# a guard that fires on those would be turned off within a week. Exit 0 is also
# asserted for the clean tree, for an untracked stale file (the scan is scoped to
# the tracked tree, deliberately), and for the real repository.
#
# The sandbox's allowlisted fixtures are derived from the guard's own allowlist,
# counts included, so a new entry there does not silently un-test these checks.

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

# The allowlist the guard enforces (`path|count|reason`), read out of the guard.
allow_entries() {
    sed -n "/^ALLOWLIST='/,/'\$/p" "$SCRIPT" \
        | sed -e "s/^ALLOWLIST='//" -e "s/'\$//"
}

allow_paths() { allow_entries | cut -d'|' -f1; }

allow_count() { allow_entries | awk -F'|' -v p="$1" '$1 == p { print $2 }'; }

# write_matching path count : (over)write a sandbox file with exactly `count`
# lines that name a retired profile, so the file satisfies its allowlist count.
write_matching() {
    local path="$1" count="$2" i=0
    mkdir -p "$SANDBOX/$(dirname "$path")"
    : >"$SANDBOX/$path"
    while [ "$i" -lt "$count" ]; do
        printf 'fixture: names the retired prod-shaped profile\n' >>"$SANDBOX/$path"
        i=$((i + 1))
    done
}

# make_sandbox : a tracked tree that is CLEAN — every allowlisted path exists and
# names a retired profile exactly as many times as the allowlist claims, and
# nothing else mentions one.
make_sandbox() {
    SANDBOX="$(mktemp -d)"
    (
        cd "$SANDBOX"
        git -c init.defaultBranch=main init -q .
    )
    while IFS='|' read -r path count _; do
        [ -n "$path" ] || continue
        write_matching "$path" "$count"
        case "$path" in
            .ai/spec/*)
                printf 'Superseded on the seed profile — kept as history.\n' >>"$SANDBOX/$path"
                ;;
        esac
    done < <(allow_entries)
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

test_quoted_flag_value_fails() {
    echo "test: exit 1 when the retired flag value is quoted"
    make_sandbox
    # Only the retired `dev` value, never `prod-shaped`: the world-name pattern
    # matches that literal on its own, which would pass this test for the wrong
    # reason no matter how the flag were quoted.
    printf 'Run `crm-admin --seed --profile "dev" --yes` to seed.\n' >"$SANDBOX/README.md"
    printf "Or \`crm-admin --seed --profile='dev' --yes\`.\n" >>"$SANDBOX/README.md"
    track
    run_guard
    assert_rc 1 "quoted flag value"
    assert_names "README.md" "quoted flag value"
    cleanup_sandbox
}

test_yaml_profile_assignment_fails() {
    echo "test: exit 1 on a YAML profile key set to a retired world"
    make_sandbox
    mkdir -p "$SANDBOX/.github/workflows"
    printf 'jobs:\n  tours:\n    env:\n      TOURS_SEED_PROFILE: dev\n' \
        >"$SANDBOX/.github/workflows/qa.yml"
    track
    run_guard
    assert_rc 1 "yaml profile assignment"
    assert_names ".github/workflows/qa.yml" "yaml profile assignment"
    cleanup_sandbox
}

test_shell_profile_default_fails() {
    echo "test: exit 1 on a shell default that falls back to a retired world"
    make_sandbox
    mkdir -p "$SANDBOX/scripts"
    printf '#!/bin/bash\nSEED_PROFILE="${SEED_PROFILE:-dev}"\n' >"$SANDBOX/scripts/dev-seed.sh"
    track
    run_guard
    assert_rc 1 "shell profile default"
    assert_names "scripts/dev-seed.sh" "shell profile default"
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

test_prose_profile_reference_fails() {
    echo "test: exit 1 on prose that tells the operator to use the retired profile"
    make_sandbox
    printf 'Reseed staging with the dev profile before running the tours.\n' \
        >"$SANDBOX/README.md"
    track
    run_guard
    assert_rc 1 "prose profile reference"
    assert_names "README.md" "prose profile reference"
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

test_innocent_dev_words_pass() {
    echo "test: exit 0 on everyday uses of the word 'dev'"
    make_sandbox
    cat >"$SANDBOX/README.md" <<'INNOCENT'
Seed a local world with `bash scripts/dev-seed.sh`, or `make dev-seed`.
Start everything with `make dev`; the dev server reloads on save.
The development database is postgres://dev:dev@localhost:5432/crm_dev.
Frontend tooling lives under devDependencies in package.json.
Worktrees live in /home/dev/workspace/PersonalCRM/.claude/worktrees/agent-x.
Seed the declared world: `crm-admin --seed --profile standard --yes`.
CI sets TOURS_SEED_PROFILE: standard and profile: development.
INNOCENT
    track
    run_guard
    assert_rc 0 "innocent dev words"
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

test_extra_occurrence_in_allowlisted_file_fails() {
    echo "test: exit 1 when an allowlisted file gains an occurrence nobody vouched for"
    make_sandbox
    victim="$(allow_paths | head -n 1)"
    printf 'To seed locally run `crm-admin --seed --profile dev --yes`.\n' >>"$SANDBOX/$victim"
    track
    run_guard
    assert_rc 1 "extra occurrence"
    assert_names "the allowlist expects" "extra occurrence"
    assert_names "$victim" "extra occurrence"
    assert_names "nobody vouched for" "extra occurrence"
    cleanup_sandbox
}

test_dropped_occurrence_in_allowlisted_file_fails() {
    echo "test: exit 1 when an allowlisted file keeps fewer occurrences than it claims"
    make_sandbox
    victim="$(allow_entries | awk -F'|' '$2 >= 2 { print $1; exit }')"
    if [ -z "$victim" ]; then
        fail "dropped occurrence: no allowlist entry expects 2+ occurrences to drop one from"
        cleanup_sandbox
        return
    fi
    write_matching "$victim" "$(( $(allow_count "$victim") - 1 ))"
    track
    run_guard
    assert_rc 1 "dropped occurrence"
    assert_names "the allowlist expects" "dropped occurrence"
    assert_names "$victim" "dropped occurrence"
    assert_names "vouched-for occurrence is gone" "dropped occurrence"
    cleanup_sandbox
}

test_malformed_allowlist_entry_fails() {
    echo "test: exit 1 on an ALLOWLIST entry that is missing its count"
    make_sandbox
    # The allowlist is baked into the guard, so this one runs a mutated COPY.
    mutant="$(mktemp)"
    sed "s/^\\(ALLOWLIST='[^|]*\\)|[0-9][0-9]*|/\\1|/" "$SCRIPT" >"$mutant"
    if ! grep -q "^ALLOWLIST='[^|]*|[a-z]" "$mutant"; then
        fail "malformed allowlist entry: the mutation did not land in the copy"
        rm -f "$mutant"
        cleanup_sandbox
        return
    fi
    OUT="$(bash "$mutant" "$SANDBOX" 2>&1)"
    RC=$?
    assert_rc 1 "malformed allowlist entry"
    assert_names "malformed ALLOWLIST entry" "malformed allowlist entry"
    rm -f "$mutant"
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
    write_matching "$victim" "$(allow_count "$victim")"
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
    test_quoted_flag_value_fails
    test_yaml_profile_assignment_fails
    test_shell_profile_default_fails
    test_retired_prose_fails
    test_prose_profile_reference_fails
    test_retired_world_name_fails
    test_innocent_dev_words_pass
    test_untracked_stale_file_is_out_of_scope
    test_extra_occurrence_in_allowlisted_file_fails
    test_dropped_occurrence_in_allowlisted_file_fails
    test_malformed_allowlist_entry_fails
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
