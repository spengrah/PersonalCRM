#!/usr/bin/env bash
# Tests for backup-db.sh — the CRM_USER/CRM_HOME tenant parameterization.
#
# These run anywhere (no Pi, no podman, no root, no real crm/staging account).
# PATH is shadowed with stub id/sudo/podman/systemctl/ssh/cp/du/sleep that record
# argv to a per-test call log; assert on the recorded calls + exit code + stdout.
#
# The load-bearing points:
#   - DEFAULT env (no overrides) reproduces the prior prod behavior verbatim:
#     system user `crm`, `id -u crm`, HOME=/var/lib/personalcrm, XDG uid for crm.
#     (prod behavioral-equivalence guard — defaults must not have changed.)
#   - CRM_USER=staging CRM_HOME=/var/lib/staging substitutes the tenant everywhere
#     (both --local and ssh modes), including the recovery hints.
#   - SHARED-across-tenants resources stay LITERAL regardless of CRM_USER: the
#     `personalcrm-db` volume, the `personalcrm-*` services, `pg_isready -U crm_user`.
#   - --local mode emits exactly the BACKUP_PATH=<path> stdout contract.
#
# Portability: no BSD-only stat -f / date -r / sed -i. Stubs validate each form.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$REPO_ROOT/scripts/backup-db.sh"

# Fixed fake uids per tenant (no real account needed).
CRM_UID=995
STAGING_UID=1995

PASS=0
FAIL=0
fail() { echo "  FAIL: $1" >&2; FAIL=$((FAIL + 1)); }
ok()   { PASS=$((PASS + 1)); }

# ---------------------------------------------------------------------------
# Sandbox: a fresh tmp dir per scenario with stub bin/ + a call log.
# ---------------------------------------------------------------------------
make_sandbox() {
    SANDBOX="$(mktemp -d)"
    CALL_LOG="$SANDBOX/calls.log"
    : > "$CALL_LOG"
    mkdir -p "$SANDBOX/bin" "$SANDBOX/volume/_data"

    # --- stub: id (resolve a tenant uid without a real account) ---
    cat > "$SANDBOX/bin/id" <<EOF
#!/usr/bin/env bash
echo "id \$*" >> "$CALL_LOG"
if [ "\$1" = "-u" ]; then
    case "\$2" in
        crm) echo $CRM_UID; exit 0 ;;
        staging) echo $STAGING_UID; exit 0 ;;
    esac
fi
exec /usr/bin/id "\$@"
EOF

    # --- stub: sudo (record full argv, strip -u/-n + leading KEY=val, exec rest) ---
    cat > "$SANDBOX/bin/sudo" <<EOF
#!/usr/bin/env bash
echo "sudo \$*" >> "$CALL_LOG"
# Mirror real sudo: parse options (-u <user>, -n, leading KEY=val env) ONLY until
# the first non-option token (the command); the rest is the command's own argv,
# copied verbatim — so a command flag like \`sed -n\` is never stripped.
args=("\$@")
out=()
i=0
opts=1
while [ \$i -lt \${#args[@]} ]; do
    a="\${args[\$i]}"
    if [ "\$opts" = "1" ]; then
        if [ "\$a" = "-u" ]; then i=\$((i + 2)); continue; fi
        if [ "\$a" = "-n" ]; then i=\$((i + 1)); continue; fi
        if [[ "\$a" == *=* ]]; then i=\$((i + 1)); continue; fi
        opts=0
    fi
    out+=("\$a")
    i=\$((i + 1))
done
exec "\${out[@]}"
EOF

    # --- stub: podman (volume inspect + pg_isready exec) ---
    cat > "$SANDBOX/bin/podman" <<EOF
#!/usr/bin/env bash
echo "podman \$*" >> "$CALL_LOG"
case "\$1" in
  volume) echo "$SANDBOX/volume/_data"; exit 0 ;;
  exec)   exit "\${STUB_PG_NOT_READY:-0}" ;;
esac
exit 0
EOF

    # --- stub: systemctl ---
    cat > "$SANDBOX/bin/systemctl" <<EOF
#!/usr/bin/env bash
echo "systemctl \$*" >> "$CALL_LOG"
exit 0
EOF

    # --- stub: cp / du (root volume copy + size) ---
    cat > "$SANDBOX/bin/cp" <<EOF
#!/usr/bin/env bash
echo "cp \$*" >> "$CALL_LOG"
exit 0
EOF
    cat > "$SANDBOX/bin/du" <<EOF
#!/usr/bin/env bash
echo "du \$*" >> "$CALL_LOG"
echo "1.2M"
exit 0
EOF

    # --- stub: ssh (dispatch on the remote command text) ---
    cat > "$SANDBOX/bin/ssh" <<EOF
#!/usr/bin/env bash
echo "ssh \$*" >> "$CALL_LOG"
all="\$*"
case "\$all" in
  *"id -u staging"*) echo $STAGING_UID; exit 0 ;;
  *"id -u crm"*)     echo $CRM_UID; exit 0 ;;
  *"volume inspect"*) echo "$SANDBOX/volume/_data"; exit 0 ;;
  *"pg_isready"*) exit "\${STUB_PG_NOT_READY:-0}" ;;
  *"du -sh"*) echo "1.2M"; exit 0 ;;
  *) exit 0 ;;
esac
EOF

    # --- stub: sleep (no-op so retry loops run instantly) ---
    cat > "$SANDBOX/bin/sleep" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

    chmod +x "$SANDBOX"/bin/*
}

cleanup_sandbox() { [ -n "${SANDBOX:-}" ] && rm -rf "$SANDBOX"; }

# run_backup <args...> : run backup-db.sh in the sandbox. Sets RC, OUT (stdout),
# and leaves stderr in $SANDBOX/stderr. Tenant overrides come from the caller's
# env (CRM_USER / CRM_HOME), so a bare call exercises the DEFAULT (prod) path.
run_backup() {
    OUT="$(PATH="$SANDBOX/bin:$PATH" PI_HOST=stovepipes bash "$SCRIPT" "$@" 2>"$SANDBOX/stderr")"
    RC=$?
}

log_has()   { grep -qF -- "$1" "$CALL_LOG"; }
log_lacks() { ! grep -qF -- "$1" "$CALL_LOG"; }

# assert_rootless_env <user> <home> <uid>: every sudo-wrapped podman/systemctl
# call carries -u <user>, HOME=<home>, XDG_RUNTIME_DIR=/run/user/<uid>; systemctl
# also carries the matching DBUS bus path.
assert_rootless_env() {
    local user="$1" home="$2" uid="$3" bad=0 line
    while IFS= read -r line; do
        case "$line" in
            "sudo "*podman*|"sudo "*systemctl*)
                [[ "$line" == *"-u $user"* ]] || { bad=1; echo "    no -u $user: $line" >&2; }
                [[ "$line" == *"HOME=$home"* ]] || { bad=1; echo "    no HOME=$home: $line" >&2; }
                [[ "$line" == *"XDG_RUNTIME_DIR=/run/user/$uid"* ]] || { bad=1; echo "    no XDG uid $uid: $line" >&2; }
                ;;
        esac
    done < "$CALL_LOG"
    if [ "$bad" -eq 0 ]; then ok; else fail "some sudo podman/systemctl calls lacked the rootless env for $user"; fi
    if grep -E '^sudo .*systemctl' "$CALL_LOG" | grep -q "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$uid/bus"; then ok
    else fail "systemctl calls missing DBUS bus for uid $uid"; fi
}

# assert_shared_invariants: SHARED resources are literal regardless of tenant.
assert_shared_invariants() {
    if log_has "personalcrm-db"; then ok; else fail "volume personalcrm-db missing"; fi
    if log_has "personalcrm-backend.service"; then ok; else fail "personalcrm-backend.service missing"; fi
    if log_has "personalcrm-database.service"; then ok; else fail "personalcrm-database.service missing"; fi
    if log_has "pg_isready -U crm_user"; then ok; else fail "pg_isready -U crm_user missing"; fi
    # Tenant name must NOT leak into the shared resource names.
    if log_lacks "staging-db"; then ok; else fail "tenant name leaked into the volume name"; fi
    if log_lacks "pg_isready -U staging"; then ok; else fail "tenant name leaked into the PG role"; fi
}

# ===========================================================================
# Tests
# ===========================================================================

test_default_local_rootless_env() {
    echo "test: DEFAULT env (local) uses the crm tenant verbatim (prod equivalence)"
    make_sandbox
    run_backup --local
    if [ "$RC" -eq 0 ]; then ok; else fail "default local backup should exit 0, got $RC"; fi
    if log_has "id -u crm"; then ok; else fail "default must resolve id -u crm"; fi
    assert_rootless_env crm /var/lib/personalcrm "$CRM_UID"
    assert_shared_invariants
    cleanup_sandbox
}

test_staging_local_rootless_env() {
    echo "test: CRM_USER=staging (local) substitutes the staging tenant"
    make_sandbox
    CRM_USER=staging CRM_HOME=/var/lib/staging run_backup --local
    if [ "$RC" -eq 0 ]; then ok; else fail "staging local backup should exit 0, got $RC"; fi
    if log_has "id -u staging"; then ok; else fail "staging must resolve id -u staging"; fi
    assert_rootless_env staging /var/lib/staging "$STAGING_UID"
    assert_shared_invariants
    cleanup_sandbox
}

test_local_backup_path_stdout_contract() {
    echo "test: --local emits exactly one BACKUP_PATH=<path> line on stdout"
    make_sandbox
    run_backup --local --no-restart
    if [ "$RC" -eq 0 ]; then ok; else fail "local --no-restart should exit 0, got $RC"; fi
    local n
    n="$(printf '%s\n' "$OUT" | grep -c .)"
    if [ "$n" -eq 1 ]; then ok; else fail "stdout must be exactly one line, got $n: $OUT"; fi
    if printf '%s' "$OUT" | grep -qE '^BACKUP_PATH=.*/_data\.bak-[0-9]{8}-[0-9]{6}$'; then ok
    else fail "stdout not the BACKUP_PATH contract: $OUT"; fi
    cleanup_sandbox
}

test_no_restart_skips_restart_and_hints_tenant() {
    echo "test: --no-restart leaves services stopped; recovery hint names the tenant"
    make_sandbox
    CRM_USER=staging CRM_HOME=/var/lib/staging run_backup --local --no-restart
    if [ "$RC" -eq 0 ]; then ok; else fail "staging --no-restart should exit 0, got $RC"; fi
    # No restart of the app/DB after the stop.
    if grep -E '^sudo .*systemctl' "$CALL_LOG" | grep -q 'start personalcrm'; then
        fail "--no-restart must NOT start any service"
    else ok; fi
    # The manual-recovery hint (stderr) names the staging tenant + home.
    if grep -q 'sudo -u staging' "$SANDBOX/stderr" && grep -q 'HOME=/var/lib/staging' "$SANDBOX/stderr"; then ok
    else fail "recovery hint must name the staging tenant"; fi
    cleanup_sandbox
}

test_default_ssh_substitution() {
    echo "test: DEFAULT env (ssh) resolves id -u crm + sudo -n -u crm remotely"
    make_sandbox
    run_backup
    if [ "$RC" -eq 0 ]; then ok; else fail "default ssh backup should exit 0, got $RC ($(cat "$SANDBOX/stderr"))"; fi
    if grep -E '^ssh ' "$CALL_LOG" | grep -q 'id -u crm'; then ok; else fail "ssh mode must resolve id -u crm"; fi
    if grep -E '^ssh ' "$CALL_LOG" | grep -q 'sudo -n -u crm'; then ok; else fail "ssh remote cmd must use sudo -n -u crm"; fi
    assert_shared_invariants
    cleanup_sandbox
}

test_staging_ssh_substitution() {
    echo "test: CRM_USER=staging (ssh) resolves id -u staging + sudo -n -u staging"
    make_sandbox
    CRM_USER=staging CRM_HOME=/var/lib/staging run_backup
    if [ "$RC" -eq 0 ]; then ok; else fail "staging ssh backup should exit 0, got $RC"; fi
    if grep -E '^ssh ' "$CALL_LOG" | grep -q 'id -u staging'; then ok; else fail "ssh mode must resolve id -u staging"; fi
    if grep -E '^ssh ' "$CALL_LOG" | grep -q 'sudo -n -u staging'; then ok; else fail "ssh remote cmd must use sudo -n -u staging"; fi
    if grep -E '^ssh ' "$CALL_LOG" | grep -q 'HOME=/var/lib/staging'; then ok; else fail "ssh remote cmd must use staging HOME"; fi
    # SHARED resources unchanged even with the staging tenant.
    if grep -E '^ssh ' "$CALL_LOG" | grep -q 'personalcrm-db'; then ok; else fail "ssh mode must still use the shared volume"; fi
    cleanup_sandbox
}

# ---------------------------------------------------------------------------
main() {
    test_default_local_rootless_env
    test_staging_local_rootless_env
    test_local_backup_path_stdout_contract
    test_no_restart_skips_restart_and_hints_tenant
    test_default_ssh_substitution
    test_staging_ssh_substitution

    echo ""
    echo "===================="
    echo "PASS=$PASS FAIL=$FAIL"
    echo "===================="
    [ "$FAIL" -eq 0 ]
}

main "$@"
