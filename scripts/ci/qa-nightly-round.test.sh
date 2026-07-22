#!/usr/bin/env bash
# Tests for qa-nightly-round.sh — the pure, stateless nightly QA-round orchestrator.
#
# The orchestrator sequences four collaborators: the cadence gate, the deployed-sha
# assert, the reseed command, and the `make` pipeline (tours -> qa-report -> qa-export).
# Each is STUBBED here so every branch of the orchestrator's own logic — skip, clean
# advance, the incomplete-round grades, and the start-time aborts — is exercised
# deterministically with no network, browser, git repo, or Langfuse:
#   - the two sibling scripts are COPIED as controllable stubs into the fixture's
#     scripts/ci/ (the orchestrator resolves them relative to its own location, so the
#     fixture copy shadows the real ones);
#   - `make` is a stub bin pointed at by QA_MAKE that dispatches on the target, writes
#     the manifest / trace, prints a controllable qa-export RESULT line, and records
#     each call (+ its inherited git-location env) to a call log;
#   - QA_RESEED_CMD / QA_DEPLOYED_SHA_CMD are injected shell commands.
# Every assertion pins the EXACT advance/round decision (not just the exit), so a logic
# revert in the predicate or the branch flips the grade and reddens the test.
#
# Portable: no network, no BSD-only flags — runs on Ubuntu CI and the macOS judge host.

set -uo pipefail

# Sanitize hook-inherited git env BEFORE anything (this suite may run under the pre-push
# hook, which exports GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE). Also lets the git-env
# isolation test set them explicitly per-run without a leaked baseline.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_COMMON_DIR GIT_OBJECT_DIRECTORY

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
REAL_ORCH="$REPO_ROOT/scripts/ci/qa-nightly-round.sh"
HEAD_SHA="abc1230000000000000000000000000000000def"  # a plausible 40-hex deployed sha
BASE_SHA="def4560000000000000000000000000000000abc"

PASS=0
FAIL=0
fail() { echo "  FAIL: $1" >&2; FAIL=$((FAIL + 1)); }
ok()   { PASS=$((PASS + 1)); }

# require_tmpdir <varname>: mktemp -d into the named var, aborting the WHOLE run if it
# fails or yields no directory (an empty dir would make the fixture write into the real
# tree). The repo's git-fixture hazard, guarded hard — same contract as the sibling.
require_tmpdir() {
    local __d
    __d="$(mktemp -d)" || { echo "FATAL: mktemp -d failed" >&2; exit 2; }
    { [ -n "$__d" ] && [ -d "$__d" ]; } || { echo "FATAL: mktemp -d produced no directory" >&2; exit 2; }
    printf -v "$1" '%s' "$__d"
}

# ---------------------------------------------------------------------------
# Fixture: the orchestrator copy + controllable stubs.
# ---------------------------------------------------------------------------
make_fixture() {
    require_tmpdir FIXTURE
    GHENV="$FIXTURE/ghenv"
    CALL_LOG="$FIXTURE/calls.log"
    mkdir -p "$FIXTURE/scripts/ci" "$FIXTURE/bin"
    cp "$REAL_ORCH" "$FIXTURE/scripts/ci/qa-nightly-round.sh"

    # Stub cadence gate: emits the four-flag tuple (stdout + GITHUB_ENV, like the real
    # one) with a controllable run_round; STUB_GATE_RC drives the lost-decision path.
    cat > "$FIXTURE/scripts/ci/qa-round-cadence-gate.sh" <<'EOF'
#!/usr/bin/env bash
e() { printf '%s=%s\n' "$1" "$2"; [ -n "${GITHUB_ENV:-}" ] && printf '%s=%s\n' "$1" "$2" >> "$GITHUB_ENV"; }
e run_round "${STUB_RUN_ROUND:-true}"
e judge_relevant_change true
e base_known true
e changed_groups backend
exit "${STUB_GATE_RC:-0}"
EOF

    # Stub deployed-sha assert: 1 arg = pre-tours pin (STUB_ASSERT_PRE_RC), 2 args =
    # post-tours provenance (STUB_ASSERT_POST_RC). Keyed on arg count, like the real call sites.
    cat > "$FIXTURE/scripts/ci/qa-round-deployed-sha-assert.sh" <<'EOF'
#!/usr/bin/env bash
if [ "$#" -eq 1 ]; then exit "${STUB_ASSERT_PRE_RC:-0}"; else exit "${STUB_ASSERT_POST_RC:-0}"; fi
EOF

    # Stub `make`: dispatch on target. Records each call + its inherited git-location env
    # to the call log (for the isolation contract test). `tours` writes the manifest under
    # the run id the orchestrator passed; `qa-report` writes the trace; `qa-export` prints
    # a controllable RESULT line.
    cat > "$FIXTURE/bin/make" <<'EOF'
#!/usr/bin/env bash
log="${STUB_CALL_LOG:?}"
# Record the FULL arg vector + the provenance/isolation env each call inherited, so a test
# can assert that dropping JUDGE=1 / RUNDIR / TRACE / QA_RUN_ID / QA_GIT_SHA reddens.
printf 'make %s | args:%s | gitenv:GIT_DIR=%s,GIT_WORK_TREE=%s,GIT_INDEX_FILE=%s,GIT_COMMON_DIR=%s,GIT_OBJECT_DIRECTORY=%s | skip_reset=%s | judge_trace=%s | run_id=%s | git_sha=%s | reseed_cmd=%s | sha_cmd=%s | gen_cmd=%s | lang_secret=%s | tours_key=%s\n' \
    "$1" "$*" "${GIT_DIR:-}" "${GIT_WORK_TREE:-}" "${GIT_INDEX_FILE:-}" "${GIT_COMMON_DIR:-}" "${GIT_OBJECT_DIRECTORY:-}" "${TOURS_SKIP_RESET:-}" "${QA_JUDGE_TRACE:-}" "${QA_RUN_ID:-}" "${QA_GIT_SHA:-}" "${QA_RESEED_CMD:-}" "${QA_DEPLOYED_SHA_CMD:-}" "${QA_DEPLOYED_GEN_CMD:-}" "${LANGFUSE_SECRET_KEY:-}" "${TOURS_API_KEY:-}" >> "$log"
case "$1" in
  tours)
    mkdir -p "frontend/tests/tours/.runs/${TOURS_RUN_ID:?}"
    printf '{"gitSha":"%s"}\n' "${STUB_MANIFEST_SHA:-unknown}" > "frontend/tests/tours/.runs/${TOURS_RUN_ID}/manifest.json"
    exit "${STUB_TOURS_RC:-0}" ;;
  qa-report)
    [ -n "${QA_JUDGE_TRACE:-}" ] && printf 'span\n' >> "$QA_JUDGE_TRACE"
    exit "${STUB_REPORT_RC:-0}" ;;
  qa-export)
    printf '%s\n' "${STUB_EXPORT_OUT:-qa-export: 5 trace(s), 12 screenshot(s); enqueued 3/3}"
    exit "${STUB_EXPORT_RC:-0}" ;;
  *) echo "stub make: unexpected target $1" >&2; exit 99 ;;
esac
EOF
    chmod +x "$FIXTURE/bin/make"
}

cleanup_fixture() { [ -n "${FIXTURE:-}" ] && rm -rf "$FIXTURE"; }

# run_orch <ENV=val...>: run the orchestrator copy with all seams injected; sets RC.
# Extra args override/add env (e.g. STUB_RUN_ROUND=false, QA_RESEED_CMD=false).
OUT=""; ERR=""; RC=0
run_orch() {
    : > "$GHENV"; : > "$CALL_LOG"
    OUT="$FIXTURE/out.txt"; ERR="$FIXTURE/err.txt"
    ( cd "$FIXTURE" && env \
        QA_MAKE="$FIXTURE/bin/make" \
        GITHUB_ENV="$GHENV" \
        STUB_CALL_LOG="$CALL_LOG" \
        QA_BASE_SHA="$BASE_SHA" QA_HEAD_SHA="$HEAD_SHA" QA_DAYS_SINCE=0 \
        QA_JUDGE_TRACE="$FIXTURE/trace.jsonl" \
        QA_DEPLOYED_SHA_CMD="printf %s $HEAD_SHA" \
        STUB_MANIFEST_SHA="$HEAD_SHA" \
        "$@" \
        bash "$FIXTURE/scripts/ci/qa-nightly-round.sh" ) >"$OUT" 2>"$ERR"
    RC=$?
}

# ghv <key>: the LAST value emitted for <key> in $GITHUB_ENV (the orchestrator's own
# decision keys win over the gate's; last-write is the emitted decision).
ghv() { grep -E "^$1=" "$GHENV" | tail -1 | cut -d= -f2-; }

assert_rc0()     { if [ "$RC" -eq 0 ]; then ok; else fail "$1: want exit 0, got $RC (err: $(head -2 "$ERR"))"; fi; }
assert_rc_nz()   { if [ "$RC" -ne 0 ]; then ok; else fail "$1: want non-zero exit, got 0"; fi; }
assert_kv()      { local got; got="$(ghv "$1")"; if [ "$got" = "$2" ]; then ok; else fail "$3: want $1=$2, got '$got'"; fi; }
assert_ran()     { if grep -q "^make $1 " "$CALL_LOG"; then ok; else fail "$2: expected 'make $1' to run"; fi; }
assert_not_ran() { if grep -q "^make $1 " "$CALL_LOG"; then fail "$2: 'make $1' must NOT run"; else ok; fi; }
# assert_call <ERE> <label>: a recorded call line matches the pattern (argv/env forwarding).
assert_call()    { if grep -qE "$1" "$CALL_LOG"; then ok; else fail "$2: no call matches /$1/ in log"; fi; }
# assert_reason <substr> <label>: the emitted reason contains <substr>.
assert_reason()  { case "$(ghv reason)" in *"$1"*) ok;; *) fail "$2: reason should contain '$1', got '$(ghv reason)'";; esac; }

# ===========================================================================
# Branch coverage — every assertion pins the exact decision (mutation-reddens).
# ===========================================================================

test_skip() {
    echo "test: run_round=false -> round=skipped, advance=false, nothing else runs"
    make_fixture
    run_orch STUB_RUN_ROUND=false
    assert_rc0 skip
    assert_kv round skipped skip
    assert_kv advance false skip
    assert_not_ran tours skip
    assert_not_ran qa-report skip
    assert_not_ran qa-export skip
    cleanup_fixture
}

test_clean() {
    echo "test: run_round=true + all green -> round=clean, advance=true, target=HEAD"
    make_fixture
    run_orch
    assert_rc0 clean
    assert_kv round clean clean
    assert_kv advance true clean
    assert_kv target "$HEAD_SHA" clean
    assert_ran tours clean
    assert_ran qa-report clean
    assert_ran qa-export clean
    # Provenance/args are forwarded: qa-report gets JUDGE=1 + a RUNDIR + the trace path;
    # qa-export gets QA_RUN_ID (timestamp form) + QA_GIT_SHA=HEAD. Dropping any reddens.
    assert_call '^make qa-report \| args:qa-report JUDGE=1 RUNDIR=tests/tours/\.runs/[0-9]{8}T[0-9]{6}Z' clean-report-args
    assert_call "^make qa-report .*judge_trace=$FIXTURE/trace\.jsonl" clean-report-trace
    assert_call "^make qa-export \| args:qa-export TRACE=$FIXTURE/trace\.jsonl" clean-export-trace-arg
    assert_call "^make qa-export .*run_id=[0-9]{8}T[0-9]{6}Z .*git_sha=$HEAD_SHA" clean-export-prov
    # Observability counts are surfaced for the wrapper.
    assert_kv traces 5 clean
    assert_kv enqueue_ok 3 clean
    cleanup_fixture
}

test_export_failed() {
    echo "test: a trace ship failure -> round=incomplete, advance=false"
    make_fixture
    run_orch STUB_EXPORT_RC=1 \
        STUB_EXPORT_OUT="qa-export: 5 trace(s), 12 screenshot(s), 2 FAILED; enqueued 1/3, 1 already queued, 1 enqueue-failed"
    assert_rc0 export-failed
    assert_kv round incomplete export-failed
    assert_kv advance false export-failed
    assert_kv ship_failed 2 export-failed
    cleanup_fixture
}

test_pre_assert_fail() {
    echo "test: pre-tours pin fail -> round=aborted, advance=false, exit!=0, tours never run"
    make_fixture
    run_orch STUB_ASSERT_PRE_RC=1
    assert_rc_nz pre-assert
    assert_kv round aborted pre-assert
    assert_kv advance false pre-assert
    assert_not_ran tours pre-assert
    assert_not_ran qa-export pre-assert
    cleanup_fixture
}

test_post_assert_fail() {
    echo "test: mid-round redeploy (post-tours assert fail) -> advance=false, but the round RAN"
    make_fixture
    run_orch STUB_ASSERT_POST_RC=1
    assert_rc0 post-assert
    assert_kv round incomplete post-assert
    assert_kv advance false post-assert
    assert_ran qa-export post-assert   # the round still ran + shipped
    cleanup_fixture
}

test_reseed_fail() {
    echo "test: reseed command fails -> round=aborted, advance=false, exit!=0, tours never run"
    make_fixture
    run_orch QA_RESEED_CMD=false
    assert_rc_nz reseed-fail
    assert_kv round aborted reseed-fail
    assert_kv advance false reseed-fail
    assert_not_ran tours reseed-fail
    cleanup_fixture
}

test_reseed_fail_no_secret_leak() {
    echo "test: a failing QA_RESEED_CMD's OUTPUT (secret-bearing) never leaks into the round output"
    make_fixture
    # QA_RESEED_CMD can PRINT credentials / private hostnames (or a shell diagnostic naming
    # them). The reseed cmd here emits a secret token to stdout AND stderr, then fails; the
    # orchestrator must DISCARD its output, so the token appears nowhere and the abort reason
    # is a fixed message. (A cmd that only takes the token as an ARG — `false TOKEN` — never
    # prints it, so it would pass even without the suppression; that is NOT a real test.)
    run_orch QA_RESEED_CMD='echo SECRETTOKEN_zzz; echo SECRETTOKEN_zzz >&2; exit 1'
    assert_rc_nz reseed-noleak
    assert_kv round aborted reseed-noleak
    if grep -rq 'SECRETTOKEN_zzz' "$GHENV" "$OUT" "$ERR"; then fail "reseed-noleak: reseed command output leaked into round output"; else ok; fi
    if [ "$(grep -c '^round=' "$GHENV")" -eq 1 ]; then ok; else fail "reseed-noleak: emit protocol corrupted (duplicate keys)"; fi
    cleanup_fixture
}

test_reseed_ok_skips_tours_reset() {
    echo "test: a supplied reseed runs first + tours get TOURS_SKIP_RESET=1 (no double reseed)"
    make_fixture
    run_orch QA_RESEED_CMD=true
    assert_rc0 reseed-ok
    assert_kv round clean reseed-ok
    if grep -qE '^make tours .*skip_reset=1' "$CALL_LOG"; then ok; else fail "reseed-ok: tours must get TOURS_SKIP_RESET=1"; fi
    cleanup_fixture
}

test_no_reseed_lets_tours_reset() {
    echo "test: no reseed cmd -> tours run WITHOUT skip_reset (tours resets itself)"
    make_fixture
    run_orch   # QA_RESEED_CMD unset
    if grep -qE '^make tours .*skip_reset=1' "$CALL_LOG"; then fail "no-reseed: tours must NOT get skip_reset"; else ok; fi
    cleanup_fixture
}

test_trap_miss_still_exports() {
    echo "test: trap miss (qa-report exit!=0) -> incomplete, advance=false, export STILL ships (INV-B)"
    make_fixture
    run_orch STUB_REPORT_RC=2
    assert_rc0 trap-miss
    assert_kv round incomplete trap-miss
    assert_kv advance false trap-miss
    assert_ran qa-export trap-miss   # the ; step ships the diagnostic trace regardless
    cleanup_fixture
}

test_tours_fail() {
    echo "test: tours fail/incomplete -> advance=false, but report+export still run"
    make_fixture
    run_orch STUB_TOURS_RC=1
    assert_rc0 tours-fail
    assert_kv round incomplete tours-fail
    assert_kv advance false tours-fail
    assert_ran qa-report tours-fail
    assert_ran qa-export tours-fail
    cleanup_fixture
}

test_no_enqueue_landed() {
    echo "test: zero eligible enqueue landed (0/0, none already queued) -> advance=false"
    make_fixture
    run_orch STUB_EXPORT_OUT="qa-export: 5 trace(s), 12 screenshot(s); enqueued 0/0"
    assert_rc0 no-enqueue
    assert_kv round incomplete no-enqueue
    assert_kv advance false no-enqueue
    cleanup_fixture
}

test_enqueue_via_already_queued() {
    echo "test: mandatory item already-queued (0 new, 2 already) -> counts as landed -> clean"
    make_fixture
    run_orch STUB_EXPORT_OUT="qa-export: 5 trace(s), 12 screenshot(s); enqueued 0/0, 2 already queued"
    assert_rc0 already-queued
    assert_kv round clean already-queued
    assert_kv advance true already-queued
    cleanup_fixture
}

test_no_traces_missing_creds() {
    echo "test: export ships nothing (missing creds / no spans) -> traces=0 -> advance=false"
    make_fixture
    run_orch STUB_EXPORT_OUT="qa-export: LANGFUSE_HOST / LANGFUSE_PUBLIC_KEY / LANGFUSE_SECRET_KEY not set — nothing shipped."
    assert_rc0 no-traces
    assert_kv round incomplete no-traces
    assert_kv advance false no-traces
    assert_kv traces 0 no-traces
    cleanup_fixture
}

test_gate_rc_nonzero() {
    echo "test: cadence gate exits non-zero (lost decision) -> round=aborted, exit!=0"
    make_fixture
    run_orch STUB_GATE_RC=3
    assert_rc_nz gate-rc
    assert_kv round aborted gate-rc
    assert_not_ran tours gate-rc
    cleanup_fixture
}

test_missing_judge_trace() {
    echo "test: QA_JUDGE_TRACE unset on a run -> round=aborted, exit!=0"
    make_fixture
    run_orch QA_JUDGE_TRACE=""
    assert_rc_nz missing-trace
    assert_kv round aborted missing-trace
    assert_not_ran tours missing-trace
    cleanup_fixture
}

test_missing_deployed_sha_cmd() {
    echo "test: QA_DEPLOYED_SHA_CMD unset -> cannot verify provenance -> advance=false"
    make_fixture
    run_orch QA_DEPLOYED_SHA_CMD=""
    assert_rc0 no-deployed-cmd
    assert_kv round incomplete no-deployed-cmd
    assert_kv advance false no-deployed-cmd
    cleanup_fixture
}

# ===========================================================================
# ISOLATED predicate-term cases — each fails EXACTLY ONE term, so dropping that
# term from the predicate stops reddening it (mutation-discriminating). The
# multi-term cases above still exercise realistic combinations.
# ===========================================================================
test_isolated_export_exit() {
    echo "test: ISOLATED export exit!=0 (clean summary otherwise) -> advance=false"
    make_fixture
    run_orch STUB_EXPORT_RC=1 \
        STUB_EXPORT_OUT="qa-export: 5 trace(s), 12 screenshot(s); enqueued 3/3"
    assert_rc0 iso-export-exit
    assert_kv advance false iso-export-exit
    assert_kv ship_failed 0 iso-export-exit   # only the exit term is bad
    cleanup_fixture
}

test_isolated_failed_only() {
    echo "test: ISOLATED shipped FAILED>0 (export exit 0) -> advance=false"
    make_fixture
    run_orch STUB_EXPORT_RC=0 \
        STUB_EXPORT_OUT="qa-export: 5 trace(s), 12 screenshot(s), 2 FAILED; enqueued 3/3"
    assert_rc0 iso-failed
    assert_kv advance false iso-failed
    assert_kv ship_failed 2 iso-failed
    assert_kv export_exit 0 iso-failed
    cleanup_fixture
}

test_isolated_enqueue_failed_only() {
    echo "test: ISOLATED enqueue-failed>0 (export exits 0 — the easiest term to lose) -> advance=false"
    make_fixture
    run_orch STUB_EXPORT_RC=0 \
        STUB_EXPORT_OUT="qa-export: 5 trace(s), 12 screenshot(s); enqueued 2/3, 1 enqueue-failed"
    assert_rc0 iso-enq-failed
    assert_kv advance false iso-enq-failed
    assert_kv enqueue_failed 1 iso-enq-failed
    assert_kv export_exit 0 iso-enq-failed   # proves this term isn't covered by the exit term
    cleanup_fixture
}

test_isolated_traces_zero() {
    echo "test: ISOLATED traces==0 but an enqueue landed -> advance=false"
    make_fixture
    run_orch STUB_EXPORT_RC=0 \
        STUB_EXPORT_OUT="qa-export: 0 trace(s), 0 screenshot(s); enqueued 3/3"
    assert_rc0 iso-traces0
    assert_kv advance false iso-traces0
    assert_kv traces 0 iso-traces0
    assert_kv enqueue_ok 3 iso-traces0   # the enqueue term is satisfied; only traces fails
    cleanup_fixture
}

test_deployed_sha_moved() {
    echo "test: mid-round redeploy — re-read sha is a DIFFERENT valid sha, assert PASSES -> no advance"
    make_fixture
    # The reader returns a valid 40-hex sha that is NOT the round's target, and the post
    # assert stub SUCCEEDS. Only the CURRENT==QA_HEAD_SHA pin can catch this: a deployment
    # that moved mid-round must not advance the watermark to a sha the round didn't run on.
    local other="feedfeed0000000000000000000000000000feed"
    run_orch QA_DEPLOYED_SHA_CMD="printf %s $other" STUB_ASSERT_POST_RC=0
    assert_rc0 sha-moved
    assert_kv round incomplete sha-moved
    assert_kv advance false sha-moved
    cleanup_fixture
}

test_deployed_cmd_fails() {
    echo "test: deployed-sha reader prints the sha then EXITS NONZERO -> advance=false"
    make_fixture
    # Stdout is the expected sha, but exit is nonzero: a discarded exit status would
    # wrongly pass the post-assert. The stub assert would pass (STUB_ASSERT_POST_RC=0),
    # so ONLY the reader's exit status can catch this.
    run_orch QA_DEPLOYED_SHA_CMD="printf %s $HEAD_SHA; exit 7"
    assert_rc0 deployed-cmd-fail
    assert_kv round incomplete deployed-cmd-fail
    assert_kv advance false deployed-cmd-fail
    cleanup_fixture
}

test_deployed_cmd_multiline() {
    echo "test: deployed-sha reader returns multiline/garbage -> advance=false, emit stays single-line"
    make_fixture
    run_orch QA_DEPLOYED_SHA_CMD="printf '%s\n../evil\n' $HEAD_SHA"
    assert_rc0 deployed-multiline
    assert_kv advance false deployed-multiline
    # The raw multiline output must NOT corrupt the emit protocol: every emitted line is a
    # single key=value, so `advance=` appears exactly once and there is no stray line.
    if [ "$(grep -c '^advance=' "$GHENV")" -eq 1 ]; then ok; else fail "deployed-multiline: advance= emitted more than once (protocol corrupted)"; fi
    if grep -q '../evil' "$GHENV"; then fail "deployed-multiline: raw command output leaked into GITHUB_ENV"; else ok; fi
    cleanup_fixture
}

test_export_summary_fragment_ignored() {
    echo "test: a pre-summary/HTTP-body 'enqueued 1/1' fragment is IGNORED; the FINAL summary governs"
    make_fixture
    # A queue-resolve error embeds an external body containing 'enqueued 1/1', plus a
    # pre-summary log line, BEFORE the real final summary (enqueued 0/0). A fragment scan
    # would pick 1/1 and falsely pass the mandatory-enqueue term. The anchored single-line
    # parse must use the final summary (0/0) -> mandatory enqueue landed 0 -> advance=false.
    run_orch STUB_EXPORT_OUT="  enqueue: queue resolve failed: {\"error\":\"body says enqueued 1/1 haha\"}
  enqueued 9/9 (pre-summary progress log)
qa-export: 5 trace(s), 12 screenshot(s); enqueued 0/0"
    assert_rc0 fragment
    assert_kv advance false fragment
    assert_kv enqueue_ok 0 fragment          # parsed from the FINAL summary, not the fragment
    assert_kv enqueue_attempted 0 fragment
    cleanup_fixture
}

test_export_duplicate_summary() {
    echo "test: two lines matching the summary shape -> ambiguous -> advance=false"
    make_fixture
    run_orch STUB_EXPORT_OUT="qa-export: 5 trace(s), 12 screenshot(s); enqueued 3/3
qa-export: 6 trace(s), 12 screenshot(s); enqueued 3/3"
    assert_rc0 dup-summary
    assert_kv round incomplete dup-summary
    assert_kv advance false dup-summary
    assert_kv export_summary_lines 2 dup-summary
    cleanup_fixture
}

# The summary line is parsed with a FULLY ANCHORED regex requiring exactly one match:
# a field added to run.ts's line without the matching regex change makes EVERY count
# parse as zero, so the round silently stops advancing while every unit test stays
# green. These two cases pin both shapes the parser must accept.
test_export_summary_with_observations() {
    echo "test: the summary line carrying an observation(s) field parses (counts + advance intact)"
    make_fixture
    run_orch STUB_EXPORT_OUT="qa-export: 5 trace(s), 12 screenshot(s), 4 observation(s); enqueued 3/3"
    assert_rc0 obs-summary
    assert_kv round clean obs-summary
    assert_kv advance true obs-summary
    assert_kv export_summary_lines 1 obs-summary
    assert_kv traces 5 obs-summary
    assert_kv observations 4 obs-summary
    assert_kv enqueue_ok 3 obs-summary
    cleanup_fixture
}

test_export_summary_without_observations() {
    echo "test: a summary line WITHOUT the observation(s) field still parses (field is optional)"
    make_fixture
    run_orch STUB_EXPORT_OUT="qa-export: 5 trace(s), 12 screenshot(s), 2 FAILED; enqueued 3/3" \
        STUB_EXPORT_RC=1
    assert_rc0 no-obs-summary
    assert_kv export_summary_lines 1 no-obs-summary
    assert_kv traces 5 no-obs-summary
    assert_kv observations 0 no-obs-summary
    assert_kv ship_failed 2 no-obs-summary
    cleanup_fixture
}

test_runid_collision_aborts() {
    echo "test: run-id collision (run dir already exists) -> round=aborted, exit!=0, tours never run"
    make_fixture
    # Pre-create the exact run dir the orchestrator will target (via the QA_RUN_ID_OVERRIDE
    # test seam), so the fresh-per-round collision guard must fail-closed.
    mkdir -p "$FIXTURE/frontend/tests/tours/.runs/20260101T000000Z"
    run_orch QA_RUN_ID_OVERRIDE=20260101T000000Z
    assert_rc_nz collision
    assert_kv round aborted collision
    assert_kv advance false collision
    assert_reason collision collision   # a pre-existing dir is labeled a collision, not an fs error
    assert_not_ran tours collision
    cleanup_fixture
}

test_rundir_create_error() {
    echo "test: run dir cannot be created (fs/permission, not a collision) -> aborted with a distinct reason"
    if [ "$(id -u)" -eq 0 ]; then echo "  SKIP: running as root, chmod bypassed"; return; fi
    make_fixture
    # Make the .runs parent read-only so the atomic leaf mkdir fails WITHOUT the dir existing
    # -> the "could not create" branch, distinct from a collision.
    mkdir -p "$FIXTURE/frontend/tests/tours/.runs"
    chmod 555 "$FIXTURE/frontend/tests/tours/.runs"
    run_orch QA_RUN_ID_OVERRIDE=20260101T000000Z
    chmod 755 "$FIXTURE/frontend/tests/tours/.runs"
    assert_rc_nz rundir-fs
    assert_kv round aborted rundir-fs
    assert_reason "could not create" rundir-fs
    assert_not_ran tours rundir-fs
    cleanup_fixture
}

test_deploy_gen_unset_records_false() {
    echo "test: no deploy-generation seam -> clean via sha-only guard; deploy_gen_checked=false (residual documented)"
    make_fixture
    run_orch   # QA_DEPLOYED_GEN_CMD unset
    assert_rc0 gen-unset
    assert_kv round clean gen-unset
    assert_kv advance true gen-unset
    assert_kv deploy_gen_checked false gen-unset
    cleanup_fixture
}

test_deploy_gen_matches_clean() {
    echo "test: deploy generation provided + unchanged pre/post -> clean, deploy_gen_checked=true"
    make_fixture
    run_orch QA_DEPLOYED_GEN_CMD="echo 42"
    assert_rc0 gen-match
    assert_kv round clean gen-match
    assert_kv advance true gen-match
    assert_kv deploy_gen_checked true gen-match
    cleanup_fixture
}

test_deploy_gen_bump_no_advance() {
    echo "test: deploy generation BUMPS mid-round (A->B->A rollback) -> advance=false though the sha matches"
    make_fixture
    # A monotonic counter file: pre-tours read=1, post-tours read=2 -> generation changed, so
    # a redeploy happened even though the deployed sha (default stub) still equals the target
    # -> the case the sha-equality pin alone cannot catch.
    run_orch QA_DEPLOYED_GEN_CMD="n=\$(cat $FIXTURE/gen 2>/dev/null || echo 0); n=\$((n+1)); echo \$n > $FIXTURE/gen; echo \$n"
    assert_rc0 gen-bump
    assert_kv round incomplete gen-bump
    assert_kv advance false gen-bump
    cleanup_fixture
}

test_deploy_gen_reader_fails() {
    echo "test: deploy-generation reader fails -> cannot confirm -> advance=false"
    make_fixture
    run_orch QA_DEPLOYED_GEN_CMD="exit 5"
    assert_rc0 gen-fail
    assert_kv advance false gen-fail
    cleanup_fixture
}

test_lost_github_env_write() {
    echo "test: an unwritable GITHUB_ENV makes emit fail VISIBLY (non-zero), not a silent exit-0"
    make_fixture
    OUT="$FIXTURE/out.txt"; ERR="$FIXTURE/err.txt"
    # Point GITHUB_ENV at a path whose parent doesn't exist; run_round=false reaches the
    # first orchestrator emit quickly. A lost decision write must exit non-zero.
    ( cd "$FIXTURE" && env \
        QA_MAKE="$FIXTURE/bin/make" STUB_CALL_LOG="$CALL_LOG" \
        GITHUB_ENV="$FIXTURE/no-such-dir/ghenv" \
        QA_BASE_SHA="$BASE_SHA" QA_HEAD_SHA="$HEAD_SHA" QA_DAYS_SINCE=0 \
        QA_JUDGE_TRACE="$FIXTURE/trace.jsonl" \
        STUB_RUN_ROUND=false \
        bash "$FIXTURE/scripts/ci/qa-nightly-round.sh" ) >"$OUT" 2>"$ERR"
    local rc=$?
    if [ "$rc" -ne 0 ]; then ok; else fail "lost-ghenv: want non-zero exit on GITHUB_ENV write failure"; fi
    cleanup_fixture
}

# ===========================================================================
# Contract tests: fixture fail-closed guard + git-env isolation.
# ===========================================================================
test_require_tmpdir_fails_closed() {
    echo "test: require_tmpdir aborts (non-zero) when mktemp fails (fixture guard is fail-closed)"
    if ( mktemp() { return 1; }; require_tmpdir X ) >/dev/null 2>&1; then
        fail "require_tmpdir must abort on mktemp failure"
    else
        ok
    fi
}

test_git_env_isolation() {
    echo "test: the orchestrator unsets ALL git-location vars before invoking children"
    make_fixture
    # Poison all five in the outer env; the stub make records what it inherited. The
    # orchestrator must have cleared them all, so make sees every one EMPTY. Dropping any
    # var from the orchestrator's unset line reddens this (that var reaches the child).
    run_orch GIT_DIR=/x GIT_WORK_TREE=/x GIT_INDEX_FILE=/x GIT_COMMON_DIR=/x GIT_OBJECT_DIRECTORY=/x
    assert_rc0 git-iso
    if grep -qE 'gitenv:GIT_DIR=,GIT_WORK_TREE=,GIT_INDEX_FILE=,GIT_COMMON_DIR=,GIT_OBJECT_DIRECTORY= ' "$CALL_LOG"; then
        ok
    else
        fail "git-iso: a git-location var leaked to a child ($(grep -m1 gitenv "$CALL_LOG"))"
    fi
    cleanup_fixture
}

# ===========================================================================
# Predicate discrimination: enqueue accounting, exporter warnings, run-id safety.
# ===========================================================================
test_enqueue_inconsistent() {
    echo "test: 'enqueued 2/3' with NO enqueue-failed suffix -> inconsistent summary -> no advance"
    make_fixture
    # enqueued(2) + failed(0, absent suffix) != attempted(3): a truncated/mixed summary. The
    # counts otherwise look clean (failed=0, landed>=1), so only the partition check catches it.
    run_orch STUB_EXPORT_OUT="qa-export: 5 trace(s), 12 screenshot(s); enqueued 2/3"
    assert_rc0 enq-inconsistent
    assert_kv advance false enq-inconsistent
    assert_reason "inconsistent enqueue summary" enq-inconsistent
    cleanup_fixture
}

test_export_warning_no_advance() {
    echo "test: an exporter 'qa-export: WARNING' (degraded provenance) before a clean summary -> no advance"
    make_fixture
    # A reused-trace-path / degraded-sidecar warning means the round's evidence is stale/mixed;
    # a clean summary can still follow, so the counts look fine — only the warning scan catches it.
    run_orch STUB_EXPORT_OUT="$(printf 'qa-export: WARNING — trace path reused across rounds; stale spans are being relabeled.\nqa-export: 5 trace(s), 12 screenshot(s); enqueued 3/3')"
    assert_rc0 export-warn
    assert_kv advance false export-warn
    assert_reason "degradation/uncertainty warning" export-warn
    cleanup_fixture
}

test_runid_traversal_rejected() {
    echo "test: a traversal QA_RUN_ID_OVERRIDE -> aborted BEFORE the mkdir reservation; no dir escapes .runs"
    make_fixture
    run_orch QA_RUN_ID_OVERRIDE="../evilrun"
    assert_rc_nz runid-traversal
    assert_kv round aborted runid-traversal
    assert_reason "invalid run id" runid-traversal
    # The reservation must not have created a directory outside .runs.
    if [ -e "$FIXTURE/frontend/tests/tours/evilrun" ] || [ -e "$FIXTURE/frontend/tests/evilrun" ]; then
        fail "runid-traversal: a directory was created outside .runs"
    else
        ok
    fi
    cleanup_fixture
}

test_deploy_gen_stdout_nonzero() {
    echo "test: deploy-gen PRE-read prints a value but EXITS NON-ZERO -> untrusted -> no advance (ISOLATES the pre-read rc guard)"
    make_fixture
    # Counter cmd: the PRE read (n=1) prints "7" but exits 3; the POST read (n=2) prints the
    # SAME "7" and exits 0. So the post comparison (7 == 7) would PASS — only the pre-read rc
    # guard catches the untrusted pre value. Removing that guard reddens this (post masks the
    # earlier same-command tests; this one does not).
    run_orch QA_DEPLOYED_GEN_CMD="n=\$(cat $FIXTURE/gsn 2>/dev/null || echo 0); n=\$((n+1)); echo \$n > $FIXTURE/gsn; echo 7; [ \$n -eq 1 ] && exit 3; exit 0"
    assert_rc0 gen-stdout-nz
    assert_kv advance false gen-stdout-nz
    cleanup_fixture
}

test_deploy_gen_empty_no_advance() {
    echo "test: deploy-gen reader EXITS ZERO but empty (both reads) -> no generation to trust -> no advance (post-read empty guard)"
    make_fixture
    run_orch QA_DEPLOYED_GEN_CMD="true"   # exit 0, prints nothing, both reads
    assert_rc0 gen-empty
    assert_kv advance false gen-empty
    cleanup_fixture
}

test_deploy_gen_post_read_fails() {
    echo "test: deploy-gen reader OK pre-tours but FAILS post-tours -> cannot confirm no redeploy -> no advance"
    make_fixture
    # Succeeds on the first (pre) read, fails on the second (post) read via a counter file.
    run_orch QA_DEPLOYED_GEN_CMD="n=\$(cat $FIXTURE/gp 2>/dev/null || echo 0); n=\$((n+1)); echo \$n > $FIXTURE/gp; [ \$n -ge 2 ] && exit 9; echo ok"
    assert_rc0 gen-post-fail
    assert_kv advance false gen-post-fail
    cleanup_fixture
}

test_trace_freshness_truncated() {
    echo "test: a REUSED (stale) QA_JUDGE_TRACE is truncated at round start -> the round's export is fresh"
    make_fixture
    # Pre-populate the trace path with a stale span; the stub qa-report APPENDS (like the real
    # judge). If the orchestrator did not truncate, the stale span would survive into the export.
    printf 'STALE_SPAN_zzz\n' > "$FIXTURE/trace.jsonl"
    run_orch
    assert_rc0 trace-fresh
    if grep -q 'STALE_SPAN_zzz' "$FIXTURE/trace.jsonl"; then fail "trace-fresh: stale span survived — trace not truncated"; else ok; fi
    cleanup_fixture
}

test_tours_skip_reset_cleared() {
    echo "test: an INHERITED TOURS_SKIP_RESET=1 with NO reseed is cleared -> tours still reset (not advance an unreseeded round)"
    make_fixture
    # Poison the outer env with TOURS_SKIP_RESET=1 and provide NO QA_RESEED_CMD. The orchestrator
    # must clear it so `make tours` sees it EMPTY (reset happens). The stub records skip_reset=<val>.
    run_orch TOURS_SKIP_RESET=1
    assert_rc0 skip-reset-clear
    if grep -qE '^make tours .* skip_reset= ' "$CALL_LOG"; then ok; else fail "skip-reset-clear: TOURS_SKIP_RESET leaked to tours ($(grep -m1 '^make tours' "$CALL_LOG"))"; fi
    cleanup_fixture
}

test_media_incomplete_no_advance() {
    echo "test: traces shipped but 0 screenshots (media upload failed, export still exit 0) -> no advance"
    make_fixture
    run_orch STUB_EXPORT_OUT="qa-export: 3 trace(s), 0 screenshot(s); enqueued 3/3"
    assert_rc0 media-incomplete
    assert_kv advance false media-incomplete
    assert_reason "0 screenshots" media-incomplete
    cleanup_fixture
}

test_reader_stderr_no_leak() {
    echo "test: an injected reader's STDERR (secret-bearing) is discarded, not leaked into the round output"
    make_fixture
    # The deployed-sha reader prints the (valid) sha to stdout but a secret token to stderr.
    # The orchestrator captures stdout with 2>/dev/null, so the token must appear nowhere.
    run_orch QA_DEPLOYED_SHA_CMD="printf %s $HEAD_SHA; echo SHASECRET_zzz >&2"
    if grep -rq 'SHASECRET_zzz' "$GHENV" "$OUT" "$ERR"; then fail "reader-noleak: reader stderr leaked into round output"; else ok; fi
    cleanup_fixture
}

test_cmd_vars_stripped_from_children() {
    echo "test: the injected QA_*_CMD vars (secret-bearing) are stripped from make children (judge Codex can't inherit them)"
    make_fixture
    run_orch QA_RESEED_CMD="true" QA_DEPLOYED_GEN_CMD="echo 1"
    assert_rc0 cmd-strip
    # Every recorded make call must show the command vars EMPTY (env -u removed them).
    if grep -qE 'reseed_cmd=true|sha_cmd=[^ ]| gen_cmd=echo' "$CALL_LOG"; then
        fail "cmd-strip: a QA_*_CMD leaked into a make child ($(grep -m1 'reseed_cmd=[^ ]' "$CALL_LOG"))"
    else
        ok
    fi
    cleanup_fixture
}

test_deployed_sha_nul_rejected() {
    echo "test: a deployed-sha reader emitting <sha>+NUL (bash \$() strips NUL) is rejected as malformed -> no advance"
    make_fixture
    run_orch QA_DEPLOYED_SHA_CMD="printf '%s\\000' $HEAD_SHA"
    assert_rc0 sha-nul
    assert_kv advance false sha-nul
    assert_reason "NUL byte" sha-nul
    cleanup_fixture
}

test_reader_gen_stderr_no_leak() {
    echo "test: the deploy-GEN reader's STDERR (secret-bearing) is discarded, not leaked"
    make_fixture
    run_orch QA_DEPLOYED_GEN_CMD="echo 5; echo GENSECRET_zzz >&2"
    if grep -rq 'GENSECRET_zzz' "$GHENV" "$OUT" "$ERR"; then fail "gen-noleak: gen reader stderr leaked"; else ok; fi
    cleanup_fixture
}

test_gen_baseline_before_reseed() {
    echo "test: the deploy-generation baseline is captured BEFORE the reseed -> a redeploy DURING reseed is caught"
    make_fixture
    # gen reader = a counter file that does not exist yet at round start; the reseed writes "1"
    # to it. Baselined BEFORE reseed, gen_pre="0" (file absent) and gen_post="1" -> changed -> no
    # advance. If the baseline were taken AFTER reseed, gen_pre would already be "1" == gen_post
    # and the round would wrongly advance.
    run_orch QA_DEPLOYED_GEN_CMD="cat $FIXTURE/gc 2>/dev/null || echo 0" QA_RESEED_CMD="echo 1 > $FIXTURE/gc"
    assert_rc0 gen-baseline
    assert_kv advance false gen-baseline
    cleanup_fixture
}

test_judge_creds_stripped() {
    echo "test: tours/langfuse API secrets are stripped from the qa-report JUDGE env (Codex can't read them)"
    make_fixture
    run_orch LANGFUSE_SECRET_KEY=sk-SECRET TOURS_API_KEY=tk-SECRET
    assert_rc0 judge-creds
    # The qa-report (judge) call must show BOTH secrets empty (env -u removed them).
    local jline; jline="$(grep '^make qa-report ' "$CALL_LOG")"
    if printf '%s' "$jline" | grep -qE 'lang_secret=sk-SECRET|tours_key=tk-SECRET'; then
        fail "judge-creds: a secret leaked into the judge env ($jline)"
    else
        ok
    fi
    cleanup_fixture
}

test_deploy_gen_nul_no_advance() {
    echo "test: PRE gen read has a NUL that strips to == the (clean) POST read -> false-equal -> no advance (ISOLATES pre-NUL guard)"
    make_fixture
    # Counter: PRE (n=1) prints "A\0B" (raw, with NUL); POST (n=2) prints clean "AB". bash \$()
    # strips the NUL so both become "AB" and would compare EQUAL -> advance. Only the pre-read
    # NUL guard catches it (the post read has no NUL, so the post NUL guard cannot).
    run_orch QA_DEPLOYED_GEN_CMD="n=\$(cat $FIXTURE/gnul 2>/dev/null || echo 0); n=\$((n+1)); echo \$n > $FIXTURE/gnul; if [ \$n -eq 1 ]; then printf 'A\\000B'; else printf 'AB'; fi"
    assert_rc0 gen-nul
    assert_kv advance false gen-nul
    cleanup_fixture
}

test_reseed_after_runid_validation() {
    echo "test: an invalid run id aborts BEFORE the destructive reseed runs (reseed ordering)"
    make_fixture
    # The reseed cmd drops a sentinel if it runs. An invalid run-id must abort first, so the
    # sentinel must NOT appear — proving the reserve/validate step precedes the reseed.
    run_orch QA_RUN_ID_OVERRIDE="../evil2" QA_RESEED_CMD="touch $FIXTURE/reseed-ran"
    assert_rc_nz reseed-order
    assert_reason "invalid run id" reseed-order
    if [ -e "$FIXTURE/reseed-ran" ]; then fail "reseed-order: reseed ran before run-id validation (destructive)"; else ok; fi
    cleanup_fixture
}

# ---------------------------------------------------------------------------
main() {
    test_skip
    test_clean
    test_export_failed
    test_pre_assert_fail
    test_post_assert_fail
    test_reseed_fail
    test_reseed_fail_no_secret_leak
    test_reseed_ok_skips_tours_reset
    test_no_reseed_lets_tours_reset
    test_trap_miss_still_exports
    test_tours_fail
    test_no_enqueue_landed
    test_enqueue_via_already_queued
    test_no_traces_missing_creds
    test_gate_rc_nonzero
    test_missing_judge_trace
    test_missing_deployed_sha_cmd

    test_isolated_export_exit
    test_isolated_failed_only
    test_isolated_enqueue_failed_only
    test_isolated_traces_zero
    test_deployed_sha_moved
    test_deployed_cmd_fails
    test_deployed_cmd_multiline
    test_export_summary_fragment_ignored
    test_export_duplicate_summary
    test_export_summary_with_observations
    test_export_summary_without_observations
    test_runid_collision_aborts
    test_rundir_create_error
    test_deploy_gen_unset_records_false
    test_deploy_gen_matches_clean
    test_deploy_gen_bump_no_advance
    test_deploy_gen_reader_fails
    test_lost_github_env_write

    test_enqueue_inconsistent
    test_export_warning_no_advance
    test_runid_traversal_rejected
    test_deploy_gen_stdout_nonzero
    test_deploy_gen_empty_no_advance
    test_deploy_gen_post_read_fails
    test_trace_freshness_truncated
    test_tours_skip_reset_cleared
    test_media_incomplete_no_advance
    test_reader_stderr_no_leak
    test_cmd_vars_stripped_from_children
    test_deployed_sha_nul_rejected
    test_reader_gen_stderr_no_leak
    test_judge_creds_stripped
    test_deploy_gen_nul_no_advance
    test_reseed_after_runid_validation
    test_gen_baseline_before_reseed

    test_require_tmpdir_fails_closed
    test_git_env_isolation

    echo ""
    echo "===================="
    echo "PASS=$PASS FAIL=$FAIL"
    echo "===================="
    [ "$FAIL" -eq 0 ]
}

main "$@"
