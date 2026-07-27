#!/bin/bash
# Unit tests for the L3 phase classifier + rc-file failure propagation in
# scripts/hooks/pre-push. Sources the hook (source-guard prevents the body from
# running) and asserts the pure functions directly. No real push, no real tests.
set -u
cd "$(dirname "${BASH_SOURCE[0]}")/../../.." || exit 1   # repo root
source scripts/hooks/pre-push                            # source-guard prevents hook body

fail=0
assert_eq() {
  # assert_eq <desc> <expected> <actual>
  if [[ "$2" == "$3" ]]; then echo "ok: $1"; else echo "FAIL: $1 (expected '$2', got '$3')"; fail=1; fi
}

# --- classify_command: lane assignment ---
assert_eq "test-e2e-diff -> E2E (exclusive)"             "E2E"        "$(classify_command 'make test-e2e-diff')"
assert_eq "test-e2e -> E2E"                              "E2E"        "$(classify_command 'make test-e2e')"
assert_eq "test-frontend -> CONCURRENT"                 "CONCURRENT" "$(classify_command 'make test-frontend')"
assert_eq "test-pre-push-filters -> CONCURRENT"         "CONCURRENT" "$(classify_command 'bash scripts/hooks/test/test-pre-push-filters.sh')"
assert_eq "test-deploy-scripts -> CONCURRENT"           "CONCURRENT" "$(classify_command 'make test-deploy-scripts')"
assert_eq "test-unit -> GO"                             "GO"         "$(classify_command 'make test-unit')"
assert_eq "test-integration -> GO"                      "GO"         "$(classify_command 'make test-integration')"
assert_eq "test-integration-fast -> GO"                 "GO"         "$(classify_command 'make test-integration-fast')"
# Unrecognized command defaults to the GO (serial, DB-owning) lane — never concurrent.
assert_eq "unrecognized command -> GO (safe default)"   "GO"         "$(classify_command 'make some-future-db-command')"

# --- refs_all_target_main: the promotion-to-main skip predicate ---
# Git feeds pushed refs on stdin as `<local_ref> <local_sha> <remote_ref>
# <remote_sha>`. 0 (true) only when EVERY remote_ref is refs/heads/main.
rc_of() { "$@"; echo $?; }   # capture a predicate's exit code as a value
assert_eq "develop:main promotion -> skip (0)"          "0" "$(rc_of refs_all_target_main 'refs/heads/develop a refs/heads/main b')"
assert_eq "all-main multi-ref -> skip (0)"              "0" "$(rc_of refs_all_target_main $'refs/heads/a x refs/heads/main y\nrefs/heads/b x refs/heads/main y')"
assert_eq "feature-branch push -> run checks (1)"       "1" "$(rc_of refs_all_target_main 'refs/heads/feat/x a refs/heads/feat/x b')"
# A mixed push that includes any non-main ref must NOT skip.
assert_eq "mixed (main + feat) -> run checks (1)"       "1" "$(rc_of refs_all_target_main $'refs/heads/develop a refs/heads/main b\nrefs/heads/feat a refs/heads/feat b')"
# Empty stdin must NOT skip.
assert_eq "empty stdin -> run checks (1)"               "1" "$(rc_of refs_all_target_main '')"

# --- full-hook integration: piping git's stdin exercises the real skip branch ---
# Positive: a develop:main promotion line makes the hook exit 0 WITHOUT printing
# the "Running checks" banner (i.e. it skips before any phase runs).
hook_out_promotion=$(printf 'refs/heads/develop %s refs/heads/main %s\n' "$(git rev-parse HEAD)" "$(git rev-parse HEAD)" | bash scripts/hooks/pre-push 2>&1)
hook_rc_promotion=$?
assert_eq "promotion push -> hook exits 0"              "0" "$hook_rc_promotion"
assert_eq "promotion push -> NO 'Running checks' banner" "0" "$(grep -c 'Running checks' <<<"$hook_out_promotion")"
assert_eq "promotion push -> emits skip message"        "1" "$(grep -c 'promotion to main' <<<"$hook_out_promotion")"
# Negative full-hook coverage is provided by the refs_all_target_main predicate
# assertions above (feat / mixed / empty all -> 1 = run checks): the hook calls
# that exact predicate, so a non-promotion push enters the normal flow. We do NOT
# pipe a feature-branch line through the full hook here — that would proceed past
# the skip into the real (slow) phases whenever a HEAD review log happens to exist.

# --- run_phase: writes rc-file + buffers log; does NOT self-background ---
tmpd=$(mktemp -d) || { echo "FAIL: mktemp -d failed"; exit 1; }

run_phase "$tmpd/ok.log" "$tmpd/ok.rc" "true"
assert_eq "run_phase success rc == 0"                   "0"          "$(cat "$tmpd/ok.rc")"

# Use a child process (not a bare `exit`, which would terminate the subshell
# before the rc is written). Real phase commands are always make/bash/external
# invocations that return an exit code rather than exiting the phase subshell.
run_phase "$tmpd/bad.log" "$tmpd/bad.rc" "bash -c 'exit 7'"
assert_eq "run_phase failure rc preserved (7)"          "7"          "$(cat "$tmpd/bad.rc")"

run_phase "$tmpd/echo.log" "$tmpd/echo.rc" "echo hello-from-phase"
assert_eq "run_phase buffers stdout to the log"         "hello-from-phase" "$(cat "$tmpd/echo.log")"

# `cd` inside a phase must NOT leak into the parent (isolating subshell).
cwd_before=$(pwd)
run_phase "$tmpd/cd.log" "$tmpd/cd.rc" "cd /tmp"
assert_eq "run_phase cd does not leak into parent cwd"  "$cwd_before" "$(pwd)"

# --- rc-file aggregation under REAL set -e: a failing background phase fails ---
# Replicate the orchestrator's aggregation loop under `set -e` (the source-guard
# bypasses the hook's own `set -e`, so the harness enables it to exercise the
# real condition).
aggregate_rcs() {
  local rcs=("$@") failed=0 rcfile rcval
  set +e
  for rcfile in "${rcs[@]}"; do
    rcval=$(cat "$rcfile" 2>/dev/null || echo 1)
    [[ "$rcval" =~ ^[0-9]+$ ]] || rcval=1
    [[ "$rcval" -ne 0 ]] && failed=1
  done
  set -e
  echo "$failed"
}

# All-pass background phases via run_phase + & + wait (the real launch shape).
set -e
run_phase "$tmpd/p1.log" "$tmpd/p1.rc" "true" &
run_phase "$tmpd/p2.log" "$tmpd/p2.rc" "true" &
wait
assert_eq "all-pass background phases -> aggregate 0"   "0" "$(aggregate_rcs "$tmpd/p1.rc" "$tmpd/p2.rc")"

# A failing required background phase -> aggregate FAIL (1).
run_phase "$tmpd/f1.log" "$tmpd/f1.rc" "true" &
run_phase "$tmpd/f2.log" "$tmpd/f2.rc" "exit 3" &
wait
assert_eq "a failing background phase -> aggregate 1"   "1" "$(aggregate_rcs "$tmpd/f1.rc" "$tmpd/f2.rc")"

# A MISSING rc-file -> treated as failure (1).
assert_eq "missing rc-file -> aggregate 1"              "1" "$(aggregate_rcs "$tmpd/nonexistent.rc")"

# --- run_lane: optional command failing does NOT fail its phase ---
# run_lane (used by ALL lanes) takes alternating cmd/optional args. An optional
# failure is tolerated (rc 0); a required failure fast-fails (non-zero rc).
run_phase "$tmpd/goopt.log" "$tmpd/goopt.rc" "run_lane $(printf '%q ' 'false' 'true' 'true' 'false')"
assert_eq "optional command failing -> lane rc stays 0" "0" "$(cat "$tmpd/goopt.rc")"

run_phase "$tmpd/goreq.log" "$tmpd/goreq.rc" "run_lane $(printf '%q ' 'false' 'false')"
assert_eq "required command failing -> lane rc != 0"    "1" "$([[ "$(cat "$tmpd/goreq.rc")" -ne 0 ]] && echo 1 || echo 0)"

# Two required commands, both pass -> rc 0 (today's GO shape).
run_phase "$tmpd/gook.log" "$tmpd/gook.rc" "run_lane $(printf '%q ' 'true' 'false' 'true' 'false')"
assert_eq "two required commands passing -> lane rc 0"  "0" "$(cat "$tmpd/gook.rc")"

# A lane with multiple required commands runs ALL of them (not just the first):
# the second command's failure must fail the lane even though the first passed.
run_phase "$tmpd/multi.log" "$tmpd/multi.rc" "run_lane $(printf '%q ' 'true' 'false' 'false' 'false')"
assert_eq "lane runs every command (2nd failure fails lane)" "1" "$([[ "$(cat "$tmpd/multi.rc")" -ne 0 ]] && echo 1 || echo 0)"

rm -rf "$tmpd"

# --- PROJECT_ROOT resolves to the PUSHING worktree, not the hook-file's
# location. Simulates core.hooksPath pointing at a DIFFERENT checkout: copy the
# hook OUTSIDE this repo, then source/run the copy under the cwd/GIT_DIR shapes
# git actually uses. The mutation check (reverting the derivation locally) confirms (a) goes
# red against the old script-location derivation.
repo_root="$(pwd)"
outside_dir=$(mktemp -d) || { echo "FAIL: mktemp -d failed"; exit 1; }
mkdir -p "$outside_dir/hooks"
cp scripts/hooks/pre-push "$outside_dir/hooks/pre-push"
real_git_dir="$(git rev-parse --absolute-git-dir)"
expected_root="$(git rev-parse --show-toplevel)"

# (a) hooksPath-simulation, the actual bug shape: cwd = repo root, GIT_DIR
# exported (exactly what git sets for a pre-push hook during a linked-worktree
# push). PROJECT_ROOT must be the pushing root, NOT the copy's grandparent
# (outside_dir) — which is what the old script-location derivation would yield.
# (rc captured via `|| rc=$?`, NOT a bare trailing `$?`: the harness runs under
# `set -e` from the earlier rc-aggregation section, so a `(...)` subshell that
# exits nonzero as a bare simple command would abort the whole script before
# assert_eq ever ran. `||` exempts the preceding command from -e.)
a_rc=0
(
  cd "$repo_root" || exit 1
  export GIT_DIR="$real_git_dir"
  source "$outside_dir/hooks/pre-push"
  [[ "$PROJECT_ROOT" == "$expected_root" ]]
) || a_rc=$?
assert_eq "hooksPath-sim (GIT_DIR set): PROJECT_ROOT == pushing worktree root" "0" "$a_rc"

# (b) no-GIT_DIR manual invocation (discovery path): same copy, cwd = repo
# root, GIT_DIR unset. Same assertion.
b_rc=0
(
  cd "$repo_root" || exit 1
  unset GIT_DIR
  source "$outside_dir/hooks/pre-push"
  [[ "$PROJECT_ROOT" == "$expected_root" ]]
) || b_rc=$?
assert_eq "no-GIT_DIR manual invocation: PROJECT_ROOT == pushing worktree root" "0" "$b_rc"

# (c) missing-config hard fail-closed: cwd = an empty temp dir OUTSIDE any repo
# tree, GIT_DIR exported (so --show-toplevel yields that config-less dir per
# documented git behavior: cwd is treated as toplevel when GIT_WORK_TREE is
# unset). RUN the copy (not source — the guard sits in the executed body) with
# PROMOTION-SHAPED stdin (every ref targets refs/heads/main) and assert exit 1
# plus the "refusing to gate" message. Promotion-shaped stdin (not a feature-
# branch ref) is deliberate: it proves the missing-config guard fires BEFORE
# the promotion-to-main skip check — a feature-branch ref would pass this
# assertion even if a regression moved the guard below the skip, since that ref
# never enters the skip branch either way. This pins the fail-closed guard: a
# wrong resolution must neither reach the fail-open "no config -> skip checks"
# branch NOR silently re-root to the hook checkout.
config_less_dir=$(mktemp -d) || { echo "FAIL: mktemp -d failed"; exit 1; }
c_rc=0
c_out=$(
  cd "$config_less_dir" || exit 1
  export GIT_DIR="$real_git_dir"
  printf 'refs/heads/develop a refs/heads/main b\n' | bash "$outside_dir/hooks/pre-push" 2>&1
) || c_rc=$?
assert_eq "missing-config fail-closed: exit 1"          "1" "$c_rc"
assert_eq "missing-config fail-closed: refusing message" "1" "$(grep -c 'Refusing to gate' <<<"$c_out")"

rm -rf "$outside_dir" "$config_less_dir"

[[ "$fail" -eq 0 ]] && { echo "ALL PASS (pre-push phases)"; exit 0; } || { echo "FAILURES (pre-push phases)"; exit 1; }
