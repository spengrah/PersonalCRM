#!/usr/bin/env bash
# Tests for deploy-artifact.sh (and the restore/backup interplay it drives).
#
# These run anywhere (no Pi, no podman, no root). PATH is shadowed with stub
# `sudo`/`id`/`podman`/`systemctl`/`curl` and stub backup/restore scripts that
# record their argv to a per-test call log. Each test drives a scenario via env
# vars (migrate-check exit code, inspect digest, health outcome, failure
# injection points) and asserts on the recorded calls + script exit code.
#
# Asserts the load-bearing correctness points from the plan §4:
#   - SHA validation (40-hex only).
#   - rollback-ref extraction from :<sha> AND :latest (digest resolution).
#   - the generated migrate command line (--entrypoint, --network, --env-file,
#     -e MIGRATIONS_PATH, NEW :sha, trailing --migrate-check/--migrate; never
#     `podman exec`).
#   - rootless `sudo -u crm` env on EVERY podman/systemctl call.
#   - exit-code branching (0 / 2 / 1 / other) and which branches touch the DB.
#   - ROLLBACK-WITH-RESTORE ordering: restore FIRST (unconditional), then re-pin.
#   - post-migrate swap-failure -> ROLLBACK-WITH-RESTORE (not the bare trap).
#   - re-pin failure -> "ROLLBACK FAILED" + app left STOPPED + snapshot retained.
#   - ntfy outcomes + degrade-open when the ntfy env file is absent.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$REPO_ROOT/scripts/deploy-artifact.sh"
TESTDATA="$REPO_ROOT/scripts/testdata"
VALID_SHA="abcdef0123456789abcdef0123456789abcdef01"
NEW_SHA="$VALID_SHA"

PASS=0
FAIL=0
fail() { echo "  FAIL: $1" >&2; FAIL=$((FAIL + 1)); }
ok()   { PASS=$((PASS + 1)); }

# ---------------------------------------------------------------------------
# Test sandbox: a fresh tmp dir per scenario with stub bin/, fixture units, and
# a call log. Returns nothing; sets globals SANDBOX, CALL_LOG, BACKEND_UNIT,
# FRONTEND_UNIT for the caller.
# ---------------------------------------------------------------------------
make_sandbox() {
    SANDBOX="$(mktemp -d)"
    CALL_LOG="$SANDBOX/calls.log"
    : > "$CALL_LOG"
    mkdir -p "$SANDBOX/bin" "$SANDBOX/units"
    cp "$TESTDATA/backend-${1:-latest}.container" "$SANDBOX/units/backend.container"
    cp "$TESTDATA/frontend-${2:-latest}.container" "$SANDBOX/units/frontend.container"
    BACKEND_UNIT="$SANDBOX/units/backend.container"
    FRONTEND_UNIT="$SANDBOX/units/frontend.container"

    # --- stub: id (resolve crm uid without a real account) ---
    cat > "$SANDBOX/bin/id" <<'EOF'
#!/usr/bin/env bash
# id -u crm  -> a fixed fake uid; everything else -> real id.
if [ "$1" = "-u" ] && [ "$2" = "crm" ]; then echo 4242; exit 0; fi
exec /usr/bin/id "$@"
EOF

    # --- stub: sudo (record the rootless prefix, then exec the inner cmd) ---
    # We record the FULL `sudo ...` argv so tests can assert `-u crm` + env, then
    # strip `-u <user>` and any leading KEY=val env assignments and exec the rest
    # (which resolves to our other PATH stubs: podman/systemctl/sed/etc.).
    cat > "$SANDBOX/bin/sudo" <<EOF
#!/usr/bin/env bash
echo "sudo \$*" >> "$CALL_LOG"
shift_user=0
args=("\$@")
out=()
i=0
while [ \$i -lt \${#args[@]} ]; do
    a="\${args[\$i]}"
    if [ "\$a" = "-u" ]; then i=\$((i + 2)); continue; fi
    if [ "\$a" = "-n" ]; then i=\$((i + 1)); continue; fi
    if [[ "\$a" == *=* ]] && [ \${#out[@]} -eq 0 ]; then i=\$((i + 1)); continue; fi
    out+=("\$a")
    i=\$((i + 1))
done
exec "\${out[@]}"
EOF

    # --- stub: podman ---
    cat > "$SANDBOX/bin/podman" <<EOF
#!/usr/bin/env bash
echo "podman \$*" >> "$CALL_LOG"
case "\$1" in
  inspect)
    # podman inspect <container> --format '{{index .RepoDigests 0}}'
    echo "\${STUB_INSPECT_DIGEST:-ghcr.io/spengrah/personalcrm-backend@sha256:c91da029deadbeefc91da029deadbeefc91da029deadbeefc91da029deadbeef}"
    exit 0 ;;
  pull)
    exit "\${STUB_PULL_RC:-0}" ;;
  volume)
    echo "$SANDBOX/volume/_data"
    exit 0 ;;
  exec)
    # pg_isready
    exit 0 ;;
  run)
    # crm-admin via --entrypoint. Last arg is the subcommand.
    sub="\${@: -1}"
    if [ "\$sub" = "--migrate-check" ]; then exit "\${STUB_MIGRATE_CHECK_RC:-0}"; fi
    if [ "\$sub" = "--migrate" ]; then exit "\${STUB_MIGRATE_RC:-0}"; fi
    exit 0 ;;
esac
exit 0
EOF

    # --- stub: systemctl ---
    # Failure injection:
    #   STUB_SYSTEMCTL_FAIL=<verb>  fail when this verb appears (e.g. daemon-reload).
    #   STUB_APP_START_FAIL=1       fail ONLY the app start (backend/frontend),
    #                               never the database start. Used to simulate a
    #                               post-migrate swap failure without breaking the
    #                               pre-migrate DB start.
    cat > "$SANDBOX/bin/systemctl" <<EOF
#!/usr/bin/env bash
echo "systemctl \$*" >> "$CALL_LOG"
joined="\$*"
if [ -n "\${STUB_SYSTEMCTL_FAIL:-}" ]; then
  for a in "\$@"; do
    if [ "\$a" = "\$STUB_SYSTEMCTL_FAIL" ]; then exit 1; fi
  done
fi
if [ "\${STUB_APP_START_FAIL:-0}" = "1" ] \
   && [[ "\$joined" == *"start"* ]] \
   && [[ "\$joined" == *"personalcrm-backend.service"* ]]; then
  exit 1
fi
exit 0
EOF

    # --- stub: curl (health gate + ntfy) ---
    cat > "$SANDBOX/bin/curl" <<EOF
#!/usr/bin/env bash
echo "curl \$*" >> "$CALL_LOG"
url="\${@: -1}"
case "\$url" in
  *contacts*) echo -n "\${STUB_CADDY_CODE:-200}"; exit 0 ;;
  *8080/health*) [ "\${STUB_HEALTH_OK:-1}" = "1" ] && { echo '{"database":"healthy"}'; exit 0; }; exit 1 ;;
  *3001*) exit "\${STUB_FRONTEND_RC:-0}" ;;
  *) exit 0 ;;  # ntfy posts
esac
EOF

    # --- stub: sed (record + real edit so pin_image assertions work) ---
    # We want the REAL sed behavior (in-place Image= rewrite) plus a call record.
    # The script targets GNU sed (`-i` takes no arg, as on the Pi). On a BSD sed
    # host (macOS) `-i` needs an empty-string arg, so translate the first bare
    # `-i` to `-i ''` when the real sed is BSD. Prefer gsed if present.
    cat > "$SANDBOX/bin/sed" <<EOF
#!/usr/bin/env bash
echo "sed \$*" >> "$CALL_LOG"
real="\$(command -v gsed || echo /usr/bin/sed)"
if [ "\$real" != "\$(command -v gsed 2>/dev/null)" ] && /usr/bin/sed --version >/dev/null 2>&1; then
    real=/usr/bin/sed  # GNU
fi
if "\$real" --version >/dev/null 2>&1; then
    exec "\$real" "\$@"          # GNU: -i takes no arg
else
    # BSD sed: insert '' after a bare -i.
    args=()
    for a in "\$@"; do
        args+=("\$a")
        if [ "\$a" = "-i" ]; then args+=(""); fi
    done
    exec /usr/bin/sed "\${args[@]}"
fi
EOF

    # --- stub: backup-db.sh ---
    cat > "$SANDBOX/bin/backup-db.sh" <<EOF
#!/usr/bin/env bash
echo "backup-db.sh \$*" >> "$CALL_LOG"
if [ "\${STUB_BACKUP_RC:-0}" != "0" ]; then exit "\${STUB_BACKUP_RC}"; fi
echo "BACKUP_PATH=$SANDBOX/volume/_data.bak-20260611-000000"
exit 0
EOF

    # --- stub: restore-db.sh ---
    cat > "$SANDBOX/bin/restore-db.sh" <<EOF
#!/usr/bin/env bash
echo "restore-db.sh \$*" >> "$CALL_LOG"
exit "\${STUB_RESTORE_RC:-0}"
EOF

    chmod +x "$SANDBOX"/bin/*
    mkdir -p "$SANDBOX/volume"
    : > "$SANDBOX/volume/_data" 2>/dev/null || mkdir -p "$SANDBOX/volume/_data"
}

cleanup_sandbox() { [ -n "${SANDBOX:-}" ] && rm -rf "$SANDBOX"; }

# run_deploy <sha> : run deploy-artifact.sh in the sandbox; sets RC + OUT.
run_deploy() {
    OUT="$(
        PATH="$SANDBOX/bin:$PATH" \
        CRM_USER=crm \
        CRM_HOME="$SANDBOX/home" \
        BACKEND_UNIT="$BACKEND_UNIT" \
        FRONTEND_UNIT="$FRONTEND_UNIT" \
        DEPLOY_ENV_FILE="$SANDBOX/env" \
        BACKUP_SCRIPT="$SANDBOX/bin/backup-db.sh" \
        RESTORE_SCRIPT="$SANDBOX/bin/restore-db.sh" \
        NTFY_ENV_FILE="${NTFY_ENV_FILE_OVERRIDE:-$SANDBOX/missing-ntfy.env}" \
        HEALTH_RETRIES=2 \
        bash "$SCRIPT" "$1" 2>&1
    )"
    RC=$?
}

# assert helpers operating on $CALL_LOG / $OUT.
log_has()    { grep -qF -- "$1" "$CALL_LOG"; }
log_lacks()  { ! grep -qF -- "$1" "$CALL_LOG"; }
# line index (1-based) of the first call line matching a fixed string.
log_idx()    { grep -nF -- "$1" "$CALL_LOG" | head -1 | cut -d: -f1; }

# ===========================================================================
# Tests
# ===========================================================================

test_sha_validation() {
    echo "test: SHA validation"
    for bad in "" "xyz" "abc123" "ABCDEF0123456789ABCDEF0123456789ABCDEF01" \
               "abcdef0123456789abcdef0123456789abcdef0" \
               "abcdef0123456789abcdef0123456789abcdef012"; do
        make_sandbox
        run_deploy "$bad"
        if [ "$RC" -ne 2 ]; then fail "bad SHA '$bad' should exit 2, got $RC"; else ok; fi
        # No DB / image op may run on a bad arg.
        if log_has "podman pull"; then fail "bad SHA '$bad' pulled an image"; else ok; fi
        cleanup_sandbox
    done

    make_sandbox
    STUB_MIGRATE_CHECK_RC=0 run_deploy "$VALID_SHA"
    if [ "$RC" -ne 0 ]; then fail "valid SHA up-to-date should exit 0, got $RC ($OUT)"; else ok; fi
    cleanup_sandbox
}

test_rollback_ref_sha_fixture() {
    echo "test: rollback ref from :<sha> fixture (deterministic, no inspect)"
    make_sandbox sha sha
    STUB_MIGRATE_CHECK_RC=0 STUB_HEALTH_OK=0 run_deploy "$VALID_SHA"
    # Health fails -> up-to-date rollback re-pins the :<sha> anchors (no inspect).
    if log_has "podman inspect"; then fail ":<sha> fixture must NOT call podman inspect"; else ok; fi
    if grep -q 'Image=ghcr.io/spengrah/personalcrm-backend:1111111111111111111111111111111111111111' "$BACKEND_UNIT"; then ok
    else fail "backend unit should be re-pinned to the :<sha> rollback anchor"; fi
    cleanup_sandbox
}

test_rollback_ref_latest_digest() {
    echo "test: rollback ref from :latest fixture (digest resolution)"
    make_sandbox latest latest
    STUB_MIGRATE_CHECK_RC=0 STUB_HEALTH_OK=0 \
      STUB_INSPECT_DIGEST="ghcr.io/spengrah/personalcrm-backend@sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef" \
      run_deploy "$VALID_SHA"
    # :latest must drive `podman inspect ... RepoDigests`.
    if log_has "podman inspect crm-backend"; then ok; else fail ":latest must resolve via podman inspect crm-backend"; fi
    if log_has "podman inspect crm-frontend"; then ok; else fail ":latest must resolve via podman inspect crm-frontend"; fi
    # Rollback re-pin should write the @sha256 digest ref.
    if grep -q 'Image=ghcr.io/spengrah/personalcrm-backend@sha256:deadbeef' "$BACKEND_UNIT"; then ok
    else fail "backend unit should be re-pinned to the resolved @sha256 digest"; fi
    cleanup_sandbox
}

test_migrate_command_line() {
    echo "test: generated migrate-check / migrate command line"
    make_sandbox
    STUB_MIGRATE_CHECK_RC=2 run_deploy "$VALID_SHA"
    # The migrate-check run line.
    local mc
    mc="$(grep -F 'podman run' "$CALL_LOG" | grep -F -- '--migrate-check' | head -1)"
    [ -n "$mc" ] || { fail "no 'podman run ... --migrate-check' recorded"; }
    for needle in "--entrypoint /usr/local/bin/crm-admin" "--network crm" \
                  "--env-file" "-e MIGRATIONS_PATH=/migrations" \
                  "ghcr.io/spengrah/personalcrm-backend:$NEW_SHA" "--migrate-check"; do
        if [[ "$mc" == *"$needle"* ]]; then ok; else fail "migrate-check line missing '$needle': $mc"; fi
    done
    # NEVER `podman exec` for crm-admin.
    if grep -F 'podman exec' "$CALL_LOG" | grep -q 'crm-admin'; then fail "crm-admin must NOT run via podman exec"; else ok; fi
    # The --migrate (apply) line on the pending path.
    local mg
    mg="$(grep -F 'podman run' "$CALL_LOG" | grep -F -- '--migrate' | grep -vF -- '--migrate-check' | head -1)"
    [ -n "$mg" ] || fail "no 'podman run ... --migrate' recorded on pending path"
    if [[ "$mg" == *"--entrypoint /usr/local/bin/crm-admin"* ]]; then ok; else fail "--migrate line missing --entrypoint: $mg"; fi
    cleanup_sandbox
}

test_rootless_env_on_every_call() {
    echo "test: rootless sudo -u crm env on every podman/systemctl call"
    make_sandbox
    STUB_MIGRATE_CHECK_RC=0 run_deploy "$VALID_SHA"
    # Every recorded sudo-wrapped podman/systemctl line must carry the rootless
    # env (-u crm, HOME, XDG).
    local bad=0 line
    while IFS= read -r line; do
        case "$line" in
            "sudo "*podman*|"sudo "*systemctl*)
                [[ "$line" == *"-u crm"* ]] || { bad=1; echo "    no -u crm: $line" >&2; }
                [[ "$line" == *"HOME=$SANDBOX/home"* ]] || { bad=1; echo "    no HOME: $line" >&2; }
                [[ "$line" == *"XDG_RUNTIME_DIR=/run/user/4242"* ]] || { bad=1; echo "    no XDG: $line" >&2; }
                ;;
        esac
    done < "$CALL_LOG"
    if [ "$bad" -eq 0 ]; then ok; else fail "some sudo podman/systemctl calls lacked the rootless env"; fi
    # systemctl must also carry DBUS for the --user bus.
    if grep -E '^sudo .*systemctl' "$CALL_LOG" | grep -q 'DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/4242/bus'; then ok
    else fail "systemctl calls missing DBUS_SESSION_BUS_ADDRESS"; fi

    # Stronger: NO bare podman/systemctl may run outside a sudo wrapper. The sudo
    # stub records `sudo ...X...` then execs the X stub which records `X ...`, so
    # each genuine call yields exactly one of each. If any X ran without sudo, the
    # bare count would exceed the sudo-wrapped count.
    local n_podman n_sudo_podman n_systemctl n_sudo_systemctl
    n_podman=$(grep -cE '^podman ' "$CALL_LOG" || true)
    n_sudo_podman=$(grep -cE '^sudo .* podman ' "$CALL_LOG" || true)
    n_systemctl=$(grep -cE '^systemctl ' "$CALL_LOG" || true)
    n_sudo_systemctl=$(grep -cE '^sudo .* systemctl ' "$CALL_LOG" || true)
    if [ "$n_podman" -ge 1 ] && [ "$n_podman" -eq "$n_sudo_podman" ]; then ok
    else fail "bare podman calls ($n_podman) != sudo-wrapped ($n_sudo_podman)"; fi
    if [ "$n_systemctl" -ge 1 ] && [ "$n_systemctl" -eq "$n_sudo_systemctl" ]; then ok
    else fail "bare systemctl calls ($n_systemctl) != sudo-wrapped ($n_sudo_systemctl)"; fi
    cleanup_sandbox
}

test_exit0_uptodate_no_db() {
    echo "test: exit 0 (up-to-date) touches no DB, swaps image, restarts app"
    make_sandbox
    STUB_MIGRATE_CHECK_RC=0 run_deploy "$VALID_SHA"
    if [ "$RC" -eq 0 ]; then ok; else fail "up-to-date should exit 0, got $RC ($OUT)"; fi
    if log_lacks "backup-db.sh"; then ok; else fail "up-to-date must NOT snapshot the DB"; fi
    if log_lacks "podman run"; then fail "expected a migrate-check podman run"; else ok; fi
    if grep -F 'podman run' "$CALL_LOG" | grep -q -- '--migrate$'; then fail "up-to-date must NOT --migrate"; else ok; fi
    if log_lacks "stop personalcrm-database.service"; then ok; else fail "up-to-date must NOT stop the DB"; fi
    if grep -q "Image=ghcr.io/spengrah/personalcrm-backend:$NEW_SHA" "$BACKEND_UNIT"; then ok; else fail "image not swapped to :sha"; fi
    if log_has "restart personalcrm-backend.service"; then ok; else fail "expected app restart"; fi
    cleanup_sandbox
}

test_exit1_abort_no_touch() {
    echo "test: exit 1 (migrate-check error) aborts before touching anything"
    make_sandbox
    STUB_MIGRATE_CHECK_RC=1 run_deploy "$VALID_SHA"
    if [ "$RC" -eq 1 ]; then ok; else fail "check-error should exit 1, got $RC"; fi
    if log_lacks "backup-db.sh"; then ok; else fail "error path must NOT snapshot"; fi
    if grep -F 'podman run' "$CALL_LOG" | grep -q -- '--migrate$'; then fail "error path must NOT migrate"; else ok; fi
    if log_lacks "stop personalcrm"; then ok; else fail "error path must NOT stop services"; fi
    # Image= untouched.
    if grep -q 'Image=ghcr.io/spengrah/personalcrm-backend:latest' "$BACKEND_UNIT"; then ok; else fail "error path must NOT rewrite Image="; fi
    cleanup_sandbox
}

test_exit_other_abort() {
    echo "test: exit 7 (unexpected) aborts before touching anything"
    make_sandbox
    STUB_MIGRATE_CHECK_RC=7 run_deploy "$VALID_SHA"
    if [ "$RC" -eq 1 ]; then ok; else fail "unexpected check rc should abort exit 1, got $RC"; fi
    if log_lacks "backup-db.sh"; then ok; else fail "unexpected rc must NOT snapshot"; fi
    cleanup_sandbox
}

test_pending_happy_path() {
    echo "test: exit 2 (pending) snapshot -> migrate -> swap -> health OK"
    make_sandbox
    STUB_MIGRATE_CHECK_RC=2 run_deploy "$VALID_SHA"
    if [ "$RC" -eq 0 ]; then ok; else fail "pending happy path should exit 0, got $RC ($OUT)"; fi
    # Ordering: stop app -> backup -> start DB -> migrate -> swap -> start app.
    local i_stop i_backup i_startdb i_migrate
    i_stop="$(log_idx 'stop personalcrm-backend.service personalcrm-frontend.service')"
    i_backup="$(log_idx 'backup-db.sh --local --no-restart')"
    i_startdb="$(log_idx 'start personalcrm-database.service')"
    i_migrate="$(grep -nF 'podman run' "$CALL_LOG" | grep -F -- '--migrate' | grep -vF -- '--migrate-check' | head -1 | cut -d: -f1)"
    if [ -n "$i_stop" ] && [ -n "$i_backup" ] && [ "$i_stop" -lt "$i_backup" ]; then ok; else fail "app stop must precede backup"; fi
    if [ -n "$i_backup" ] && [ -n "$i_startdb" ] && [ "$i_backup" -lt "$i_startdb" ]; then ok; else fail "backup must precede DB start"; fi
    if [ -n "$i_startdb" ] && [ -n "$i_migrate" ] && [ "$i_startdb" -lt "$i_migrate" ]; then ok; else fail "DB start must precede migrate"; fi
    if grep -q "Image=ghcr.io/spengrah/personalcrm-backend:$NEW_SHA" "$BACKEND_UNIT"; then ok; else fail "image not swapped after migrate"; fi
    cleanup_sandbox
}

test_backup_failure_aborts_with_ntfy() {
    echo "test: snapshot/backup non-zero exit -> abort + ntfy (no migrate)"
    make_sandbox
    printf 'NTFY_URL=https://ntfy.example\nNTFY_TOPIC=tok\n' > "$SANDBOX/ntfy.env"
    NTFY_ENV_FILE_OVERRIDE="$SANDBOX/ntfy.env" STUB_MIGRATE_CHECK_RC=2 STUB_BACKUP_RC=1 run_deploy "$VALID_SHA"
    if [ "$RC" -eq 1 ]; then ok; else fail "backup failure should exit 1, got $RC"; fi
    # Must NOT have migrated (backup failed first).
    if grep -F 'podman run' "$CALL_LOG" | grep -q -- '--migrate$'; then fail "must NOT migrate after backup failure"; else ok; fi
    # Must have sent a 'Deploy aborted' ntfy (not silently restart via the trap).
    if grep -F 'curl' "$CALL_LOG" | grep -q 'Title: Deploy aborted'; then ok; else fail "backup failure must ntfy 'Deploy aborted'"; fi
    cleanup_sandbox
}

test_restore_pg_not_ready_is_rollback_failed() {
    echo "test: restore pg_isready never ready -> restore FAILS -> ROLLBACK FAILED"
    make_sandbox
    # restore-db.sh returns non-zero (simulating pg never ready) during rollback.
    printf 'NTFY_URL=https://ntfy.example\nNTFY_TOPIC=tok\n' > "$SANDBOX/ntfy.env"
    NTFY_ENV_FILE_OVERRIDE="$SANDBOX/ntfy.env" \
      STUB_MIGRATE_CHECK_RC=2 STUB_MIGRATE_RC=0 STUB_HEALTH_OK=0 STUB_RESTORE_RC=1 run_deploy "$VALID_SHA"
    if [ "$RC" -eq 1 ]; then ok; else fail "restore failure should exit 1, got $RC"; fi
    if grep -F 'curl' "$CALL_LOG" | grep -q 'Title: ROLLBACK FAILED'; then ok; else fail "restore failure must ntfy ROLLBACK FAILED"; fi
    cleanup_sandbox
}

test_migrate_failure_restores() {
    echo "test: --migrate FAILS -> ROLLBACK-WITH-RESTORE (restore first)"
    make_sandbox
    STUB_MIGRATE_CHECK_RC=2 STUB_MIGRATE_RC=1 run_deploy "$VALID_SHA"
    if [ "$RC" -eq 1 ]; then ok; else fail "migrate failure should exit 1, got $RC"; fi
    if log_has "restore-db.sh --local --no-app-start"; then ok; else fail "migrate failure must restore"; fi
    cleanup_sandbox
}

test_post_migrate_swap_failure_restores() {
    echo "test: post-migrate SWAP (start) FAILS -> ROLLBACK-WITH-RESTORE"
    make_sandbox
    # migrate-check pending, migrate OK, but the post-migrate app `start` fails
    # (DB start in step c must still succeed, else we never reach the migrate).
    STUB_MIGRATE_CHECK_RC=2 STUB_MIGRATE_RC=0 STUB_APP_START_FAIL=1 run_deploy "$VALID_SHA"
    if [ "$RC" -eq 1 ]; then ok; else fail "post-migrate swap failure should exit 1, got $RC"; fi
    # restore MUST run (DB was forward-migrated; swap failed).
    if log_has "restore-db.sh --local --no-app-start"; then ok; else fail "post-migrate failure must restore the DB"; fi
    cleanup_sandbox
}

test_rollback_restore_ordering() {
    echo "test: ROLLBACK-WITH-RESTORE runs restore BEFORE re-pin (unconditional)"
    make_sandbox
    # Health fails after a successful migrate -> rollback. Assert order:
    #   stop app (rollback) -> restore-db.sh -> re-pin Image= (sed on units) -> start app
    STUB_MIGRATE_CHECK_RC=2 STUB_MIGRATE_RC=0 STUB_HEALTH_OK=0 run_deploy "$VALID_SHA"
    if [ "$RC" -eq 1 ]; then ok; else fail "health fail after migrate should exit 1, got $RC"; fi
    local i_restore i_repin i_startapp
    i_restore="$(log_idx 'restore-db.sh --local --no-app-start')"
    # The re-pin sed that writes the rollback ref (digest) -- the LAST sed writing the unit.
    i_repin="$(grep -nF 'sed -i' "$CALL_LOG" | grep -F 'personalcrm-backend@sha256' | head -1 | cut -d: -f1)"
    i_startapp="$(grep -nF 'start personalcrm-backend.service personalcrm-frontend.service' "$CALL_LOG" | tail -1 | cut -d: -f1)"
    if [ -n "$i_restore" ]; then ok; else fail "restore must run in rollback"; fi
    if [ -n "$i_restore" ] && [ -n "$i_repin" ] && [ "$i_restore" -lt "$i_repin" ]; then ok; else fail "restore must precede re-pin (got restore=$i_restore repin=$i_repin)"; fi
    if [ -n "$i_repin" ] && [ -n "$i_startapp" ] && [ "$i_repin" -lt "$i_startapp" ]; then ok; else fail "re-pin must precede app start"; fi
    cleanup_sandbox
}

test_restore_failure_is_rollback_failed() {
    echo "test: restore FAILS in rollback -> ROLLBACK FAILED, app left stopped"
    make_sandbox
    STUB_MIGRATE_CHECK_RC=2 STUB_MIGRATE_RC=0 STUB_HEALTH_OK=0 STUB_RESTORE_RC=1 run_deploy "$VALID_SHA"
    if [ "$RC" -eq 1 ]; then ok; else fail "restore failure should exit 1, got $RC"; fi
    # After a restore failure, the app must NOT be (re)started.
    local i_restore i_startapp
    i_restore="$(log_idx 'restore-db.sh --local --no-app-start')"
    i_startapp="$(grep -nF 'start personalcrm-backend.service personalcrm-frontend.service' "$CALL_LOG" | awk -F: -v r="$i_restore" '$1 > r {print $1; exit}')"
    if [ -z "$i_startapp" ]; then ok; else fail "app must stay STOPPED after restore failure (started at $i_startapp)"; fi
    cleanup_sandbox
}

test_repin_failure_is_rollback_failed() {
    echo "test: re-pin FAILS after restore -> ROLLBACK FAILED, app left stopped"
    make_sandbox
    # Restore succeeds, but make the re-pin daemon-reload fail.
    STUB_MIGRATE_CHECK_RC=2 STUB_MIGRATE_RC=0 STUB_HEALTH_OK=0 STUB_SYSTEMCTL_FAIL=daemon-reload run_deploy "$VALID_SHA"
    if [ "$RC" -eq 1 ]; then ok; else fail "re-pin failure should exit 1, got $RC"; fi
    if log_has "restore-db.sh --local --no-app-start"; then ok; else fail "restore must have run before re-pin"; fi
    # App must NOT be started after a failed re-pin.
    local i_restore i_startapp
    i_restore="$(log_idx 'restore-db.sh --local --no-app-start')"
    i_startapp="$(grep -nF 'start personalcrm-backend.service personalcrm-frontend.service' "$CALL_LOG" | awk -F: -v r="$i_restore" '$1 > r {print $1; exit}')"
    if [ -z "$i_startapp" ]; then ok; else fail "app must stay STOPPED after re-pin failure"; fi
    cleanup_sandbox
}

test_ntfy_degrade_open() {
    echo "test: ntfy absent env file -> skip + still deploy (degrade-open)"
    make_sandbox
    # NTFY_ENV_FILE points at a missing file by default; confirm a clean deploy.
    STUB_MIGRATE_CHECK_RC=0 run_deploy "$VALID_SHA"
    if [ "$RC" -eq 0 ]; then ok; else fail "degrade-open deploy should exit 0, got $RC"; fi
    # No ntfy POST should have been attempted (no curl to an ntfy URL).
    if grep -F 'curl' "$CALL_LOG" | grep -qi 'ntfy'; then fail "ntfy posted despite absent env file"; else ok; fi
    cleanup_sandbox
}

test_ntfy_present_outcomes() {
    echo "test: ntfy present -> correct Title/Priority/Tags per outcome"
    make_sandbox
    printf 'NTFY_URL=https://ntfy.example\nNTFY_TOPIC=tok\n' > "$SANDBOX/ntfy.env"
    NTFY_ENV_FILE_OVERRIDE="$SANDBOX/ntfy.env" STUB_MIGRATE_CHECK_RC=0 run_deploy "$VALID_SHA"
    # Deploy OK: Title 'Deploy OK', Tags white_check_mark.
    if grep -F 'curl' "$CALL_LOG" | grep -q 'Title: Deploy OK'; then ok; else fail "missing 'Deploy OK' ntfy title"; fi
    if grep -F 'curl' "$CALL_LOG" | grep -q 'Tags: white_check_mark'; then ok; else fail "missing white_check_mark tag"; fi
    cleanup_sandbox

    make_sandbox
    printf 'NTFY_URL=https://ntfy.example\nNTFY_TOPIC=tok\n' > "$SANDBOX/ntfy.env"
    NTFY_ENV_FILE_OVERRIDE="$SANDBOX/ntfy.env" STUB_MIGRATE_CHECK_RC=2 STUB_MIGRATE_RC=0 STUB_HEALTH_OK=0 STUB_RESTORE_RC=1 run_deploy "$VALID_SHA"
    if grep -F 'curl' "$CALL_LOG" | grep -q 'Title: ROLLBACK FAILED'; then ok; else fail "missing ROLLBACK FAILED ntfy title"; fi
    if grep -F 'curl' "$CALL_LOG" | grep -q 'Priority: urgent'; then ok; else fail "ROLLBACK FAILED must be urgent priority"; fi
    cleanup_sandbox
}

test_ntfy_post_failure_does_not_change_outcome() {
    echo "test: present-but-failing ntfy curl does not change deploy outcome"
    make_sandbox
    printf 'NTFY_URL=https://ntfy.invalid\nNTFY_TOPIC=tok\n' > "$SANDBOX/ntfy.env"
    # Make curl to the ntfy host fail; the deploy itself is up-to-date + healthy.
    cat > "$SANDBOX/bin/curl" <<EOF
#!/usr/bin/env bash
echo "curl \$*" >> "$CALL_LOG"
url="\${@: -1}"
case "\$url" in
  *contacts*) echo -n 200; exit 0 ;;
  *8080/health*) echo '{"database":"healthy"}'; exit 0 ;;
  *3001*) exit 0 ;;
  *ntfy.invalid*) exit 7 ;;
  *) exit 0 ;;
esac
EOF
    chmod +x "$SANDBOX/bin/curl"
    NTFY_ENV_FILE_OVERRIDE="$SANDBOX/ntfy.env" STUB_MIGRATE_CHECK_RC=0 run_deploy "$VALID_SHA"
    if [ "$RC" -eq 0 ]; then ok; else fail "failing ntfy POST must not change exit 0, got $RC"; fi
    cleanup_sandbox
}

test_snapshot_retained_on_failure() {
    echo "test: snapshot is never pruned on a failure path"
    make_sandbox
    STUB_MIGRATE_CHECK_RC=2 STUB_MIGRATE_RC=0 STUB_HEALTH_OK=0 run_deploy "$VALID_SHA"
    # The prune helper deletes via `rm -rf ...bak-...`; on a rollback it must NOT run.
    if grep -F 'sudo rm -rf' "$CALL_LOG" | grep -q '_data.bak-'; then fail "snapshot pruned on a failure path"; else ok; fi
    cleanup_sandbox
}

# ---------------------------------------------------------------------------
main() {
    test_sha_validation
    test_rollback_ref_sha_fixture
    test_rollback_ref_latest_digest
    test_migrate_command_line
    test_rootless_env_on_every_call
    test_exit0_uptodate_no_db
    test_exit1_abort_no_touch
    test_exit_other_abort
    test_pending_happy_path
    test_backup_failure_aborts_with_ntfy
    test_restore_pg_not_ready_is_rollback_failed
    test_migrate_failure_restores
    test_post_migrate_swap_failure_restores
    test_rollback_restore_ordering
    test_restore_failure_is_rollback_failed
    test_repin_failure_is_rollback_failed
    test_ntfy_degrade_open
    test_ntfy_present_outcomes
    test_ntfy_post_failure_does_not_change_outcome
    test_snapshot_retained_on_failure

    echo ""
    echo "===================="
    echo "PASS=$PASS FAIL=$FAIL"
    echo "===================="
    [ "$FAIL" -eq 0 ]
}

main "$@"
