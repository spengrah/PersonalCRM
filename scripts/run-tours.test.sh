#!/usr/bin/env bash
# Tests for run-tours.sh's reset-path selection (the reseed seam).
#
# run-tours.sh picks ONE of three reset behaviors before launching Playwright:
#   TOURS_SKIP_RESET=1   -> skip entirely (use the already-seeded target)
#   TOURS_RESEED_SSH set -> qa forced-command ssh reseed (the QA-sandbox path)
#   else                 -> bash scripts/staging-reset.sh (the Mac / ssh-to-host path)
#
# We run a REWRITTEN COPY of run-tours.sh with the real staging-reset.sh invocation
# rewritten to a recording stub (sed to stdout; the committed script stays byte-pure),
# and `ssh` / `bunx` shadowed on PATH by recording stubs so nothing touches the
# network, a browser, or staging. All checks run anywhere.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$REPO_ROOT/scripts/run-tours.sh"
cd "$REPO_ROOT" # so run-tours.sh's `git rev-parse --show-toplevel` resolves

PASS=0
FAIL=0
fail() { echo "  FAIL: $1" >&2; FAIL=$((FAIL + 1)); }
ok()   { PASS=$((PASS + 1)); }

make_sandbox() {
    SANDBOX="$(mktemp -d)"
    CALL_LOG="$SANDBOX/calls.log"
    : > "$CALL_LOG"
    mkdir -p "$SANDBOX/bin"

    # ssh stub: record argv; emit no stdout (so the image-digest read resolves to
    # 'unknown'); exit code from SSH_STUB_RC (default 0) to simulate reseed failure.
    cat > "$SANDBOX/bin/ssh" <<EOF
#!/usr/bin/env bash
echo "ssh \$*" >> "$CALL_LOG"
exit \${SSH_STUB_RC:-0}
EOF
    # bunx stub: record the playwright launch; exit 0 (no real browser).
    cat > "$SANDBOX/bin/bunx" <<EOF
#!/usr/bin/env bash
echo "bunx \$*" >> "$CALL_LOG"
exit 0
EOF
    # staging-reset.sh stub (the default Mac path): record the call.
    cat > "$SANDBOX/bin/staging-reset.sh" <<EOF
#!/usr/bin/env bash
echo "staging-reset.sh \$*" >> "$CALL_LOG"
exit 0
EOF
    chmod +x "$SANDBOX/bin/ssh" "$SANDBOX/bin/bunx" "$SANDBOX/bin/staging-reset.sh"

    # Rewrite ONLY the real staging-reset.sh invocation to the stub (sed to stdout;
    # '#' delimiter so the path slashes need no escaping). Committed script untouched.
    sed "s#\"\$REPO_ROOT/scripts/staging-reset.sh\"#\"$SANDBOX/bin/staging-reset.sh\"#" \
        "$SCRIPT" > "$SANDBOX/run-tours.sh"
    # Guard: fail loudly if the rewrite no-op'd (the staging-reset.sh invocation
    # string in run-tours.sh drifted) instead of silently exercising the real script.
    grep -q "$SANDBOX/bin/staging-reset.sh" "$SANDBOX/run-tours.sh" \
        || { echo "  FAIL: sed rewrite no-op — staging-reset.sh invocation drifted" >&2; exit 1; }
    chmod +x "$SANDBOX/run-tours.sh"
}

cleanup_sandbox() { [ -n "${SANDBOX:-}" ] && rm -rf "$SANDBOX"; }

# run <ENV=val...> : run the rewritten copy with stubs on PATH; sets RC + CALL_LOG.
run() {
    : > "$CALL_LOG"
    env PATH="$SANDBOX/bin:$PATH" \
        TOURS_BASE_URL="http://staging.test" TOURS_API_KEY="k" \
        "$@" \
        bash "$SANDBOX/run-tours.sh" >/dev/null 2>&1
    RC=$?
}

echo "run-tours.sh reset-path selection tests"
make_sandbox
trap cleanup_sandbox EXIT

# 1. TOURS_SKIP_RESET=1 -> no reseed, no staging-reset, still launches playwright.
run TOURS_SKIP_RESET=1
grep -q 'staging-reset.sh' "$CALL_LOG" && fail "skip: staging-reset must NOT run" || ok
grep -qE 'ssh .*reseed' "$CALL_LOG" && fail "skip: reseed ssh must NOT run" || ok
grep -q 'bunx .*playwright' "$CALL_LOG" && ok || fail "skip: playwright should launch"

# 2. TOURS_RESEED_SSH (+key) -> qa reseed ssh with -i, NOT staging-reset.
run TOURS_RESEED_SSH="qa-staging@10.100.0.1" TOURS_RESEED_KEY="/tmp/k"
grep -qE 'ssh .*-i /tmp/k .*qa-staging@10.100.0.1 reseed' "$CALL_LOG" && ok \
    || fail "reseed: expected qa reseed ssh '-i /tmp/k ... qa-staging@10.100.0.1 reseed'"
grep -q 'staging-reset.sh' "$CALL_LOG" && fail "reseed: staging-reset must NOT run" || ok
grep -q 'bunx .*playwright' "$CALL_LOG" && ok || fail "reseed: playwright should launch"

# 2b. TOURS_RESEED_SSH without a key -> ssh reseed, no -i.
run TOURS_RESEED_SSH="qa-staging@10.100.0.1"
grep -qE 'ssh .*qa-staging@10.100.0.1 reseed' "$CALL_LOG" && ok || fail "reseed(no key): expected qa reseed ssh"
grep -qE 'ssh -o [^;]*-i ' "$CALL_LOG" && fail "reseed(no key): must not pass -i" || ok

# 2c. Reseed ssh FAILS -> fail loud: script aborts, playwright NOT launched.
run TOURS_RESEED_SSH="qa-staging@10.100.0.1" SSH_STUB_RC=1
[ "$RC" -ne 0 ] && ok || fail "reseed-fail: script should exit non-zero"
grep -q 'bunx .*playwright' "$CALL_LOG" && fail "reseed-fail: playwright must NOT launch after a failed reseed" || ok

# 3. Default -> staging-reset.sh runs, no reseed ssh.
run
grep -q 'staging-reset.sh' "$CALL_LOG" && ok || fail "default: staging-reset.sh should run"
grep -qE 'ssh .*reseed' "$CALL_LOG" && fail "default: reseed ssh must NOT run" || ok

# 4. Precedence: TOURS_SKIP_RESET=1 wins over TOURS_RESEED_SSH when both are set.
run TOURS_SKIP_RESET=1 TOURS_RESEED_SSH="qa-staging@10.100.0.1" TOURS_RESEED_KEY="/tmp/k"
grep -qE 'ssh .*reseed' "$CALL_LOG" && fail "precedence: skip must win over reseed ssh" || ok
grep -q 'staging-reset.sh' "$CALL_LOG" && fail "precedence: staging-reset must NOT run" || ok
grep -q 'bunx .*playwright' "$CALL_LOG" && ok || fail "precedence: playwright should launch"

echo "  PASS=$PASS FAIL=$FAIL"
[ "$FAIL" -eq 0 ]
