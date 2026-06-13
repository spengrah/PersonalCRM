#!/usr/bin/env bash
# Tests for trigger-mac-deploy.sh (the runner-side thin Mac deploy trigger).
#
# These run anywhere (no Mac, no real launchctl, no network). PATH is shadowed
# with a stub `launchctl` (and a stub `id`) that records its argv to a per-test
# call log and drives its exit codes via STUB_* env. Each test drives a scenario
# (timer loaded vs not; kickstart exit 0 vs non-zero) and asserts on the recorded
# calls + the script exit code. The real-launchctl path — and the
# runner-session->login-session crossing the redesign bets on — is the Bucket-B
# supervised dry-run, NOT this suite; launchctl is reached ONLY through a stub so
# the suite is green on the Ubuntu CI backend runner too.
#
# Asserts the load-bearing D1 classification:
#   - timer NOT loaded (launchctl print non-zero) -> red exit + the
#     load-the-timer remediation hint + NO kickstart attempted.
#   - timer loaded + kickstart exit 0 -> green exit + the "trigger sent, watch
#     ntfy" fire-and-forget line.
#   - timer loaded + kickstart non-zero -> red exit + the captured launchctl
#     stderr surfaced.
#   - kickstart is NEVER invoked with -k (no kill-mid-codesign).
#   - the target is gui/<uid>/<label> with the configured label.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$REPO_ROOT/scripts/trigger-mac-deploy.sh"
LABEL="xyz.spengrah.crm-mac-deploy"

PASS=0
FAIL=0
fail() { echo "  FAIL: $1" >&2; FAIL=$((FAIL + 1)); }
ok()   { PASS=$((PASS + 1)); }

# ---------------------------------------------------------------------------
# Test sandbox: a fresh tmp dir per scenario with a stub bin/ + a call log.
# Sets globals SANDBOX, CALL_LOG.
# ---------------------------------------------------------------------------
make_sandbox() {
    SANDBOX="$(mktemp -d)"
    CALL_LOG="$SANDBOX/calls.log"
    : > "$CALL_LOG"
    mkdir -p "$SANDBOX/bin"

    # --- stub: launchctl ---------------------------------------------------
    #   print <target>     -> exit ${STUB_PRINT_RC:-0} (is the timer loaded?)
    #   kickstart <target> -> emit ${STUB_KICKSTART_STDERR} on stderr,
    #                         exit ${STUB_KICKSTART_RC:-0}
    cat > "$SANDBOX/bin/launchctl" <<EOF
#!/usr/bin/env bash
echo "launchctl \$*" >> "$CALL_LOG"
case "\$1" in
  print)     exit "\${STUB_PRINT_RC:-0}" ;;
  kickstart)
    if [ -n "\${STUB_KICKSTART_STDERR:-}" ]; then
      echo "\${STUB_KICKSTART_STDERR}" >&2
    fi
    exit "\${STUB_KICKSTART_RC:-0}" ;;
  *) exit 0 ;;
esac
EOF

    # --- stub: id (stable uid without depending on the host) ---------------
    cat > "$SANDBOX/bin/id" <<'EOF'
#!/usr/bin/env bash
if [ "$1" = "-u" ]; then echo 4242; exit 0; fi
exec /usr/bin/id "$@"
EOF

    chmod +x "$SANDBOX"/bin/*
}

cleanup_sandbox() { [ -n "${SANDBOX:-}" ] && rm -rf "$SANDBOX"; }

# run_trigger : run trigger-mac-deploy.sh in the sandbox; sets RC + OUT. Honors
# any STUB_* env the caller exported.
run_trigger() {
    OUT="$(
        PATH="$SANDBOX/bin:$PATH" \
        bash "$SCRIPT" 2>&1
    )"
    RC=$?
}

log_has()   { grep -qF -- "$1" "$CALL_LOG"; }
log_lacks() { ! grep -qF -- "$1" "$CALL_LOG"; }
out_has()   { printf '%s' "$OUT" | grep -qF -- "$1"; }

# ===========================================================================
# Tests
# ===========================================================================

test_not_loaded_red_with_hint_no_kickstart() {
    echo "test: timer NOT loaded -> red + load-the-timer hint + NO kickstart"
    make_sandbox
    STUB_PRINT_RC=1 run_trigger
    if [ "$RC" -ne 0 ]; then ok; else fail "not-loaded must exit non-zero, got $RC"; fi
    if log_has "print gui/4242/$LABEL"; then ok; else fail "must probe the gui/<uid>/<label> target"; fi
    if out_has "make setup-mac-deploy"; then ok; else fail "not-loaded must print the load-the-timer hint"; fi
    if log_lacks "kickstart"; then ok; else fail "not-loaded must NOT attempt a kickstart"; fi
    cleanup_sandbox
}

test_loaded_kickstart_ok_green_with_watch_line() {
    echo "test: timer loaded + kickstart 0 -> green + the 'trigger sent, watch ntfy' line"
    make_sandbox
    STUB_PRINT_RC=0 STUB_KICKSTART_RC=0 run_trigger
    if [ "$RC" -eq 0 ]; then ok; else fail "loaded + kickstart 0 must exit 0, got $RC ($OUT)"; fi
    if log_has "kickstart gui/4242/$LABEL"; then ok; else fail "must kickstart the gui/<uid>/<label> target"; fi
    if out_has "trigger was SENT"; then ok; else fail "green path must print the fire-and-forget 'trigger sent' line"; fi
    if out_has "watch ntfy"; then ok; else fail "green path must tell the operator to watch ntfy for the real result"; fi
    cleanup_sandbox
}

test_loaded_kickstart_nonzero_red_with_stderr() {
    echo "test: timer loaded + kickstart non-zero -> red + captured launchctl stderr surfaced"
    make_sandbox
    STUB_PRINT_RC=0 STUB_KICKSTART_RC=5 STUB_KICKSTART_STDERR="launchd refused: service disabled" run_trigger
    if [ "$RC" -ne 0 ]; then ok; else fail "kickstart non-zero must exit non-zero, got $RC"; fi
    if out_has "launchd refused: service disabled"; then ok; else fail "kickstart non-zero must surface the captured launchctl stderr"; fi
    if out_has "exited 5"; then ok; else fail "kickstart non-zero must report the captured exit code"; fi
    cleanup_sandbox
}

test_never_passes_dash_k() {
    echo "test: kickstart is NEVER invoked with -k (no kill-mid-codesign)"
    make_sandbox
    STUB_PRINT_RC=0 STUB_KICKSTART_RC=0 run_trigger
    # The recorded call must be a plain `kickstart <target>` with no -k flag.
    if log_lacks "kickstart -k"; then ok; else fail "kickstart must NOT pass -k (would kill a mid-codesign build)"; fi
    cleanup_sandbox
}

test_honors_label_override() {
    echo "test: CRM_MAC_LAUNCH_AGENT_LABEL override changes the target label"
    make_sandbox
    CRM_MAC_LAUNCH_AGENT_LABEL="xyz.spengrah.custom-label" STUB_PRINT_RC=0 STUB_KICKSTART_RC=0 run_trigger
    if [ "$RC" -eq 0 ]; then ok; else fail "override happy path must exit 0, got $RC ($OUT)"; fi
    if log_has "kickstart gui/4242/xyz.spengrah.custom-label"; then ok; else fail "override label must be used in the kickstart target"; fi
    cleanup_sandbox
}

# ---------------------------------------------------------------------------
main() {
    test_not_loaded_red_with_hint_no_kickstart
    test_loaded_kickstart_ok_green_with_watch_line
    test_loaded_kickstart_nonzero_red_with_stderr
    test_never_passes_dash_k
    test_honors_label_override

    echo ""
    echo "===================="
    echo "PASS=$PASS FAIL=$FAIL"
    echo "===================="
    [ "$FAIL" -eq 0 ]
}

main "$@"
