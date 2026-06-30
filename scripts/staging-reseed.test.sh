#!/usr/bin/env bash
# Tests for staging-reseed.sh — the root-owned auto-reseed wrapper (env-trust seam).
#
# The wrapper forces CRM_USER=staging + CRM_HOME=/var/lib/staging and execs
# staging-reset.sh with EXACTLY `--local --require-oauth-empty` (the oauth guard is
# pinned in the wrapper, not workflow-controllable). The wrapper hardcodes the
# absolute /usr/local/sbin/staging-reset.sh path (that IS the seam), so the test
# rewrites that one path to a recording stub via sed-to-stdout (portable; no sed -i)
# and runs the rewritten copy — the committed wrapper stays byte-pure.
#
# All checks run anywhere (no Pi/podman/root).

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
WRAPPER="$REPO_ROOT/scripts/staging-reseed.sh"

PASS=0
FAIL=0
fail() { echo "  FAIL: $1" >&2; FAIL=$((FAIL + 1)); }
ok()   { PASS=$((PASS + 1)); }

make_sandbox() {
    SANDBOX="$(mktemp -d)"
    CALL_LOG="$SANDBOX/calls.log"
    : > "$CALL_LOG"
    mkdir -p "$SANDBOX/bin"

    cat > "$SANDBOX/bin/staging-reset.sh" <<EOF
#!/usr/bin/env bash
echo "argc=\$#" >> "$CALL_LOG"
echo "args=[\$*]" >> "$CALL_LOG"
echo "env CRM_USER=\${CRM_USER:-<unset>} CRM_HOME=\${CRM_HOME:-<unset>}" >> "$CALL_LOG"
exit 0
EOF
    chmod +x "$SANDBOX/bin/staging-reset.sh"

    # Rewrite ONLY the hardcoded exec path to the stub (sed to stdout; '#'
    # delimiter so the path slashes need no escaping). Committed wrapper untouched.
    sed "s#/usr/local/sbin/staging-reset.sh#$SANDBOX/bin/staging-reset.sh#" \
        "$WRAPPER" > "$SANDBOX/staging-reseed.sh"
}

cleanup_sandbox() { [ -n "${SANDBOX:-}" ] && rm -rf "$SANDBOX"; }

run_wrapper() {
    bash "$SANDBOX/staging-reseed.sh" "$@" >/dev/null 2>&1
    RC=$?
}

# ===========================================================================
test_wrapper_execs_reset_with_oauth_flag() {
    echo "test: wrapper execs staging-reset.sh with EXACTLY --local --require-oauth-empty + tenant"
    make_sandbox
    run_wrapper
    if [ "$RC" -eq 0 ]; then ok; else fail "wrapper should exit 0, got $RC"; fi
    if grep -qF "args=[--local --require-oauth-empty]" "$CALL_LOG"; then ok
    else fail "wrapper must exec with exactly '--local --require-oauth-empty'"; fi
    if grep -qF "argc=2" "$CALL_LOG"; then ok; else fail "wrapper must pass exactly two args"; fi
    if grep -qF "env CRM_USER=staging CRM_HOME=/var/lib/staging" "$CALL_LOG"; then ok
    else fail "wrapper must export CRM_USER=staging + CRM_HOME=/var/lib/staging"; fi
    cleanup_sandbox
}

test_wrapper_overrides_caller_supplied_tenant() {
    echo "test: wrapper OVERRIDES a caller-supplied CRM_USER/CRM_HOME (defense-in-depth)"
    make_sandbox
    CRM_USER=attacker CRM_HOME=/evil run_wrapper
    if grep -qF "env CRM_USER=staging CRM_HOME=/var/lib/staging" "$CALL_LOG"; then ok
    else fail "wrapper must override a caller-supplied tenant with staging"; fi
    if grep -qF "CRM_USER=attacker" "$CALL_LOG"; then fail "caller tenant leaked through the wrapper"; else ok; fi
    cleanup_sandbox
}

test_committed_wrapper_pins_flags() {
    echo "test: committed wrapper execs the absolute reset path with the pinned flags"
    if grep -qE 'exec[[:space:]]+/usr/local/sbin/staging-reset\.sh[[:space:]]+--local[[:space:]]+--require-oauth-empty' "$WRAPPER"; then ok
    else fail "committed wrapper must 'exec /usr/local/sbin/staging-reset.sh --local --require-oauth-empty'"; fi
    if grep -qE '^export CRM_USER=staging' "$WRAPPER"; then ok; else fail "wrapper must export CRM_USER=staging"; fi
    if grep -qE '^export CRM_HOME=/var/lib/staging' "$WRAPPER"; then ok; else fail "wrapper must export CRM_HOME=/var/lib/staging"; fi
    # The wrapper execs staging-reset.sh DIRECTLY (no sudo of its own); the env-trust
    # seam against `sudo -E`/--preserve-env is enforced on the workflow side
    # (deploy-staging.test.sh::test_workflow_no_env_passthrough).
}

# ---------------------------------------------------------------------------
main() {
    test_wrapper_execs_reset_with_oauth_flag
    test_wrapper_overrides_caller_supplied_tenant
    test_committed_wrapper_pins_flags

    echo ""
    echo "===================="
    echo "PASS=$PASS FAIL=$FAIL"
    echo "===================="
    [ "$FAIL" -eq 0 ]
}

main "$@"
