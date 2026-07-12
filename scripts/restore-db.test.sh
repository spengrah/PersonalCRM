#!/usr/bin/env bash
# Tests for restore-db.sh — the CRM_USER/CRM_HOME tenant parameterization.
#
# Mirror of backup-db.test.sh: PATH-shadowed stubs, call-log assertions, no Pi /
# podman / root / real account. Covers:
#   - DEFAULT env reproduces the prior prod behavior verbatim (crm tenant).
#   - CRM_USER=staging substitutes the staging tenant (local + ssh modes).
#   - --no-app-start brings Postgres up but leaves the app STOPPED (the rollback
#     contract deploy-artifact.sh relies on).
#   - SHARED resources stay literal: personalcrm-db volume, personalcrm-* services,
#     pg_isready -U crm_user.
#
# Portability: no BSD-only stat -f / date -r / sed -i.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$REPO_ROOT/scripts/restore-db.sh"

CRM_UID=995
STAGING_UID=1995

PASS=0
FAIL=0
fail() { echo "  FAIL: $1" >&2; FAIL=$((FAIL + 1)); }
ok()   { PASS=$((PASS + 1)); }

make_sandbox() {
    SANDBOX="$(mktemp -d)"
    CALL_LOG="$SANDBOX/calls.log"
    : > "$CALL_LOG"
    mkdir -p "$SANDBOX/bin" "$SANDBOX/volume/_data"
    # A real snapshot dir so the `sudo test -e` existence check passes (local mode
    # uses the real `test` binary; ssh mode goes through the ssh stub).
    SNAPSHOT="$SANDBOX/volume/_data.bak-20260611-000000"
    mkdir -p "$SNAPSHOT"

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

    cat > "$SANDBOX/bin/podman" <<EOF
#!/usr/bin/env bash
echo "podman \$*" >> "$CALL_LOG"
case "\$1" in
  volume) echo "$SANDBOX/volume/_data"; exit 0 ;;
  exec)   exit "\${STUB_PG_NOT_READY:-0}" ;;
esac
exit 0
EOF

    cat > "$SANDBOX/bin/systemctl" <<EOF
#!/usr/bin/env bash
echo "systemctl \$*" >> "$CALL_LOG"
exit 0
EOF

    # mv/cp/rm: record-only no-ops so the on-disk volume is never actually moved.
    for tool in mv cp rm; do
        cat > "$SANDBOX/bin/$tool" <<EOF
#!/usr/bin/env bash
echo "$tool \$*" >> "$CALL_LOG"
exit 0
EOF
    done

    cat > "$SANDBOX/bin/ssh" <<EOF
#!/usr/bin/env bash
echo "ssh \$*" >> "$CALL_LOG"
all="\$*"
case "\$all" in
  *"id -u staging"*) echo $STAGING_UID; exit 0 ;;
  *"id -u crm"*)     echo $CRM_UID; exit 0 ;;
  *"volume inspect"*) echo "$SANDBOX/volume/_data"; exit 0 ;;
  *"pg_isready"*) exit "\${STUB_PG_NOT_READY:-0}" ;;
  *) exit 0 ;;
esac
EOF

    cat > "$SANDBOX/bin/sleep" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

    chmod +x "$SANDBOX"/bin/*
}

cleanup_sandbox() { [ -n "${SANDBOX:-}" ] && rm -rf "$SANDBOX"; }

# run_restore <args...> : run restore-db.sh in the sandbox. Sets RC; stdout →
# $SANDBOX/stdout, stderr → $SANDBOX/stderr. Assertions read the call log + stderr.
run_restore() {
    PATH="$SANDBOX/bin:$PATH" PI_HOST=pi.test.invalid bash "$SCRIPT" "$@" \
        >"$SANDBOX/stdout" 2>"$SANDBOX/stderr"
    RC=$?
}

log_has()   { grep -qF -- "$1" "$CALL_LOG"; }
log_lacks() { ! grep -qF -- "$1" "$CALL_LOG"; }

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

assert_shared_invariants() {
    if log_has "personalcrm-db"; then ok; else fail "volume personalcrm-db missing"; fi
    if log_has "personalcrm-database.service"; then ok; else fail "personalcrm-database.service missing"; fi
    if log_has "pg_isready -U crm_user"; then ok; else fail "pg_isready -U crm_user missing"; fi
    if log_lacks "staging-db"; then ok; else fail "tenant name leaked into the volume name"; fi
    if log_lacks "pg_isready -U staging"; then ok; else fail "tenant name leaked into the PG role"; fi
}

# ===========================================================================
# Tests
# ===========================================================================

test_default_local_no_app_start() {
    echo "test: DEFAULT env (local --no-app-start) — crm tenant; DB up, app stopped"
    make_sandbox
    run_restore --local --no-app-start "$SNAPSHOT"
    if [ "$RC" -eq 0 ]; then ok; else fail "default restore should exit 0, got $RC ($(cat "$SANDBOX/stderr"))"; fi
    if log_has "id -u crm"; then ok; else fail "default must resolve id -u crm"; fi
    assert_rootless_env crm /var/lib/personalcrm "$CRM_UID"
    # DB is brought up; the app is NOT started.
    if grep -E '^sudo .*systemctl' "$CALL_LOG" | grep -q 'start personalcrm-database.service'; then ok
    else fail "--no-app-start must still start the database"; fi
    if grep -E '^sudo .*systemctl' "$CALL_LOG" | grep -q 'start personalcrm-backend.service'; then
        fail "--no-app-start must NOT start the app"
    else ok; fi
    assert_shared_invariants
    cleanup_sandbox
}

test_staging_local_no_app_start() {
    echo "test: CRM_USER=staging (local --no-app-start) substitutes the staging tenant"
    make_sandbox
    CRM_USER=staging CRM_HOME=/var/lib/staging run_restore --local --no-app-start "$SNAPSHOT"
    if [ "$RC" -eq 0 ]; then ok; else fail "staging restore should exit 0, got $RC"; fi
    if log_has "id -u staging"; then ok; else fail "staging must resolve id -u staging"; fi
    assert_rootless_env staging /var/lib/staging "$STAGING_UID"
    assert_shared_invariants
    cleanup_sandbox
}

test_full_local_restart_starts_app() {
    echo "test: full local restore (no --no-app-start) restarts the app"
    make_sandbox
    run_restore --local "$SNAPSHOT"
    if [ "$RC" -eq 0 ]; then ok; else fail "full restore should exit 0, got $RC"; fi
    if grep -E '^sudo .*systemctl' "$CALL_LOG" | grep -q 'start personalcrm-backend.service'; then ok
    else fail "full restore must restart the app"; fi
    assert_rootless_env crm /var/lib/personalcrm "$CRM_UID"
    cleanup_sandbox
}

test_default_ssh_substitution() {
    echo "test: DEFAULT env (ssh) resolves id -u crm + sudo -n -u crm remotely"
    make_sandbox
    run_restore --no-app-start "$SNAPSHOT"
    if [ "$RC" -eq 0 ]; then ok; else fail "default ssh restore should exit 0, got $RC ($(cat "$SANDBOX/stderr"))"; fi
    if grep -E '^ssh ' "$CALL_LOG" | grep -q 'id -u crm'; then ok; else fail "ssh mode must resolve id -u crm"; fi
    if grep -E '^ssh ' "$CALL_LOG" | grep -q 'sudo -n -u crm'; then ok; else fail "ssh remote cmd must use sudo -n -u crm"; fi
    assert_shared_invariants
    cleanup_sandbox
}

test_staging_ssh_substitution() {
    echo "test: CRM_USER=staging (ssh) resolves id -u staging + sudo -n -u staging"
    make_sandbox
    CRM_USER=staging CRM_HOME=/var/lib/staging run_restore --no-app-start "$SNAPSHOT"
    if [ "$RC" -eq 0 ]; then ok; else fail "staging ssh restore should exit 0, got $RC"; fi
    if grep -E '^ssh ' "$CALL_LOG" | grep -q 'id -u staging'; then ok; else fail "ssh mode must resolve id -u staging"; fi
    if grep -E '^ssh ' "$CALL_LOG" | grep -q 'sudo -n -u staging'; then ok; else fail "ssh remote cmd must use sudo -n -u staging"; fi
    if grep -E '^ssh ' "$CALL_LOG" | grep -q 'HOME=/var/lib/staging'; then ok; else fail "ssh remote cmd must use staging HOME"; fi
    cleanup_sandbox
}

# ---------------------------------------------------------------------------
main() {
    test_default_local_no_app_start
    test_staging_local_no_app_start
    test_full_local_restart_starts_app
    test_default_ssh_substitution
    test_staging_ssh_substitution

    echo ""
    echo "===================="
    echo "PASS=$PASS FAIL=$FAIL"
    echo "===================="
    [ "$FAIL" -eq 0 ]
}

main "$@"
