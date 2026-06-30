#!/usr/bin/env bash
# Tests for staging-reset.sh — the rootless-tenant HARD reset + reseed.
#
# PATH-shadowed stubs (id/sudo/podman/systemctl/sed/ssh), fixture env + unit
# files, call-log assertions. No Pi/podman/root/real account. Covers:
#   - Ordering: stop backend -> ephemeral `podman run ... --entrypoint crm-admin
#     ... --reset-and-seed --profile prod-shaped --yes` -> start backend.
#   - The reset runs against the image ref read VERBATIM from the unit's pinned
#     Image= line (:<sha> / @sha256:), NEVER a hardcoded :latest, and NEVER via
#     `podman exec` into the (stopped) running container.
#   - Fail-closed production refuse (production / prod / empty / absent / quoted),
#     BEFORE stopping anything; CRM_ENV=staging proceeds.
#   - Trap-before-stop: a failing stop still triggers the restart (the trap is
#     installed before the stop); a failing reset still restarts the backend.
#   - Rootless env on every tenant op; ssh mode targets STAGING_HOST, --local
#     does not ssh.
#
# Portability: no BSD-only stat -f / date -r / sed -i (only `sed -n 's/.../p'`).

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$REPO_ROOT/scripts/staging-reset.sh"
STAGING_UID=1995
SHA_REF="ghcr.io/spengrah/personalcrm-backend:1111111111111111111111111111111111111111"
DIGEST_REF="ghcr.io/spengrah/personalcrm-backend@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"

PASS=0
FAIL=0
fail() { echo "  FAIL: $1" >&2; FAIL=$((FAIL + 1)); }
ok()   { PASS=$((PASS + 1)); }

# write_env <crm_env_value|__none__> : (re)write the fixture staging .env.
write_env() {
    if [ "$1" = "__none__" ]; then
        printf 'DATABASE_URL=postgres://crm_user:x@crm-postgres:5432/personal_crm\n' > "$SANDBOX/staging.env"
    else
        printf 'CRM_ENV=%s\nDATABASE_URL=postgres://crm_user:x@crm-postgres:5432/personal_crm\n' "$1" > "$SANDBOX/staging.env"
    fi
}

# write_unit <image_ref> : (re)write the fixture backend Quadlet unit.
write_unit() {
    printf '[Container]\nContainerName=crm-backend\nImage=%s\nNetwork=crm.network\n' "$1" > "$SANDBOX/backend.container"
}

make_sandbox() {
    SANDBOX="$(mktemp -d)"
    CALL_LOG="$SANDBOX/calls.log"
    : > "$CALL_LOG"
    mkdir -p "$SANDBOX/bin"
    write_env staging
    write_unit "$SHA_REF"

    cat > "$SANDBOX/bin/id" <<EOF
#!/usr/bin/env bash
echo "id \$*" >> "$CALL_LOG"
if [ "\$1" = "-u" ]; then
    case "\$2" in
        staging) echo $STAGING_UID; exit 0 ;;
        crm) echo 995; exit 0 ;;
    esac
fi
exec /usr/bin/id "\$@"
EOF

    cat > "$SANDBOX/bin/sudo" <<EOF
#!/usr/bin/env bash
echo "sudo \$*" >> "$CALL_LOG"
# Mirror real sudo: parse options (-u <user>, -n, leading KEY=val env) ONLY until
# the first non-option token (the command); everything after is the command's own
# argv, copied verbatim — so a command flag like \`sed -n\` is never stripped.
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
if [ "\$1" = "exec" ]; then
    # podman exec crm-postgres psql ... -tAc "SELECT count(*) FROM oauth_credential"
    if [[ "\$*" == *oauth_credential* ]]; then
        if [ "\${STUB_OAUTH_PSQL_FAIL:-0}" = "1" ]; then exit 1; fi
        echo "\${STUB_OAUTH_COUNT:-0}"
        exit 0
    fi
    exit 0
fi
if [ "\$1" = "run" ]; then
    for a in "\$@"; do
        if [ "\$a" = "--reset-and-seed" ]; then exit "\${STUB_RESET_RC:-0}"; fi
    done
fi
exit 0
EOF

    cat > "$SANDBOX/bin/systemctl" <<EOF
#!/usr/bin/env bash
echo "systemctl \$*" >> "$CALL_LOG"
joined="\$*"
if [ "\${STUB_STOP_FAIL:-0}" = "1" ] && [[ "\$joined" == *stop* ]]; then exit 1; fi
exit 0
EOF

    cat > "$SANDBOX/bin/sed" <<EOF
#!/usr/bin/env bash
echo "sed \$*" >> "$CALL_LOG"
exec /usr/bin/sed "\$@"
EOF

    cat > "$SANDBOX/bin/ssh" <<EOF
#!/usr/bin/env bash
echo "ssh \$*" >> "$CALL_LOG"
all="\$*"
case "\$all" in
  *"id -u staging"*) echo $STAGING_UID; exit 0 ;;
  *"s/^CRM_ENV=//p"*) echo "\${STUB_CRM_ENV:-staging}"; exit 0 ;;
  *"s/^Image=//p"*)   echo "\${STUB_IMAGE_REF:-$SHA_REF}"; exit 0 ;;
  *"test -e"*) exit "\${STUB_ENV_MISSING:-0}" ;;
  *systemctl*) if [ "\${STUB_STOP_FAIL:-0}" = "1" ] && [[ "\$all" == *stop* ]]; then exit 1; fi; exit 0 ;;
  *"podman run"*) if [[ "\$all" == *--reset-and-seed* ]]; then exit "\${STUB_RESET_RC:-0}"; fi; exit 0 ;;
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

# run_local : run staging-reset.sh --local in the sandbox (sets RC; stderr saved).
run_local() {
    PATH="$SANDBOX/bin:$PATH" \
        STAGING_ENV_FILE="$SANDBOX/staging.env" \
        STAGING_BACKEND_UNIT="$SANDBOX/backend.container" \
        bash "$SCRIPT" --local >/dev/null 2>"$SANDBOX/stderr"
    RC=$?
}

# run_ssh : run staging-reset.sh in ssh mode (STAGING_HOST=stovepipes).
run_ssh() {
    PATH="$SANDBOX/bin:$PATH" \
        STAGING_HOST=stovepipes \
        STAGING_ENV_FILE=/srv/personalcrm/.env \
        STAGING_BACKEND_UNIT=/var/lib/staging/.config/containers/systemd/personalcrm-backend.container \
        bash "$SCRIPT" >/dev/null 2>"$SANDBOX/stderr"
    RC=$?
}

# run_local_oauth : --local --require-oauth-empty (the auto path's invocation).
run_local_oauth() {
    PATH="$SANDBOX/bin:$PATH" \
        STAGING_ENV_FILE="$SANDBOX/staging.env" \
        STAGING_BACKEND_UNIT="$SANDBOX/backend.container" \
        bash "$SCRIPT" --local --require-oauth-empty >/dev/null 2>"$SANDBOX/stderr"
    RC=$?
}

# write_env_raw <contents> : write the fixture .env verbatim (for custom URLs).
write_env_raw() { printf '%s' "$1" > "$SANDBOX/staging.env"; }

log_has()   { grep -qF -- "$1" "$CALL_LOG"; }
log_lacks() { ! grep -qF -- "$1" "$CALL_LOG"; }
log_idx()   { grep -nF -- "$1" "$CALL_LOG" | head -1 | cut -d: -f1; }

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

# assert the recorded `podman run` reset line carries all required flags + a ref.
assert_reset_run_line() {
    local want_ref="$1" run
    run="$(grep -F 'podman run' "$CALL_LOG" | grep -F -- '--reset-and-seed' | head -1)"
    if [ -z "$run" ]; then fail "no 'podman run ... --reset-and-seed' recorded"; return; fi
    local needle
    for needle in "--rm" "--network crm" "--env-file" "-e MIGRATIONS_PATH=/migrations" \
                  "--entrypoint /usr/local/bin/crm-admin" "$want_ref" \
                  "--reset-and-seed" "--profile prod-shaped" "--yes"; do
        if [[ "$run" == *"$needle"* ]]; then ok; else fail "reset run line missing '$needle': $run"; fi
    done
    # NEVER the mutable :latest tag.
    if [[ "$run" == *":latest"* ]]; then fail "reset run must NOT use :latest"; else ok; fi
}

# ===========================================================================
# Tests
# ===========================================================================

test_happy_path_ordering() {
    echo "test: stop -> ephemeral reset run -> start (ordering + command + sha ref)"
    make_sandbox
    write_unit "$SHA_REF"
    run_local
    if [ "$RC" -eq 0 ]; then ok; else fail "happy path should exit 0, got $RC ($(cat "$SANDBOX/stderr"))"; fi
    local i_stop i_run i_start
    i_stop="$(log_idx 'stop personalcrm-backend.service')"
    i_run="$(grep -nF 'podman run' "$CALL_LOG" | grep -F -- '--reset-and-seed' | head -1 | cut -d: -f1)"
    i_start="$(log_idx 'start personalcrm-backend.service')"
    if [ -n "$i_stop" ] && [ -n "$i_run" ] && [ "$i_stop" -lt "$i_run" ]; then ok; else fail "stop must precede the reset run (stop=$i_stop run=$i_run)"; fi
    if [ -n "$i_run" ] && [ -n "$i_start" ] && [ "$i_run" -lt "$i_start" ]; then ok; else fail "reset run must precede start (run=$i_run start=$i_start)"; fi
    assert_reset_run_line "$SHA_REF"
    assert_rootless_env staging /var/lib/staging "$STAGING_UID"
    cleanup_sandbox
}

test_image_ref_digest_verbatim() {
    echo "test: reset uses the pinned @sha256 digest ref verbatim, not :latest"
    make_sandbox
    write_unit "$DIGEST_REF"
    run_local
    if [ "$RC" -eq 0 ]; then ok; else fail "digest-ref reset should exit 0, got $RC"; fi
    assert_reset_run_line "$DIGEST_REF"
    cleanup_sandbox
}

test_never_podman_exec() {
    echo "test: NEVER 'podman exec' into the running container for the reset"
    make_sandbox
    run_local
    if grep -F 'podman exec' "$CALL_LOG" | grep -qE 'reset-and-seed|crm-admin'; then
        fail "reset must NOT run via podman exec"
    else ok; fi
    cleanup_sandbox
}

test_staging_proceeds() {
    echo "test: CRM_ENV=staging proceeds (stop + run + start all happen)"
    make_sandbox
    write_env staging
    run_local
    if [ "$RC" -eq 0 ]; then ok; else fail "staging should proceed, got $RC"; fi
    if log_has "stop personalcrm-backend.service"; then ok; else fail "staging must stop the backend"; fi
    if grep -F 'podman run' "$CALL_LOG" | grep -q -- '--reset-and-seed'; then ok; else fail "staging must run the reset"; fi
    cleanup_sandbox
}

test_refuse_matrix() {
    echo "test: fail-closed refuse on production/prod/empty/absent/quoted (before any stop)"
    local val
    for val in production prod "" '"production"' "'prod'"; do
        make_sandbox
        if [ -z "$val" ]; then write_env ""; else write_env "$val"; fi
        run_local
        if [ "$RC" -ne 0 ]; then ok; else fail "CRM_ENV='$val' must refuse (non-zero), got $RC"; fi
        if log_lacks "stop personalcrm-backend.service"; then ok; else fail "CRM_ENV='$val' must refuse BEFORE stopping"; fi
        if grep -F 'podman run' "$CALL_LOG" | grep -q -- '--reset-and-seed'; then fail "CRM_ENV='$val' must NOT run the reset"; else ok; fi
        cleanup_sandbox
    done
    # CRM_ENV line entirely absent -> treated as empty -> refuse.
    make_sandbox
    write_env __none__
    run_local
    if [ "$RC" -ne 0 ]; then ok; else fail "absent CRM_ENV must refuse, got $RC"; fi
    if log_lacks "stop personalcrm-backend.service"; then ok; else fail "absent CRM_ENV must refuse before stop"; fi
    cleanup_sandbox
}

test_missing_env_file_errors() {
    echo "test: missing staging .env -> clear error, exit 1, no stop"
    make_sandbox
    rm -f "$SANDBOX/staging.env"
    run_local
    if [ "$RC" -eq 1 ]; then ok; else fail "missing env file should exit 1, got $RC"; fi
    if grep -q 'env file not found' "$SANDBOX/stderr"; then ok; else fail "expected a 'env file not found' message"; fi
    if log_lacks "stop personalcrm-backend.service"; then ok; else fail "missing env must not stop anything"; fi
    cleanup_sandbox
}

test_trap_before_stop() {
    echo "test: a FAILING stop still triggers the restart (trap installed before stop)"
    make_sandbox
    STUB_STOP_FAIL=1 run_local
    if [ "$RC" -ne 0 ]; then ok; else fail "a failing stop should exit non-zero, got $RC"; fi
    # The EXIT trap (installed before the stop) must still attempt the start.
    if log_has "start personalcrm-backend.service"; then ok; else fail "trap must restart the backend after a failed stop"; fi
    cleanup_sandbox
}

test_reset_failure_restarts() {
    echo "test: a FAILING reset still restarts the backend (partial-state recovery)"
    make_sandbox
    STUB_RESET_RC=1 run_local
    if [ "$RC" -ne 0 ]; then ok; else fail "a failing reset should exit non-zero, got $RC"; fi
    local i_run i_start
    i_run="$(grep -nF 'podman run' "$CALL_LOG" | grep -F -- '--reset-and-seed' | head -1 | cut -d: -f1)"
    i_start="$(log_idx 'start personalcrm-backend.service')"
    if [ -n "$i_run" ] && [ -n "$i_start" ] && [ "$i_run" -lt "$i_start" ]; then ok
    else fail "trap must restart the backend after a failed reset (run=$i_run start=$i_start)"; fi
    cleanup_sandbox
}

test_local_does_not_ssh() {
    echo "test: --local mode never invokes ssh"
    make_sandbox
    run_local
    if log_lacks "ssh "; then ok; else fail "--local must not ssh"; fi
    cleanup_sandbox
}

test_ssh_targets_host() {
    echo "test: ssh mode targets STAGING_HOST and runs the reset there with the pinned ref"
    make_sandbox
    STUB_CRM_ENV=staging STUB_IMAGE_REF="$SHA_REF" run_ssh
    if [ "$RC" -eq 0 ]; then ok; else fail "ssh happy path should exit 0, got $RC ($(cat "$SANDBOX/stderr"))"; fi
    if grep -E '^ssh ' "$CALL_LOG" | grep -q 'stovepipes'; then ok; else fail "ssh mode must target STAGING_HOST"; fi
    if grep -E '^ssh ' "$CALL_LOG" | grep -F -- '--reset-and-seed' | grep -q "$SHA_REF"; then ok
    else fail "ssh reset must run the pinned ref on the host"; fi
    if grep -E '^ssh ' "$CALL_LOG" | grep -q 'sudo -n -u staging'; then ok; else fail "ssh remote ops must use sudo -n -u staging"; fi
    cleanup_sandbox
}

test_ssh_refuses_production() {
    echo "test: ssh mode also refuses a production CRM_ENV before stopping"
    make_sandbox
    STUB_CRM_ENV=production run_ssh
    if [ "$RC" -ne 0 ]; then ok; else fail "ssh production must refuse, got $RC"; fi
    if grep -E '^ssh ' "$CALL_LOG" | grep -F -- '--reset-and-seed' | grep -q .; then
        fail "ssh production must NOT run the reset"
    else ok; fi
    cleanup_sandbox
}

# --- OAuth guard (--require-oauth-empty), the auto path's destructive-skip gate ---

# psql_count_line : the recorded `podman exec ... psql ... oauth_credential` line.
psql_count_line() { grep -F 'podman exec' "$CALL_LOG" | grep -F 'oauth_credential' | head -1; }

test_oauth_empty_proceeds() {
    echo "test: --require-oauth-empty + count 0 -> proceeds (stop -> reseed -> start)"
    make_sandbox
    STUB_OAUTH_COUNT=0 run_local_oauth
    if [ "$RC" -eq 0 ]; then ok; else fail "oauth-empty must proceed (exit 0), got $RC ($(cat "$SANDBOX/stderr"))"; fi
    if log_has "stop personalcrm-backend.service"; then ok; else fail "oauth-empty must stop the backend"; fi
    if grep -F 'podman run' "$CALL_LOG" | grep -q -- '--reset-and-seed'; then ok; else fail "oauth-empty must run the reset"; fi
    # The count targets the DATABASE_URL user/dbname over the in-container local
    # socket: no PGPASSWORD, no -h/TCP (trust auth).
    local line; line="$(psql_count_line)"
    if [ -n "$line" ]; then ok; else fail "no oauth_credential count recorded"; fi
    if [[ "$line" == *"-U crm_user"* ]]; then ok; else fail "count must use DATABASE_URL user (-U crm_user): $line"; fi
    if [[ "$line" == *"-d personal_crm"* ]]; then ok; else fail "count must use DATABASE_URL dbname (-d personal_crm): $line"; fi
    if [[ "$line" == *"PGPASSWORD"* ]]; then fail "count must NOT pass PGPASSWORD: $line"; else ok; fi
    if [[ "$line" == *" -h "* ]]; then fail "count must NOT use -h/TCP: $line"; else ok; fi
    cleanup_sandbox
}

test_oauth_present_skips() {
    echo "test: --require-oauth-empty + count >0 -> clean skip (no stop, no reseed, marker)"
    make_sandbox
    STUB_OAUTH_COUNT=2 run_local_oauth
    if [ "$RC" -eq 0 ]; then ok; else fail "oauth-present must skip cleanly (exit 0), got $RC"; fi
    if log_lacks "stop personalcrm-backend.service"; then ok; else fail "oauth-present must NOT stop the backend"; fi
    if grep -F 'podman run' "$CALL_LOG" | grep -q -- '--reset-and-seed'; then fail "oauth-present must NOT run the reset"; else ok; fi
    if grep -qF 'skipping auto-reseed' "$SANDBOX/stderr"; then ok; else fail "oauth-present must log the stable 'skipping auto-reseed' marker"; fi
    cleanup_sandbox
}

test_oauth_unverifiable_fails() {
    echo "test: --require-oauth-empty + unverifiable count -> fail-closed (exit !=0, no stop/reseed)"
    # (a) psql connection fails entirely.
    make_sandbox
    STUB_OAUTH_PSQL_FAIL=1 run_local_oauth
    if [ "$RC" -ne 0 ]; then ok; else fail "psql failure must fail-closed (exit !=0), got $RC"; fi
    if log_lacks "stop personalcrm-backend.service"; then ok; else fail "unverifiable count must NOT stop the backend"; fi
    if grep -F 'podman run' "$CALL_LOG" | grep -q -- '--reset-and-seed'; then fail "unverifiable count must NOT run the reset"; else ok; fi
    cleanup_sandbox
    # (b) non-numeric count output.
    make_sandbox
    STUB_OAUTH_COUNT=boom run_local_oauth
    if [ "$RC" -ne 0 ]; then ok; else fail "non-numeric count must fail-closed (exit !=0), got $RC"; fi
    if log_lacks "stop personalcrm-backend.service"; then ok; else fail "non-numeric count must NOT stop the backend"; fi
    if grep -F 'podman run' "$CALL_LOG" | grep -q -- '--reset-and-seed'; then fail "non-numeric count must NOT run the reset"; else ok; fi
    cleanup_sandbox
}

test_oauth_missing_database_url_fails() {
    echo "test: --require-oauth-empty + no DATABASE_URL -> fail-closed (exit !=0, no stop/reseed)"
    make_sandbox
    write_env_raw $'CRM_ENV=staging\n'   # staging passes prod-refuse, but no DATABASE_URL
    STUB_OAUTH_COUNT=0 run_local_oauth
    if [ "$RC" -ne 0 ]; then ok; else fail "missing DATABASE_URL must fail-closed, got $RC"; fi
    if log_lacks "stop personalcrm-backend.service"; then ok; else fail "missing DATABASE_URL must NOT stop the backend"; fi
    if grep -F 'podman run' "$CALL_LOG" | grep -q -- '--reset-and-seed'; then fail "missing DATABASE_URL must NOT run the reset"; else ok; fi
    cleanup_sandbox
}

test_oauth_dbname_authoritative() {
    echo "test: DATABASE_URL dbname/user are authoritative even when POSTGRES_* disagree"
    make_sandbox
    # POSTGRES_DB/POSTGRES_USER deliberately DISAGREE with DATABASE_URL; the count
    # MUST use the DATABASE_URL values (else it could count an empty wrong DB and
    # wipe real oauth rows). regression guard for the round-2 [P1] finding.
    write_env_raw $'CRM_ENV=staging\nDATABASE_URL=postgres://crm_user:x@crm-postgres:5432/personal_crm\nPOSTGRES_DB=crm_staging\nPOSTGRES_USER=other\n'
    STUB_OAUTH_COUNT=0 run_local_oauth
    if [ "$RC" -eq 0 ]; then ok; else fail "should proceed with count 0, got $RC"; fi
    local line; line="$(psql_count_line)"
    if [[ "$line" == *"-d personal_crm"* ]]; then ok; else fail "count must use DATABASE_URL dbname personal_crm: $line"; fi
    if [[ "$line" == *"crm_staging"* ]]; then fail "count must NOT use POSTGRES_DB crm_staging: $line"; else ok; fi
    if [[ "$line" == *"-U crm_user"* ]]; then ok; else fail "count must use DATABASE_URL user crm_user: $line"; fi
    if [[ "$line" == *"-U other"* ]]; then fail "count must NOT use POSTGRES_USER other: $line"; else ok; fi
    cleanup_sandbox
}

test_oauth_foreign_host_fails() {
    echo "test: DATABASE_URL host not the local target -> fail-closed BEFORE counting"
    make_sandbox
    write_env_raw $'CRM_ENV=staging\nDATABASE_URL=postgres://crm_user:x@db.example.com:5432/personal_crm\n'
    STUB_OAUTH_COUNT=0 run_local_oauth
    if [ "$RC" -ne 0 ]; then ok; else fail "foreign host must fail-closed, got $RC"; fi
    # Refused BEFORE counting: no psql count attempted, no stop, no reseed.
    if grep -F 'podman exec' "$CALL_LOG" | grep -q 'oauth_credential'; then fail "foreign host must NOT attempt the count"; else ok; fi
    if log_lacks "stop personalcrm-backend.service"; then ok; else fail "foreign host must NOT stop the backend"; fi
    if grep -F 'podman run' "$CALL_LOG" | grep -q -- '--reset-and-seed'; then fail "foreign host must NOT run the reset"; else ok; fi
    cleanup_sandbox
}

test_oauth_flag_is_the_gate() {
    echo "test: WITHOUT --require-oauth-empty, reseed runs regardless of oauth (no count)"
    make_sandbox
    STUB_OAUTH_COUNT=5 run_local   # default invocation, NO flag
    if [ "$RC" -eq 0 ]; then ok; else fail "no-flag default must reseed (exit 0), got $RC"; fi
    if log_has "stop personalcrm-backend.service"; then ok; else fail "no-flag default must stop the backend"; fi
    if grep -F 'podman run' "$CALL_LOG" | grep -q -- '--reset-and-seed'; then ok; else fail "no-flag default must run the reset"; fi
    if grep -F 'podman exec' "$CALL_LOG" | grep -q 'oauth_credential'; then fail "no-flag default must NOT count oauth_credential (flag is the gate)"; else ok; fi
    cleanup_sandbox
}

# ---------------------------------------------------------------------------
main() {
    test_happy_path_ordering
    test_image_ref_digest_verbatim
    test_never_podman_exec
    test_staging_proceeds
    test_refuse_matrix
    test_missing_env_file_errors
    test_trap_before_stop
    test_reset_failure_restarts
    test_local_does_not_ssh
    test_ssh_targets_host
    test_ssh_refuses_production
    test_oauth_empty_proceeds
    test_oauth_present_skips
    test_oauth_unverifiable_fails
    test_oauth_missing_database_url_fails
    test_oauth_dbname_authoritative
    test_oauth_foreign_host_fails
    test_oauth_flag_is_the_gate

    echo ""
    echo "===================="
    echo "PASS=$PASS FAIL=$FAIL"
    echo "===================="
    [ "$FAIL" -eq 0 ]
}

main "$@"
