#!/usr/bin/env bash
# Tests for qa-round-cadence-gate.sh AND qa-round-deployed-sha-assert.sh — the
# nightly QA round's cadence decision + deployed-sha pin/manifest assertion.
#
# Builds throwaway git fixture repos in a temp dir and COPIES the two scripts, the
# pre-push hook (for file_in_group), and path-filters.yml into each fixture's own
# scripts/ci / scripts/hooks layout. Running the FIXTURE copy makes each script
# resolve its REPO_ROOT to the fixture, so a test can damage the fixture's
# path-filters.yml / pre-push (missing / unreadable / absent-or-broken required
# group / absent file_in_group) WITHOUT ever touching the real checkout. The
# infra files are copied UNTRACKED and each feature commit adds only its own file,
# so the copied scripts never appear in any diff range.
#
# The degrade-to-SKIP contract is load-bearing: for this advisory gate EVERY
# uncertainty (empty/unresolvable BASE or HEAD, missing/unreadable filters, absent
# file_in_group, temp-file / git / read failure, unusable DAYS_SINCE) must emit the
# uncertain-skip tuple (run_round=FALSE, judge_relevant_change=unknown,
# base_known=false, changed_groups=) and exit 0 — it fails toward NOT running
# (conservative on judge-token spend; a manual trigger is the backstop). The SEPARATE
# deployed-sha-assert script stays FAIL-CLOSED (integrity gate, not a skip decision).
#
# Portable: no network, no BSD-only flags — this suite runs on Ubuntu CI.

set -uo pipefail

# Sanitize hook-inherited git env BEFORE any git command.
# Git pre-push hooks export GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE; without unsetting
# them the fixture's git ops (and the scripts' git cat-file/diff/rev-parse, run in
# the fixture CWD) would operate against the REAL repo and corrupt it / pollute
# identity. Unsetting restores cwd-based discovery. (Run directly the vars are
# absent, so this is a no-op outside the hook.)
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
REAL_CADENCE="$REPO_ROOT/scripts/ci/qa-round-cadence-gate.sh"
REAL_ASSERT="$REPO_ROOT/scripts/ci/qa-round-deployed-sha-assert.sh"
REAL_PREPUSH="$REPO_ROOT/scripts/hooks/pre-push"
REAL_FILTERS="$REPO_ROOT/path-filters.yml"
REAL_GIT="$(command -v git)"
ABSENT_SHA40="0123456789abcdef0123456789abcdef01234567"  # 40-hex, not in any fixture

PASS=0
FAIL=0
fail() { echo "  FAIL: $1" >&2; FAIL=$((FAIL + 1)); }
ok()   { PASS=$((PASS + 1)); }

# ---------------------------------------------------------------------------
# Fixture construction
# ---------------------------------------------------------------------------

# require_tmpdir <varname>: mktemp -d into the named var, aborting the WHOLE run if
# it fails or yields no directory. A silently-empty temp dir would make copy_infra
# target /scripts and `cd ""` stay in the real checkout, so the fixture's git ops
# would corrupt the REAL repo — this is the repo's git-fixture hazard, guarded hard.
require_tmpdir() {
    local __d
    __d="$(mktemp -d)" || { echo "FATAL: mktemp -d failed" >&2; exit 2; }
    { [ -n "$__d" ] && [ -d "$__d" ]; } || { echo "FATAL: mktemp -d produced no directory" >&2; exit 2; }
    printf -v "$1" '%s' "$__d"
}

copy_infra() {
    mkdir -p "$FIXTURE/scripts/ci" "$FIXTURE/scripts/hooks"
    cp "$REAL_CADENCE" "$FIXTURE/scripts/ci/qa-round-cadence-gate.sh"
    cp "$REAL_ASSERT"  "$FIXTURE/scripts/ci/qa-round-deployed-sha-assert.sh"
    cp "$REAL_PREPUSH" "$FIXTURE/scripts/hooks/pre-push"
    cp "$REAL_FILTERS" "$FIXTURE/path-filters.yml"
}

git_init_fixture() {
    git init -q
    git config user.email "ci@example.com"
    git config user.name "CI"
    git config commit.gpgsign false
}

commit_file() {
    # commit_file <path> <content> <msg>
    mkdir -p "$(dirname "$1")"
    printf '%s\n' "$2" > "$1"
    git add "$1"
    git commit -q -m "$3"
    git rev-parse HEAD
}

# Standard linear fixture: one file per commit across the group surfaces.
make_fixture() {
    require_tmpdir FIXTURE
    GHENV="$FIXTURE/ghenv"
    copy_infra
    (
        cd "$FIXTURE" || exit 1
        git_init_fixture
        commit_file README.md "root" "root"                                                    > /dev/null; C0="$(git rev-parse HEAD)"
        commit_file frontend/src/x.tsx "export const x = 1" "frontend change"                  > /dev/null; C1="$(git rev-parse HEAD)"
        commit_file backend/internal/service/x.go "package service" "backend change"           > /dev/null; C2="$(git rev-parse HEAD)"
        commit_file backend/internal/synthetic/profiles.go "package synthetic" "seed change"   > /dev/null; C3="$(git rev-parse HEAD)"
        # Judge-irrelevant surfaces (docs + spec + mac-daemon) in one commit.
        mkdir -p docs spec mac-daemon
        printf 'doc\n' > docs/x.md
        printf 'spec\n' > spec/x.yaml
        printf 'swift\n' > mac-daemon/x.swift
        git add docs/x.md spec/x.yaml mac-daemon/x.swift
        git commit -q -m "judge-irrelevant change"; C4="$(git rev-parse HEAD)"
        # Mixed: frontend + docs together.
        mkdir -p frontend/src docs
        printf 'export const y = 2\n' > frontend/src/y.tsx
        printf 'doc y\n' > docs/y.md
        git add frontend/src/y.tsx docs/y.md
        git commit -q -m "mixed change"; C5="$(git rev-parse HEAD)"
        printf '%s\n%s\n%s\n%s\n%s\n%s\n' "$C0" "$C1" "$C2" "$C3" "$C4" "$C5" > shas.txt
    )
    { read -r C0; read -r C1; read -r C2; read -r C3; read -r C4; read -r C5; } < "$FIXTURE/shas.txt"
}

cleanup_fixture() { [ -n "${FIXTURE:-}" ] && rm -rf "$FIXTURE"; }

# ---------------------------------------------------------------------------
# Runners + assertions
# ---------------------------------------------------------------------------

# run_cadence <base> <head> [days_since]: runs the FIXTURE copy in the fixture CWD
# with an isolated GITHUB_ENV; captures stdout/stderr; sets RC. days_since defaults
# to 0 (< the floor) so a not-judge-relevant change SKIPs unless a test asks for a
# staler value; a judge-relevant / uncertain-skip case ignores it.
OUT=""; ERR=""; RC=0
run_cadence() {
    # A PRESENT-but-empty 3rd arg must pass through as "" (not default to 0) so the
    # invalid-DAYS_SINCE skip path is testable; ${3:-0} would collapse "" to 0.
    local days=0
    [ "$#" -ge 3 ] && days="$3"
    : > "$GHENV"
    OUT="$FIXTURE/out.txt"; ERR="$FIXTURE/err.txt"
    ( cd "$FIXTURE" && GITHUB_ENV="$GHENV" bash "$FIXTURE/scripts/ci/qa-round-cadence-gate.sh" "$1" "$2" "$days" ) >"$OUT" 2>"$ERR"
    RC=$?
}

run_cadence_fakegit() {
    : > "$GHENV"
    OUT="$FIXTURE/out.txt"; ERR="$FIXTURE/err.txt"
    ( cd "$FIXTURE" && PATH="$FIXTURE/fakebin:$PATH" GITHUB_ENV="$GHENV" \
        bash "$FIXTURE/scripts/ci/qa-round-cadence-gate.sh" "$1" "$2" 0 ) >"$OUT" 2>"$ERR"
    RC=$?
}

assert_exit0() { if [ "$RC" -eq 0 ]; then ok; else fail "want exit 0, got $RC"; fi; }

# assert_tuple <run_round> <judge_relevant_change> <base_known> <changed_groups>:
# the STDOUT contract must be EXACTLY these four lines in this order — no missing,
# duplicated, extra, reordered, or partial-newline lines. Compared BYTE-FOR-BYTE
# with cmp against an expected file: a `"$(cat)"` string compare strips trailing
# newlines from both sides, so a missing final terminator or an extra blank 5th
# line would slip past it.
assert_tuple() {
    local expf="$FIXTURE/expected_tuple.txt"
    printf 'run_round=%s\njudge_relevant_change=%s\nbase_known=%s\nchanged_groups=%s\n' "$1" "$2" "$3" "$4" > "$expf"
    if cmp -s "$expf" "$OUT"; then ok; else fail "stdout tuple mismatch: $(diff "$expf" "$OUT" 2>&1 | head -5)"; fi
}

# The uncertain-SKIP tuple (exit 0 + exact four-line block). On any uncertainty the
# cadence gate fails toward NOT running: run_round=false, base_known=false.
assert_skip_uncertain() {
    assert_exit0
    assert_tuple false unknown false ""
}

setup_fake_git() {
    mkdir -p "$FIXTURE/fakebin"
    cat > "$FIXTURE/fakebin/git" <<EOF
#!/usr/bin/env bash
# Delegate to real git for everything EXCEPT diff, which fails — simulating a git
# diff error after cat-file already resolved both endpoints.
if [ "\$1" = "diff" ]; then echo "simulated git diff failure" >&2; exit 1; fi
exec "$REAL_GIT" "\$@"
EOF
    chmod +x "$FIXTURE/fakebin/git"
}

# A fake git that FAILS if ANY git-location var leaked into its environment, else
# delegates to the real git. Prepended to PATH with all five vars set in the OUTER
# env, it proves the script UNSET them before calling git (the isolation contract):
# all cleared -> fake git never trips; drop one from the script's unset -> that var
# leaks -> fake git exits non-zero -> the script's git call fails. Covers all five
# uniformly, including inert GIT_INDEX_FILE (tests the contract, not an outcome).
setup_fake_git_isolation() {
    mkdir -p "$FIXTURE/fakebin"
    cat > "$FIXTURE/fakebin/git" <<EOF
#!/usr/bin/env bash
for __v in GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_COMMON_DIR GIT_OBJECT_DIRECTORY; do
  if [ -n "\${!__v:-}" ]; then echo "fake-git: leaked \$__v" >&2; exit 77; fi
done
exec "$REAL_GIT" "\$@"
EOF
    chmod +x "$FIXTURE/fakebin/git"
}

# ===========================================================================
# Cadence decision — clean cases (full stdout tuple asserted every time).
# ===========================================================================
test_frontend_only() {
    echo "test: frontend-only -> run_round=true, changed_groups=frontend"
    make_fixture
    run_cadence "$C0" "$C1"
    assert_exit0
    assert_tuple true true true frontend
    cleanup_fixture
}

test_backend_only() {
    echo "test: backend-only -> run_round=true, changed_groups=backend"
    make_fixture
    run_cadence "$C1" "$C2"
    assert_exit0
    assert_tuple true true true backend
    cleanup_fixture
}

test_seed_only() {
    echo "test: seed-only -> changed_groups=backend,seed (sorted csv, both match)"
    make_fixture
    run_cadence "$C2" "$C3"
    assert_exit0
    assert_tuple true true true "backend,seed"
    cleanup_fixture
}

test_judge_irrelevant() {
    echo "test: judge-irrelevant (docs+spec+mac-daemon) -> run_round=false, changed_groups empty"
    make_fixture
    run_cadence "$C3" "$C4"
    assert_exit0
    assert_tuple false false true ""
    cleanup_fixture
}

test_base_equals_head() {
    echo "test: BASE==HEAD (no staging change) -> SKIP regardless of the staleness floor"
    make_fixture
    run_cadence "$C1" "$C1"
    assert_exit0
    assert_tuple false false true ""
    # A very stale DAYS_SINCE must NOT force a run when nothing changed.
    run_cadence "$C1" "$C1" 999
    assert_exit0
    assert_tuple false false true ""
    # An INVALID equal pair (two identical UNRESOLVABLE refs) is NOT a confirmed
    # "unchanged" -> it must uncertain-skip, not clean-skip.
    run_cadence "$ABSENT_SHA40" "$ABSENT_SHA40" 5
    assert_skip_uncertain
    cleanup_fixture
}

test_staleness_floor() {
    echo "test: staleness floor (changed but not judge-relevant) -> DAYS_SINCE decides"
    make_fixture
    # C3..C4 is a docs/spec/mac-daemon change: staging changed, but NOT judge-relevant.
    # DAYS_SINCE < 7 -> skip; >= 7 -> run (floor); invalid -> uncertain skip.
    run_cadence "$C3" "$C4" 6;  assert_exit0; assert_tuple false false true ""
    run_cadence "$C3" "$C4" 7;  assert_exit0; assert_tuple true false true ""
    run_cadence "$C3" "$C4" 8;  assert_exit0; assert_tuple true false true ""
    run_cadence "$C3" "$C4" 0;  assert_exit0; assert_tuple false false true ""
    # Invalid / unresolvable DAYS_SINCE while it IS needed (not judge-relevant) ->
    # uncertain skip.
    run_cadence "$C3" "$C4" "notaninteger"; assert_skip_uncertain
    run_cadence "$C3" "$C4" "-1";           assert_skip_uncertain
    run_cadence "$C3" "$C4" "";             assert_skip_uncertain
    # Out-of-range (int64-overflowing) DAYS_SINCE: `^[0-9]+$` would accept it but the
    # `-ge` comparison errors and would fall through as a clean within-floor skip; the
    # length cap routes it to uncertain-skip instead.
    run_cadence "$C3" "$C4" "99999999999999999999"; assert_skip_uncertain
    # A judge-relevant change RUNs regardless of DAYS_SINCE (fast path, floor moot) --
    # WITH A VALID DAYS_SINCE...
    run_cadence "$C1" "$C2" 0;  assert_exit0; assert_tuple true true true backend
    # ...AND with an INVALID/empty DAYS_SINCE: it still RUNs, which proves DAYS_SINCE
    # is validated LAZILY (only in the not-relevant branch). Eager validation would
    # wrongly uncertain-skip here.
    run_cadence "$C1" "$C2" "abc"; assert_exit0; assert_tuple true true true backend
    run_cadence "$C1" "$C2" "";    assert_exit0; assert_tuple true true true backend
    cleanup_fixture
}

test_mixed_frontend_and_docs() {
    echo "test: mixed (frontend + docs) -> run_round=true, changed_groups=frontend"
    make_fixture
    run_cadence "$C4" "$C5"
    assert_exit0
    assert_tuple true true true frontend
    cleanup_fixture
}

test_rename_to_irrelevant_dest() {
    echo "test: rename backend->docs surfaces the backend SOURCE (--no-renames) -> run_round=true"
    require_tmpdir FIXTURE
    GHENV="$FIXTURE/ghenv"
    copy_infra
    (
        cd "$FIXTURE" || exit 1
        git_init_fixture
        commit_file README.md "root" "root" > /dev/null
        # A backend file with enough content that a pure move is detected as a 100%
        # rename by git's default detection (which collapses to the destination).
        mkdir -p backend/internal/service
        printf 'package service\n// l1\n// l2\n// l3\n// l4\n' > backend/internal/service/ren.go
        git add backend/internal/service/ren.go
        git commit -q -m "add backend file"
        RBASE="$(git rev-parse HEAD)"
        mkdir -p docs
        git mv backend/internal/service/ren.go docs/ren.go
        git commit -q -m "rename backend file to docs"
        RHEAD="$(git rev-parse HEAD)"
        printf '%s\n%s\n' "$RBASE" "$RHEAD" > shas.txt
    )
    { read -r RBASE; read -r RHEAD; } < "$FIXTURE/shas.txt"
    run_cadence "$RBASE" "$RHEAD"
    assert_exit0
    # Default rename detection would show only docs/ren.go (irrelevant) -> false;
    # --no-renames surfaces the deleted backend source -> run_round=true.
    assert_tuple true true true backend
    cleanup_fixture
}

# ===========================================================================
# Uncertainty -> SKIP suite.
# ===========================================================================
test_empty_base() {
    echo "test: empty BASE -> uncertain skip"
    make_fixture
    run_cadence "" "$C3"
    assert_skip_uncertain
    cleanup_fixture
}

test_empty_head() {
    echo "test: empty HEAD -> uncertain skip"
    make_fixture
    run_cadence "$C0" ""
    assert_skip_uncertain
    cleanup_fixture
}

test_unresolvable_base() {
    echo "test: unresolvable BASE sha -> uncertain skip"
    make_fixture
    run_cadence "$ABSENT_SHA40" "$C3"
    assert_skip_uncertain
    cleanup_fixture
}

test_unresolvable_head() {
    echo "test: unresolvable HEAD sha -> uncertain skip"
    make_fixture
    run_cadence "$C0" "$ABSENT_SHA40"
    assert_skip_uncertain
    cleanup_fixture
}

test_missing_filters() {
    echo "test: missing path-filters.yml -> uncertain skip"
    make_fixture
    rm -f "$FIXTURE/path-filters.yml"
    run_cadence "$C3" "$C4"
    assert_skip_uncertain
    cleanup_fixture
}

test_unreadable_filters() {
    echo "test: unreadable path-filters.yml -> uncertain skip"
    if [ "$(id -u)" -eq 0 ]; then
        echo "  SKIP: running as root, chmod 000 stays readable"
        return
    fi
    make_fixture
    chmod 000 "$FIXTURE/path-filters.yml"
    run_cadence "$C3" "$C4"
    assert_skip_uncertain
    chmod 644 "$FIXTURE/path-filters.yml"
    cleanup_fixture
}

test_file_in_group_absent() {
    echo "test: file_in_group absent (sourceable stub, function missing) -> uncertain skip"
    make_fixture
    printf '# stub pre-push without file_in_group\n' > "$FIXTURE/scripts/hooks/pre-push"
    run_cadence "$C2" "$C3"
    assert_skip_uncertain
    cleanup_fixture
}

test_filter_removed_mid_match() {
    echo "test: path-filters.yml disappearing DURING matching -> uncertain skip (not a false floor RUN)"
    make_fixture
    # A stub file_in_group that deletes the filter file on its first call and returns
    # no-match: the pre-loop readability check passes, but the post-loop re-check must
    # catch the disappearance. Not-relevant range + DAYS>=7 so WITHOUT the re-check it
    # would reach the floor RUN. The single quotes are intentional — $FILTERS_FILE must
    # land LITERALLY in the stub so the stub expands the gate's global at run time.
    # shellcheck disable=SC2016
    printf 'file_in_group() { rm -f "$FILTERS_FILE" 2>/dev/null; return 1; }\n' > "$FIXTURE/scripts/hooks/pre-push"
    run_cadence "$C3" "$C4" 7
    assert_skip_uncertain
    cleanup_fixture
}

test_filter_replaced_by_dir_mid_match() {
    echo "test: path-filters.yml replaced by a readable DIRECTORY mid-match -> uncertain skip"
    make_fixture
    # A stub file_in_group that replaces the filter FILE with a readable DIRECTORY on
    # its first call: a bare `-r` post-check would PASS (dirs are readable) and reach
    # the floor RUN; the `-f && -r` re-check catches that it is no longer a regular file.
    # shellcheck disable=SC2016
    printf 'file_in_group() { rm -f "$FILTERS_FILE" 2>/dev/null; mkdir -p "$FILTERS_FILE" 2>/dev/null; return 1; }\n' > "$FIXTURE/scripts/hooks/pre-push"
    run_cadence "$C3" "$C4" 7
    assert_skip_uncertain
    cleanup_fixture
}

test_require_tmpdir_fails_closed() {
    echo "test: require_tmpdir aborts (non-zero) when mktemp fails (fixture guard is fail-closed)"
    # Run in a subshell with mktemp forced to fail; require_tmpdir must exit non-zero
    # rather than proceed with an empty dir (which would corrupt the real repo).
    if ( mktemp() { return 1; }; require_tmpdir X ) >/dev/null 2>&1; then
        fail "require_tmpdir must abort on mktemp failure"
    else
        ok
    fi
}

test_prepush_unsourceable() {
    echo "test: pre-push UNSOURCEABLE (parse error) -> uncertain skip (distinct from function-absent)"
    make_fixture
    # A parse error makes `source` itself fail (non-zero) before any function is
    # defined — exercises the `source ... || skip_uncertain` path, not the declare -f one.
    printf 'file_in_group() {\n  # unterminated function body — parse error\n' > "$FIXTURE/scripts/hooks/pre-push"
    run_cadence "$C2" "$C3"
    assert_skip_uncertain
    cleanup_fixture
}

test_git_diff_failure() {
    echo "test: git diff failure (cat-file passes, diff fails) -> uncertain skip"
    make_fixture
    setup_fake_git
    run_cadence_fakegit "$C2" "$C3"
    assert_skip_uncertain
    cleanup_fixture
}

test_github_env_write_success() {
    echo "test: a successful run writes the SAME decision to \$GITHUB_ENV (not just stdout)"
    make_fixture
    run_cadence "$C1" "$C2" 0          # backend -> RUN
    assert_exit0
    assert_tuple true true true backend  # stdout contract
    # The wrapper reads GITHUB_ENV, NOT stdout — assert emit actually wrote the four
    # key=value lines there, byte-for-byte, on a successful run.
    local expf="$FIXTURE/expected_ghenv.txt"
    printf 'run_round=true\njudge_relevant_change=true\nbase_known=true\nchanged_groups=backend\n' > "$expf"
    if cmp -s "$expf" "$GHENV"; then ok; else fail "GITHUB_ENV mismatch: $(diff "$expf" "$GHENV" 2>&1 | head -5)"; fi
    cleanup_fixture
}

test_github_env_write_failure() {
    echo "test: unwritable GITHUB_ENV -> fail VISIBLY (non-zero), not a silent exit-0"
    make_fixture
    # In CI the wrapper reads the decision from GITHUB_ENV; a failed append means the
    # decision was lost, so the gate must exit non-zero rather than exit 0 having
    # written only to stdout. Point GITHUB_ENV at a path whose parent doesn't exist.
    OUT="$FIXTURE/out.txt"; ERR="$FIXTURE/err.txt"
    ( cd "$FIXTURE" && GITHUB_ENV="$FIXTURE/no-such-dir/ghenv" \
        bash "$FIXTURE/scripts/ci/qa-round-cadence-gate.sh" "$C1" "$C2" 0 ) >"$OUT" 2>"$ERR"
    RC=$?
    if [ "$RC" -ne 0 ]; then ok; else fail "want non-zero exit on GITHUB_ENV write failure, got $RC"; fi
    cleanup_fixture
}

test_unwritable_tmpdir() {
    echo "test: unusable temp dir must uncertain-SKIP (never a truncated changed-file list -> run_round=false)"
    make_fixture
    # The changed-file list is buffered through a TMPDIR-derived mktemp; a broken
    # temp dir makes mktemp fail -> the status-checked `|| skip_uncertain` fires. Point
    # TMPDIR at a nonexistent directory so mktemp's template dir doesn't exist (fails
    # on both macOS and Linux, since the template path is explicit). A backend range
    # would otherwise RUN normally, so an uncertain skip (base_known=false) proves the temp
    # failure is caught rather than silently dropping the list.
    : > "$GHENV"
    OUT="$FIXTURE/out.txt"; ERR="$FIXTURE/err.txt"
    ( cd "$FIXTURE" && TMPDIR="$FIXTURE/nonexistent-tmp-$$" GITHUB_ENV="$GHENV" \
        bash "$FIXTURE/scripts/ci/qa-round-cadence-gate.sh" "$C1" "$C2" 0 ) >"$OUT" 2>"$ERR"
    RC=$?
    assert_skip_uncertain
    cleanup_fixture
}

test_special_char_path() {
    echo "test: a changed path with a TAB is matched (git diff -z, no quoting) -> run_round=true"
    require_tmpdir FIXTURE
    GHENV="$FIXTURE/ghenv"
    copy_infra
    (
        cd "$FIXTURE" || exit 1
        git_init_fixture
        commit_file README.md "root" "root" > /dev/null
        TBASE="$(git rev-parse HEAD)"
        # A real tab character in a backend filename. Without -z, git quotes this to
        # "backend/.../a\tb.go", which fails the matcher's ^backend/ anchor. Stage
        # ONLY the tabbed path (NOT `git add -A`) so the copied infra under scripts/**
        # doesn't independently match backend and mask the -z behavior.
        mkdir -p backend/internal/service
        tabpath="$(printf 'backend/internal/service/a\tb.go')"
        printf 'package service\n' > "$tabpath"
        git add "$tabpath"
        git commit -q -m "backend file with a tab in its name"
        THEAD="$(git rev-parse HEAD)"
        printf '%s\n%s\n' "$TBASE" "$THEAD" > shas.txt
    )
    { read -r TBASE; read -r THEAD; } < "$FIXTURE/shas.txt"
    run_cadence "$TBASE" "$THEAD"
    assert_exit0
    assert_tuple true true true backend
    cleanup_fixture
}

# ===========================================================================
# Two-dot divergent-history proof: two-dot and three-dot yield DIFFERENT
# changed paths; the script must follow TWO-DOT.
# ===========================================================================
test_two_dot_divergent() {
    echo "test: divergent history -> decision matches TWO-DOT diff (frontend on BASE side)"
    require_tmpdir FIXTURE
    GHENV="$FIXTURE/ghenv"
    copy_infra
    (
        cd "$FIXTURE" || exit 1
        git_init_fixture
        commit_file README.md "root" "root" > /dev/null
        root_sha="$(git rev-parse HEAD)"
        # BASE side: a judge-relevant (frontend) file that exists ONLY on this side.
        git checkout -q -b base_side
        commit_file frontend/src/only_base.tsx "export const b = 1" "frontend on base side" > /dev/null
        DBASE="$(git rev-parse HEAD)"
        # HEAD side from the same root: a docs-only (irrelevant) file.
        git checkout -q -b head_side "$root_sha"
        commit_file docs/only_head.md "head doc" "docs on head side" > /dev/null
        DHEAD="$(git rev-parse HEAD)"
        printf '%s\n%s\n' "$DBASE" "$DHEAD" > shas.txt
    )
    { read -r DBASE; read -r DHEAD; } < "$FIXTURE/shas.txt"
    # Two-dot DBASE..DHEAD includes frontend/src/only_base.tsx (a removal) -> RUN.
    # Three-dot DBASE...DHEAD (merge-base C0 -> DHEAD) is docs-only -> would be false.
    run_cadence "$DBASE" "$DHEAD"
    assert_exit0
    assert_tuple true true true frontend
    cleanup_fixture
}

# ===========================================================================
# Source-guard + git-env isolation.
# ===========================================================================
test_source_guard_isolation() {
    echo "test: sourcing pre-push runs no hook body + leaves the real repo untouched"
    make_fixture
    run_cadence "$C0" "$C1"
    assert_exit0
    # A clean four-line emit proves the source-guard returned before the hook body.
    assert_tuple true true true frontend
    # The hook body (had it executed) would emit its phase/lint/review markers.
    if grep -Eq '\[lint\]|\[review\]|\[test\]|phase|Running (deploy|lint|tests)' "$OUT" "$ERR"; then
        fail "hook body appears to have executed (found phase/lint markers)"
    else
        ok
    fi
    # The real repo's copied assets must be byte-unchanged (tests only touch copies).
    local dirty
    dirty="$(git -C "$REPO_ROOT" status --porcelain -- path-filters.yml scripts/hooks/pre-push 2>/dev/null)"
    if [ -z "$dirty" ]; then ok; else fail "real repo assets modified by tests: $dirty"; fi
    cleanup_fixture
}

# ===========================================================================
# Deployed-SHA assertion suite (same fixture repo).
# ===========================================================================
run_assert() {
    OUT="$FIXTURE/aout.txt"; ERR="$FIXTURE/aerr.txt"
    ( cd "$FIXTURE" && bash "$FIXTURE/scripts/ci/qa-round-deployed-sha-assert.sh" "$@" ) >"$OUT" 2>"$ERR"
    RC=$?
}

# run_assert_from <cwd> <args...>: same, but from an arbitrary cwd — proves the
# script resolves its repo from its own location, not the caller's cwd.
run_assert_from() {
    local cwd="$1"; shift
    OUT="$FIXTURE/aout.txt"; ERR="$FIXTURE/aerr.txt"
    ( cd "$cwd" && bash "$FIXTURE/scripts/ci/qa-round-deployed-sha-assert.sh" "$@" ) >"$OUT" 2>"$ERR"
    RC=$?
}

# run_assert_gitdir <gitdir> <args...>: run with a poisoned GIT_DIR to prove the
# script ignores inherited git-location env (resolves its OWN repo, not GIT_DIR's).
run_assert_gitdir() {
    local gitdir="$1"; shift
    OUT="$FIXTURE/aout.txt"; ERR="$FIXTURE/aerr.txt"
    ( cd "$FIXTURE" && GIT_DIR="$gitdir" bash "$FIXTURE/scripts/ci/qa-round-deployed-sha-assert.sh" "$@" ) >"$OUT" 2>"$ERR"
    RC=$?
}

assert_rc_zero()    { if [ "$RC" -eq 0 ]; then ok; else fail "want exit 0, got $RC ($1)"; fi; }
assert_rc_nonzero() { if [ "$RC" -ne 0 ]; then ok; else fail "want non-zero exit ($1)"; fi; }

upper_sha() { printf '%s' "$1" | tr 'a-f' 'A-F'; }

test_deployed_sha_pin() {
    echo "test: deployed-sha pre-tour pin — exact HEAD match exits 0; others fail-closed"
    make_fixture
    local head; head="$C5"
    local before after

    run_assert "$head";              assert_rc_zero "exact HEAD"
    before="$(git -C "$FIXTURE" rev-parse HEAD)"
    run_assert "$ABSENT_SHA40";      assert_rc_nonzero "different valid sha"
    after="$(git -C "$FIXTURE" rev-parse HEAD)"
    if [ "$before" = "$after" ]; then ok; else fail "assert must not change HEAD"; fi
    # An EXISTING but different commit must also fail — proves equality, not mere
    # existence/resolvability, is the check (C0 resolves but != HEAD C5).
    run_assert "$C0";                assert_rc_nonzero "existing but different commit"
    run_assert "nothex";             assert_rc_nonzero "malformed sha"
    run_assert "abc123";             assert_rc_nonzero "short sha"
    run_assert "$(upper_sha "$head")"; assert_rc_nonzero "uppercase sha"
    run_assert;                      assert_rc_nonzero "zero args"
    run_assert "$head" x y;          assert_rc_nonzero "three args"
    # Repo is resolved from the SCRIPT location, not the caller's cwd: an exact match
    # invoked from a non-repo cwd still passes (a cwd-based resolver would fail here).
    local nonrepo; require_tmpdir nonrepo
    run_assert_from "$nonrepo" "$head"; assert_rc_zero "resolves repo from script location, not cwd"
    rm -rf "$nonrepo"
    cleanup_fixture
}

# write_manifest <path> <json>: raw JSON so invalid-JSON cases are expressible.
write_manifest() { printf '%s' "$2" > "$1"; }

test_deployed_sha_manifest() {
    echo "test: deployed-sha post-tour manifest — exact .gitSha exits 0; malformed fail-closed"
    make_fixture
    local head m; head="$C5"; m="$FIXTURE/manifest.json"

    write_manifest "$m" "{\"gitSha\":\"$head\"}"
    run_assert "$head" "$m";                     assert_rc_zero "matching manifest"

    run_assert "$head" "$FIXTURE/nope.json";     assert_rc_nonzero "missing manifest"

    write_manifest "$m" "not json"
    run_assert "$head" "$m";                     assert_rc_nonzero "invalid json"

    write_manifest "$m" "{\"foo\":1}"
    run_assert "$head" "$m";                     assert_rc_nonzero "missing .gitSha"

    write_manifest "$m" "{\"gitSha\":123}"
    run_assert "$head" "$m";                     assert_rc_nonzero "non-string .gitSha"

    write_manifest "$m" "{\"gitSha\":\"unknown\"}"
    run_assert "$head" "$m";                     assert_rc_nonzero "'unknown' .gitSha"

    write_manifest "$m" "{\"gitSha\":\"abc\"}"
    run_assert "$head" "$m";                     assert_rc_nonzero "short .gitSha"

    write_manifest "$m" "{\"gitSha\":\"$(upper_sha "$head")\"}"
    run_assert "$head" "$m";                     assert_rc_nonzero "uppercase .gitSha"

    # A trailing newline (or leading space) makes .gitSha NOT exactly 40 hex, but
    # shell command substitution would strip the trailing newline before a bash
    # regex saw it — so the format check must live inside jq (anchored \A..\z).
    write_manifest "$m" "{\"gitSha\":\"${head}\\n\"}"
    run_assert "$head" "$m";                     assert_rc_nonzero "trailing-newline .gitSha"
    write_manifest "$m" "{\"gitSha\":\" ${head}\"}"
    run_assert "$head" "$m";                     assert_rc_nonzero "leading-space .gitSha"

    # A stream of MORE THAN ONE top-level JSON value must be rejected even if a later
    # value is valid — a jq extract without --slurp would take the last value and pass.
    write_manifest "$m" "{\"gitSha\":\"unknown\"} {\"gitSha\":\"${head}\"}"
    run_assert "$head" "$m";                     assert_rc_nonzero "multi-value manifest stream rejected"

    write_manifest "$m" "{\"gitSha\":\"$ABSENT_SHA40\"}"
    run_assert "$head" "$m";                     assert_rc_nonzero "different valid .gitSha"

    # A matching manifest must NOT mask a stale checkout. Use an EXISTING non-HEAD
    # commit (C0) as the deployed sha with a manifest that agrees (gitSha=C0): the
    # checkout check (HEAD=C5 != C0) must still fail. Using an existing commit proves
    # the gate is HEAD==DEPLOYED, not merely "DEPLOYED resolves + manifest agrees".
    write_manifest "$m" "{\"gitSha\":\"$C0\"}"
    run_assert "$C0" "$m";                       assert_rc_nonzero "matching manifest cannot mask stale checkout (existing non-HEAD commit)"

    # A supplied-but-EMPTY manifest arg (two args) must still trigger + fail the
    # manifest assertion, not silently pass as if no manifest were requested.
    run_assert "$head" "";                       assert_rc_nonzero "empty manifest arg still asserts"

    # Unreadable manifest fails closed (skip under root: chmod 000 stays readable).
    if [ "$(id -u)" -ne 0 ]; then
        write_manifest "$m" "{\"gitSha\":\"$head\"}"
        chmod 000 "$m"
        run_assert "$head" "$m";                 assert_rc_nonzero "unreadable manifest"
        chmod 644 "$m"
    fi

    # A successful manifest assertion must NOT mutate the manifest file.
    write_manifest "$m" "{\"gitSha\":\"$head\"}"
    local before_sum after_sum
    before_sum="$(cksum < "$m")"
    run_assert "$head" "$m";                     assert_rc_zero "manifest match (byte-preservation)"
    after_sum="$(cksum < "$m")"
    if [ "$before_sum" = "$after_sum" ]; then ok; else fail "assert mutated the manifest"; fi
    cleanup_fixture
}

test_deployed_sha_manifest_dash_name() {
    echo "test: a manifest file named '-' is read as a FILE, not stdin (jq operand injection guard)"
    make_fixture
    local head; head="$C5"
    # Malformed manifest file literally named '-'; a VALID manifest piped on stdin. The
    # jq OPERAND form (jq ... '-') would read stdin and PASS; the redirect form reads
    # the malformed file and FAILS. This integrity gate must not be foolable.
    printf '{"gitSha":"unknown"}' > "$FIXTURE/-"
    OUT="$FIXTURE/aout.txt"; ERR="$FIXTURE/aerr.txt"
    ( cd "$FIXTURE" && printf '{"gitSha":"%s"}' "$head" \
        | bash "$FIXTURE/scripts/ci/qa-round-deployed-sha-assert.sh" "$head" "-" ) >"$OUT" 2>"$ERR"
    RC=$?
    if [ "$RC" -ne 0 ]; then ok; else fail "manifest named '-' must be read as a file, not stdin"; fi
    rm -f "$FIXTURE/-"
    cleanup_fixture
}

test_deployed_sha_ignores_poisoned_git_env() {
    echo "test: poisoned GIT_DIR must not let the assert accept another repo's HEAD"
    make_fixture   # HEAD = C5
    local poison psha
    require_tmpdir poison
    (
        cd "$poison" || exit 1
        git init -q
        git config user.email "ci@example.com"; git config user.name "CI"; git config commit.gpgsign false
        printf 'x\n' > f; git add f; git commit -q -m poison
    )
    psha="$(git -C "$poison" rev-parse HEAD)"
    # With GIT_DIR poisoned, asserting the poison repo's HEAD must FAIL (the script
    # resolves the fixture at C5, not the poison repo) — a buggy git-honoring script
    # would resolve psha and pass.
    run_assert_gitdir "$poison/.git" "$psha";        assert_rc_nonzero "poisoned GIT_DIR ignored (checkout)"
    # A manifest agreeing with the poison sha still can't rescue it.
    local pm="$FIXTURE/poison_manifest.json"
    write_manifest "$pm" "{\"gitSha\":\"$psha\"}"
    run_assert_gitdir "$poison/.git" "$psha" "$pm";  assert_rc_nonzero "poisoned GIT_DIR ignored (manifest)"
    # Sanity: the fixture's real HEAD still passes under the poison env, proving the
    # script resolved the fixture (C5), not the poison repo. (Per-var coverage for all
    # five git-location vars is the isolation-contract test below.)
    run_assert_gitdir "$poison/.git" "$C5";          assert_rc_zero "correct HEAD passes despite poison env"
    rm -rf "$poison"
    cleanup_fixture
}

test_assert_isolates_git_env() {
    echo "test: the assert unsets ALL git-location vars before calling git (isolation contract)"
    make_fixture
    setup_fake_git_isolation
    OUT="$FIXTURE/aout.txt"; ERR="$FIXTURE/aerr.txt"
    # All five vars set in the OUTER env + a fake git that fails on ANY leaked var: the
    # correct HEAD (C5) must still resolve (rc 0), proving the script cleared them all
    # before calling git. Dropping any one var from the unset line reddens this.
    ( cd "$FIXTURE" && PATH="$FIXTURE/fakebin:$PATH" \
        GIT_DIR=/x GIT_WORK_TREE=/x GIT_INDEX_FILE=/x GIT_COMMON_DIR=/x GIT_OBJECT_DIRECTORY=/x \
        bash "$FIXTURE/scripts/ci/qa-round-deployed-sha-assert.sh" "$C5" ) >"$OUT" 2>"$ERR"
    RC=$?
    if [ "$RC" -eq 0 ]; then ok; else fail "assert leaked a git-location var to git (fake git tripped)"; fi
    cleanup_fixture
}

test_cadence_ignores_poisoned_git_env() {
    echo "test: a poisoned git-location env must not make the cadence gate diff the wrong repo"
    make_fixture
    local poison
    require_tmpdir poison
    (
        cd "$poison" || exit 1
        git init -q
        git config user.email "ci@example.com"; git config user.name "CI"; git config commit.gpgsign false
        printf 'x\n' > f; git add f; git commit -q -m poison
    )
    # GIT_DIR poisoned with a valid OTHER repo: without the unset, cat-file for the
    # fixture shas (absent from the poison repo) would fail and the gate would
    # uncertain-skip (base_known=false). The normal backend tuple (base_known=true)
    # proves it resolved the FIXTURE repo. (Per-var coverage for all five vars is the
    # isolation-contract test below.)
    : > "$GHENV"
    OUT="$FIXTURE/out.txt"; ERR="$FIXTURE/err.txt"
    ( cd "$FIXTURE" && env "GIT_DIR=$poison/.git" GITHUB_ENV="$GHENV" \
        bash "$FIXTURE/scripts/ci/qa-round-cadence-gate.sh" "$C1" "$C2" 0 ) >"$OUT" 2>"$ERR"
    RC=$?
    assert_exit0
    assert_tuple true true true backend
    rm -rf "$poison"
    cleanup_fixture
}

test_cadence_isolates_git_env() {
    echo "test: the cadence gate unsets ALL git-location vars before calling git (isolation contract)"
    make_fixture
    setup_fake_git_isolation
    : > "$GHENV"
    OUT="$FIXTURE/out.txt"; ERR="$FIXTURE/err.txt"
    # All five vars set in the OUTER env + a fake git that fails on ANY leaked var: a
    # relevant backend range must still RUN (true/true/true/backend), proving the gate
    # cleared them all before calling git. Dropping any one var from the unset reddens.
    ( cd "$FIXTURE" && PATH="$FIXTURE/fakebin:$PATH" \
        GIT_DIR=/x GIT_WORK_TREE=/x GIT_INDEX_FILE=/x GIT_COMMON_DIR=/x GIT_OBJECT_DIRECTORY=/x \
        GITHUB_ENV="$GHENV" bash "$FIXTURE/scripts/ci/qa-round-cadence-gate.sh" "$C1" "$C2" 0 ) >"$OUT" 2>"$ERR"
    RC=$?
    assert_exit0
    assert_tuple true true true backend
    cleanup_fixture
}

# ---------------------------------------------------------------------------
main() {
    test_frontend_only
    test_backend_only
    test_seed_only
    test_judge_irrelevant
    test_base_equals_head
    test_staleness_floor
    test_mixed_frontend_and_docs
    test_rename_to_irrelevant_dest
    test_special_char_path

    test_empty_base
    test_empty_head
    test_unresolvable_base
    test_unresolvable_head
    test_missing_filters
    test_unreadable_filters
    test_file_in_group_absent
    test_filter_removed_mid_match
    test_filter_replaced_by_dir_mid_match
    test_require_tmpdir_fails_closed
    test_prepush_unsourceable
    test_git_diff_failure
    test_github_env_write_success
    test_github_env_write_failure
    test_unwritable_tmpdir
    test_cadence_ignores_poisoned_git_env
    test_cadence_isolates_git_env

    test_two_dot_divergent
    test_source_guard_isolation

    test_deployed_sha_pin
    test_deployed_sha_manifest
    test_deployed_sha_manifest_dash_name
    test_deployed_sha_ignores_poisoned_git_env
    test_assert_isolates_git_env

    echo ""
    echo "===================="
    echo "PASS=$PASS FAIL=$FAIL"
    echo "===================="
    [ "$FAIL" -eq 0 ]
}

main "$@"
