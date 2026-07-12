#!/usr/bin/env bash
# Tests for setup-staging-reseed-host.sh — the staging reseed provisioning helper.
#
# PATH-shadowed stubs (id/install/visudo/stat) + sandbox destination dirs
# (USRLOCALSBIN/SUDOERSD seams) so the install + sudoers writes happen without root.
# The install stub records its argv AND copies src->dst so the generated sudoers
# drop-in is real and its content can be asserted. Covers: the 3-script install
# (root:root/0755), the exactly-2 args-free sudoers lines (no SETENV/env_keep), the
# $RUNNER_USER principal (default + override), visudo ordering, the fail-loud
# preconditions, and idempotency.
#
# Portability: no BSD-only stat -f / sed -i.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
SCRIPT="$REPO_ROOT/scripts/admin/setup-staging-reseed-host.sh"

PASS=0
FAIL=0
fail() { echo "  FAIL: $1" >&2; FAIL=$((FAIL + 1)); }
ok()   { PASS=$((PASS + 1)); }

make_sandbox() {
    SANDBOX="$(mktemp -d)"
    CALL_LOG="$SANDBOX/calls.log"
    : > "$CALL_LOG"
    mkdir -p "$SANDBOX/bin" "$SANDBOX/sbin" "$SANDBOX/sudoers.d"
    # deploy-staging.sh is a precondition (installed by the earlier standup).
    echo '#!/bin/bash' > "$SANDBOX/sbin/deploy-staging.sh"

    cat > "$SANDBOX/bin/id" <<EOF
#!/usr/bin/env bash
echo "id \$*" >> "$CALL_LOG"
if [ "\$1" = "-u" ]; then
    if [ "\${STUB_NOT_ROOT:-0}" = "1" ]; then echo 1000; else echo 0; fi
    exit 0
fi
# id <user>: exit 0 only for a KNOWN runner account.
case "\$1" in
    gha-runner|custom-runner) exit 0 ;;
    *) exit 1 ;;
esac
EOF

    # install stub: record argv, then copy the last two positional args (src dst)
    # so the sudoers drop-in is a real file whose content the test can assert.
    cat > "$SANDBOX/bin/install" <<EOF
#!/usr/bin/env bash
echo "install \$*" >> "$CALL_LOG"
args=("\$@")
n=\${#args[@]}
# cp -f mimics install(1)'s atomic replace: overwrite even a 0440 dest (a plain
# cp cannot reopen a read-only file, which would break the idempotent second run).
cp -f "\${args[\$((n-2))]}" "\${args[\$((n-1))]}"
EOF

    cat > "$SANDBOX/bin/visudo" <<EOF
#!/usr/bin/env bash
echo "visudo \$*" >> "$CALL_LOG"
exit "\${STUB_VISUDO_RC:-0}"
EOF

    cat > "$SANDBOX/bin/stat" <<EOF
#!/usr/bin/env bash
echo "stat \$*" >> "$CALL_LOG"
echo "root root 755"
exit 0
EOF

    chmod +x "$SANDBOX"/bin/*
}

cleanup_sandbox() { [ -n "${SANDBOX:-}" ] && rm -rf "$SANDBOX"; }

# run_setup : run the provisioning script with the sandbox stubs + destinations.
# Caller may prefix env (e.g. RUNNER_USER=custom-runner run_setup, STUB_NOT_ROOT=1).
run_setup() {
    PATH="$SANDBOX/bin:$PATH" \
        USRLOCALSBIN="$SANDBOX/sbin" \
        SUDOERSD="$SANDBOX/sudoers.d" \
        bash "$SCRIPT" --local >/dev/null 2>"$SANDBOX/stderr"
    RC=$?
}

# setup_ssh_stubs : add ssh + tar stubs so ssh mode (the default) can be exercised
# without a real host. The ssh stub records argv, drains any piped tarball, and
# answers the three call shapes the script makes: the reachability probe
# (-o ConnectTimeout), the bundle-ship step (mktemp — echoes a fake remote dir so
# the caller's $(...) capture is non-empty), and the remote run step (sudo).
setup_ssh_stubs() {
    cat > "$SANDBOX/bin/ssh" <<EOF
#!/usr/bin/env bash
echo "ssh \$*" >> "$CALL_LOG"
cat >/dev/null 2>&1 || true   # drain a piped tarball if present
case " \$* " in
    *ConnectTimeout*) exit 0 ;;                       # reachability probe
    *mktemp*)         echo "$SANDBOX/remote_stage" ;; # bundle-ship: echo remote dir
    *sudo*)                                           # remote --local run
        # Capture the remote command (the last positional arg) so a test can
        # re-parse it under a controlled shell and prove %q quoting + rc handling.
        for _last in "\$@"; do :; done
        printf '%s' "\$_last" > "$SANDBOX/remote_cmd.txt"
        exit "\${STUB_SSH_RUN_RC:-0}" ;;
    *)                exit 0 ;;
esac
EOF
    cat > "$SANDBOX/bin/tar" <<EOF
#!/usr/bin/env bash
echo "tar \$*" >> "$CALL_LOG"
printf 'TARBYTES'   # give the pipe some content
exit 0
EOF
    chmod +x "$SANDBOX/bin/ssh" "$SANDBOX/bin/tar"
}

# run_ssh : run the script in ssh mode (default, no --local). Caller may prefix env.
# STAGING_HOST is explicit: the script has no committed default (the real alias is
# deliberately absent from tracked artifacts), so ssh mode requires it to be set.
run_ssh() {
    PATH="$SANDBOX/bin:$PATH" STAGING_HOST="${STAGING_HOST:-staging.test.invalid}" \
        bash "$SCRIPT" >/dev/null 2>"$SANDBOX/stderr"
    RC=$?
}

DROPIN() { echo "$SANDBOX/sudoers.d/gha-runner-staging-reseed"; }

# ===========================================================================
test_installs_three_scripts_root_0755() {
    echo "test: installs all 3 scripts with -o root -g root -m 0755"
    make_sandbox
    run_setup
    if [ "$RC" -eq 0 ]; then ok; else fail "happy path should exit 0, got $RC ($(cat "$SANDBOX/stderr"))"; fi
    local name n
    for name in staging-reset.sh staging-reseed.sh staging-deployed-sha.sh; do
        if grep -F 'install ' "$CALL_LOG" | grep -F -- '-o root -g root -m 0755' | grep -q "/sbin/$name"; then ok
        else fail "must install $name root:root 0755 to the sbin dir"; fi
    done
    # Exactly 3 script installs at 0755 (the sudoers install is 0440, not counted).
    n=$(grep -F 'install ' "$CALL_LOG" | grep -c -- '-m 0755')
    if [ "$n" -eq 3 ]; then ok; else fail "expected exactly 3 0755 installs, got $n"; fi
    cleanup_sandbox
}

test_sudoers_two_lines_no_setenv() {
    echo "test: sudoers drop-in has exactly 2 args-free NOPASSWD lines, no SETENV/env_keep"
    make_sandbox
    run_setup
    local f n
    f="$(DROPIN)"
    if [ -f "$f" ]; then ok; else fail "sudoers drop-in not written to $f"; cleanup_sandbox; return; fi
    n=$(grep -c 'NOPASSWD:' "$f")
    if [ "$n" -eq 2 ]; then ok; else fail "expected exactly 2 NOPASSWD lines, got $n"; fi
    if grep -qF 'NOPASSWD: /usr/local/sbin/staging-reseed.sh' "$f"; then ok; else fail "missing the staging-reseed.sh sudoers line"; fi
    if grep -qF 'NOPASSWD: /usr/local/sbin/staging-deployed-sha.sh' "$f"; then ok; else fail "missing the staging-deployed-sha.sh sudoers line"; fi
    # staging-reset.sh must NOT get a sudoers line (the runner never sudo-calls it).
    if grep -q 'staging-reset.sh' "$f"; then fail "staging-reset.sh must NOT get a sudoers line"; else ok; fi
    if grep -q 'SETENV' "$f"; then fail "sudoers must NOT contain SETENV"; else ok; fi
    if grep -q 'env_keep' "$f"; then fail "sudoers must NOT contain env_keep"; else ok; fi
    cleanup_sandbox
}

test_sudoers_principal_default_and_override() {
    echo "test: sudoers principal is \$RUNNER_USER (default gha-runner; override honored)"
    make_sandbox
    run_setup
    local f
    f="$(DROPIN)"
    if grep -qE '^gha-runner ALL=' "$f"; then ok; else fail "default principal must be gha-runner"; fi
    cleanup_sandbox
    # Override with a KNOWN custom runner: the lines must start with it (proves the
    # override actually grants the capability to the real account, not a dead knob).
    make_sandbox
    RUNNER_USER=custom-runner run_setup
    f="$(DROPIN)"
    if [ "$RC" -eq 0 ]; then ok; else fail "override run should exit 0, got $RC ($(cat "$SANDBOX/stderr"))"; fi
    if grep -qE '^custom-runner ALL=' "$f"; then ok; else fail "override principal must be custom-runner"; fi
    if grep -qE '^gha-runner ALL=' "$f"; then fail "override must not leave gha-runner as the principal"; else ok; fi
    cleanup_sandbox
}

test_visudo_validates_before_install_and_after() {
    echo "test: visudo -cf runs on the temp file BEFORE the sudoers install, and on the installed file after"
    make_sandbox
    run_setup
    local i_first_visudo i_sudoers_install i_revalidate
    i_first_visudo=$(grep -nF 'visudo -cf' "$CALL_LOG" | head -1 | cut -d: -f1)
    i_sudoers_install=$(grep -nF 'install ' "$CALL_LOG" | grep -F 'gha-runner-staging-reseed' | head -1 | cut -d: -f1)
    i_revalidate=$(grep -nF 'visudo -cf' "$CALL_LOG" | grep -F 'gha-runner-staging-reseed' | head -1 | cut -d: -f1)
    if [ -n "$i_first_visudo" ] && [ -n "$i_sudoers_install" ] && [ "$i_first_visudo" -lt "$i_sudoers_install" ]; then ok
    else fail "visudo must validate the temp file BEFORE installing it (first=$i_first_visudo install=$i_sudoers_install)"; fi
    if [ -n "$i_revalidate" ] && [ -n "$i_sudoers_install" ] && [ "$i_sudoers_install" -lt "$i_revalidate" ]; then ok
    else fail "visudo must re-validate the installed file AFTER install (install=$i_sudoers_install reval=$i_revalidate)"; fi
    cleanup_sandbox
}

test_fail_not_root() {
    echo "test: fail-loud when not root"
    make_sandbox
    STUB_NOT_ROOT=1 run_setup
    if [ "$RC" -ne 0 ]; then ok; else fail "non-root must fail, got $RC"; fi
    if grep -q 'must run as root' "$SANDBOX/stderr"; then ok; else fail "must print a root-required message"; fi
    cleanup_sandbox
}

test_fail_missing_runner_user() {
    echo "test: fail-loud when the runner user is absent (staging remediation, not the Pi runbook)"
    make_sandbox
    RUNNER_USER=ghost-runner run_setup
    if [ "$RC" -ne 0 ]; then ok; else fail "missing runner user must fail, got $RC"; fi
    if grep -q 'self-hosted, staging' "$SANDBOX/stderr"; then ok; else fail "message must reference the staging runner context"; fi
    cleanup_sandbox
}

test_fail_missing_deploy_staging() {
    echo "test: fail-loud when deploy-staging.sh is not installed (partial-standup refusal)"
    make_sandbox
    rm -f "$SANDBOX/sbin/deploy-staging.sh"
    run_setup
    if [ "$RC" -ne 0 ]; then ok; else fail "missing deploy-staging.sh must fail, got $RC"; fi
    if grep -q 'deploy-staging.sh' "$SANDBOX/stderr"; then ok; else fail "message must reference deploy-staging.sh"; fi
    # Must refuse BEFORE writing the sudoers drop-in.
    if [ -f "$(DROPIN)" ]; then fail "must not write the sudoers drop-in on a partial standup"; else ok; fi
    cleanup_sandbox
}

test_fail_missing_source_script() {
    echo "test: fail-loud when a source script is missing from the checkout"
    make_sandbox
    # Build a fake repo checkout missing one of the three source scripts, and run a
    # copy of the setup script from it (REPO_ROOT resolves relative to its location).
    local fake="$SANDBOX/fakerepo"
    mkdir -p "$fake/scripts/admin"
    cp "$SCRIPT" "$fake/scripts/admin/setup-staging-reseed-host.sh"
    echo '#!/bin/bash' > "$fake/scripts/staging-reseed.sh"
    echo '#!/bin/bash' > "$fake/scripts/staging-deployed-sha.sh"
    # staging-reset.sh deliberately absent.
    PATH="$SANDBOX/bin:$PATH" \
        USRLOCALSBIN="$SANDBOX/sbin" \
        SUDOERSD="$SANDBOX/sudoers.d" \
        bash "$fake/scripts/admin/setup-staging-reseed-host.sh" --local >/dev/null 2>"$SANDBOX/stderr"
    RC=$?
    if [ "$RC" -ne 0 ]; then ok; else fail "missing source script must fail, got $RC"; fi
    if grep -q 'staging-reset.sh' "$SANDBOX/stderr"; then ok; else fail "message must name the missing source script"; fi
    cleanup_sandbox
}

test_idempotent_second_run() {
    echo "test: running twice converges (second run exits 0, sudoers still exactly 2 lines)"
    make_sandbox
    run_setup
    local first_rc="$RC" f n
    run_setup
    f="$(DROPIN)"
    if [ "$first_rc" -eq 0 ] && [ "$RC" -eq 0 ]; then ok; else fail "both runs must exit 0 (first=$first_rc second=$RC)"; fi
    n=$(grep -c 'NOPASSWD:' "$f")
    if [ "$n" -eq 2 ]; then ok; else fail "sudoers must still have exactly 2 lines after a second run, got $n"; fi
    cleanup_sandbox
}

test_ssh_mode_requires_staging_host() {
    echo "test: ssh mode refuses when STAGING_HOST is unset (no committed host default)"
    make_sandbox
    setup_ssh_stubs
    # Deliberately NOT going through run_ssh (which supplies a test host): the host
    # alias is kept out of tracked artifacts, so an unset STAGING_HOST must fail loudly
    # rather than silently target some baked-in default. Run a copy from a fake repo
    # root that has NO .env, so the real checkout's .env fallback cannot satisfy it —
    # otherwise this test would silently stop failing once a dev configures STAGING_HOST.
    local fake="$SANDBOX/fakerepo"
    mkdir -p "$fake/scripts/admin"
    cp "$SCRIPT" "$fake/scripts/admin/setup-staging-reseed-host.sh"
    for s in staging-reset.sh staging-reseed.sh staging-deployed-sha.sh; do
        echo '#!/bin/bash' > "$fake/scripts/$s"
    done
    PATH="$SANDBOX/bin:$PATH" STAGING_HOST="" \
        bash "$fake/scripts/admin/setup-staging-reseed-host.sh" >/dev/null 2>"$SANDBOX/stderr"
    RC=$?
    if [ "$RC" -ne 0 ]; then ok; else fail "unset STAGING_HOST must exit non-zero, got 0"; fi
    if grep -q 'STAGING_HOST is not set' "$SANDBOX/stderr"; then ok; else fail "must name STAGING_HOST in the error: $(cat "$SANDBOX/stderr")"; fi
    if grep -qF 'ssh ' "$CALL_LOG" 2>/dev/null; then fail "must not contact any host when STAGING_HOST is unset"; else ok; fi
    cleanup_sandbox
}

test_ssh_mode_bootstraps_remote() {
    echo "test: ssh mode (default) ships the bundle and runs the installer --local on the host"
    make_sandbox
    setup_ssh_stubs
    # No --local, and STUB_NOT_ROOT=1 to prove ssh mode does NOT require local root.
    STUB_NOT_ROOT=1 run_ssh
    if [ "$RC" -eq 0 ]; then ok; else fail "ssh mode should exit 0, got $RC ($(cat "$SANDBOX/stderr"))"; fi
    # ships the installer via tar
    if grep -F 'tar ' "$CALL_LOG" | grep -q 'setup-staging-reseed-host.sh'; then ok; else fail "must tar the installer bundle"; fi
    # remote command runs the installer in --local mode, via sudo, with the runner flag
    if grep -F 'ssh ' "$CALL_LOG" | grep -F 'sudo' | grep -q -- '--local'; then ok; else fail "remote command must sudo-run the installer with --local"; fi
    if grep -F 'ssh ' "$CALL_LOG" | grep -q -- '--runner-user gha-runner'; then ok; else fail "remote command must thread --runner-user (default gha-runner)"; fi
    # uses an interactive TTY so sudo can prompt
    if grep -F 'ssh ' "$CALL_LOG" | grep -F 'sudo' | grep -q -- '-t'; then ok; else fail "remote run must use ssh -t for the sudo prompt"; fi
    # cleans up the remote temp dir
    if grep -F 'ssh ' "$CALL_LOG" | grep -q 'rm -rf'; then ok; else fail "remote command must remove the temp dir"; fi
    # no local install happened (the real work is on the far side)
    if grep -qF 'install ' "$CALL_LOG"; then fail "ssh mode must NOT install locally"; else ok; fi
    cleanup_sandbox
}

test_ssh_mode_runner_user_override() {
    echo "test: ssh mode threads a RUNNER_USER override to the remote --runner-user flag"
    make_sandbox
    setup_ssh_stubs
    RUNNER_USER=custom-runner run_ssh
    if [ "$RC" -eq 0 ]; then ok; else fail "ssh override run should exit 0, got $RC ($(cat "$SANDBOX/stderr"))"; fi
    if grep -F 'ssh ' "$CALL_LOG" | grep -q -- '--runner-user custom-runner'; then ok; else fail "override must pass --runner-user custom-runner to the remote"; fi
    cleanup_sandbox
}

test_ssh_mode_quotes_hostile_runner_user() {
    echo "test: ssh mode %q-quotes a hostile RUNNER_USER (no injection; value survives as one token)"
    make_sandbox
    setup_ssh_stubs
    # A value with a shell metachar + command separator. ssh mode does NOT validate
    # the runner locally (that is the remote --local's job), so it flows straight
    # into the remote command string — the exact thing printf %q must neutralize.
    local hostile='a b; touch pwned'
    RUNNER_USER="$hostile" run_ssh
    if [ "$RC" -eq 0 ]; then ok; else fail "ssh mode run should exit 0, got $RC ($(cat "$SANDBOX/stderr"))"; fi
    local rc_file="$SANDBOX/remote_cmd.txt"
    if [ ! -f "$rc_file" ]; then fail "no remote command captured"; cleanup_sandbox; return; fi

    # Re-parse the captured remote command under a controlled shell. A `sudo` stub
    # records the args it receives (the `bash <installer> --local --runner-user X`
    # invocation) instead of running them, so a failed %q cannot do harm and the
    # reconstructed argv is captured. `touch` is deliberately REAL — if %q failed,
    # the injected `; touch pwned` would run as its own statement and create the file.
    # (The stub is named `sudo`, NOT `bash`, so a bash-named stub can't recurse into
    # itself via its own #!/usr/bin/env bash shebang.)
    mkdir -p "$SANDBOX/parsebin"
    cat > "$SANDBOX/parsebin/sudo" <<PB
#!/usr/bin/env bash
printf '%s\n' "\$@" > "$SANDBOX/remote_argv.txt"
exit 0
PB
    chmod +x "$SANDBOX/parsebin/sudo"
    ( cd "$SANDBOX" && PATH="$SANDBOX/parsebin:$PATH" bash -c "$(cat "$rc_file")" ) >/dev/null 2>&1 || true

    # No injection: the metacharacter payload must NOT have executed.
    if [ -e "$SANDBOX/pwned" ]; then fail "injection: 'pwned' created — %q did not neutralize the hostile value"; else ok; fi
    # The runner value must reconstruct as EXACTLY one token equal to the original.
    if [ -f "$SANDBOX/remote_argv.txt" ]; then
        local val
        val="$(awk '/^--runner-user$/{getline; print; exit}' "$SANDBOX/remote_argv.txt")"
        if [ "$val" = "$hostile" ]; then ok; else fail "runner-user not reconstructed as one token (got: '$val')"; fi
    else
        fail "installer argv not captured from the reconstructed remote command"
    fi
    cleanup_sandbox
}

test_ssh_mode_propagates_remote_rc_and_cleans_up() {
    echo "test: ssh mode propagates the remote exit code and still removes the temp dir"
    make_sandbox
    setup_ssh_stubs
    STUB_SSH_RUN_RC=7 run_ssh
    if [ "$RC" -eq 7 ]; then ok; else fail "remote rc must propagate to the local exit, got $RC"; fi
    # Cleanup is part of the remote command, so it runs even when the install fails.
    if [ -f "$SANDBOX/remote_cmd.txt" ] && grep -q 'rm -rf' "$SANDBOX/remote_cmd.txt"; then ok
    else fail "remote command must remove the temp dir even on a failed run"; fi
    cleanup_sandbox
}

test_ssh_mode_missing_local_source_fails_before_ssh() {
    echo "test: ssh mode validates local sources BEFORE contacting the host"
    make_sandbox
    setup_ssh_stubs
    # Run a copy from a fake checkout missing one source script; ssh mode (default).
    local fake="$SANDBOX/fakerepo"
    mkdir -p "$fake/scripts/admin"
    cp "$SCRIPT" "$fake/scripts/admin/setup-staging-reseed-host.sh"
    echo '#!/bin/bash' > "$fake/scripts/staging-reseed.sh"
    echo '#!/bin/bash' > "$fake/scripts/staging-deployed-sha.sh"
    # staging-reset.sh deliberately absent.
    PATH="$SANDBOX/bin:$PATH" \
        bash "$fake/scripts/admin/setup-staging-reseed-host.sh" >/dev/null 2>"$SANDBOX/stderr"
    RC=$?
    if [ "$RC" -ne 0 ]; then ok; else fail "missing local source must fail, got $RC"; fi
    if grep -q 'staging-reset.sh' "$SANDBOX/stderr"; then ok; else fail "message must name the missing source"; fi
    # must fail BEFORE any ssh run of the installer
    if grep -F 'ssh ' "$CALL_LOG" | grep -q 'sudo'; then fail "must not contact the host when a local source is missing"; else ok; fi
    cleanup_sandbox
}

# ---------------------------------------------------------------------------
main() {
    test_installs_three_scripts_root_0755
    test_sudoers_two_lines_no_setenv
    test_sudoers_principal_default_and_override
    test_visudo_validates_before_install_and_after
    test_fail_not_root
    test_fail_missing_runner_user
    test_fail_missing_deploy_staging
    test_fail_missing_source_script
    test_idempotent_second_run
    test_ssh_mode_requires_staging_host
    test_ssh_mode_bootstraps_remote
    test_ssh_mode_runner_user_override
    test_ssh_mode_quotes_hostile_runner_user
    test_ssh_mode_propagates_remote_rc_and_cleans_up
    test_ssh_mode_missing_local_source_fails_before_ssh

    echo ""
    echo "===================="
    echo "PASS=$PASS FAIL=$FAIL"
    echo "===================="
    [ "$FAIL" -eq 0 ]
}

main "$@"
