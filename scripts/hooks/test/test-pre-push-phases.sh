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
assert_eq "repo-hygiene -> CONCURRENT"                  "CONCURRENT" "$(classify_command 'bash scripts/hooks/repo-hygiene-check.sh')"
assert_eq "test-unit -> GO"                             "GO"         "$(classify_command 'make test-unit')"
assert_eq "test-integration -> GO"                      "GO"         "$(classify_command 'make test-integration')"
# Unrecognized command defaults to the GO (serial, DB-owning) lane — never concurrent.
assert_eq "unrecognized command -> GO (safe default)"   "GO"         "$(classify_command 'make some-future-db-command')"

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

[[ "$fail" -eq 0 ]] && { echo "ALL PASS (pre-push phases)"; exit 0; } || { echo "FAILURES (pre-push phases)"; exit 1; }
