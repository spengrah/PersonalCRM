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
#   - the name model: pulled only when the revision names a model image other
#     than the installed one, its units installed alike under production's and
#     staging's configuration, a missing image aborting before the DB, restored
#     on rollback, and never started by the deploy (PR2.c5).

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$REPO_ROOT/scripts/deploy-artifact.sh"
TESTDATA="$REPO_ROOT/scripts/testdata"
VALID_SHA="abcdef0123456789abcdef0123456789abcdef01"
NEW_SHA="$VALID_SHA"
MODEL_REPO="ghcr.io/spengrah/personalcrm-name-model"
# The model image tags two revisions name: the host runs MODEL_TAG_OLD, and a
# revision that changes the model's sources names MODEL_TAG_NEW.
MODEL_TAG_OLD="src-1111111111111111111111111111111111111111"
MODEL_TAG_NEW="src-2222222222222222222222222222222222222222"
MODEL_UNITS="$REPO_ROOT/name-model/units"

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
    # The model's units as a host holds them: installed from the image the host
    # runs (MODEL_TAG_OLD), its Image= pinned. "none" = no model installed yet.
    MODEL_UNIT="$SANDBOX/home/.config/containers/systemd/personalcrm-name-model.container"
    MODEL_SOCKET_UNIT="$SANDBOX/home/.config/systemd/user/personalcrm-name-model.socket"
    if [ "${3:-installed}" = installed ]; then
        mkdir -p "$(dirname "$MODEL_UNIT")" "$(dirname "$MODEL_SOCKET_UNIT")"
        sed "s|^Image=.*|Image=$MODEL_REPO:$MODEL_TAG_OLD|" \
            "$MODEL_UNITS/personalcrm-name-model.container" > "$MODEL_UNIT"
        cp "$MODEL_UNITS/personalcrm-name-model.socket" "$MODEL_SOCKET_UNIT"
    fi

    # --- stub: id (resolve a tenant uid without a real account) ---
    cat > "$SANDBOX/bin/id" <<'EOF'
#!/usr/bin/env bash
# id -u <tenant> -> a fixed fake uid per tenant; everything else -> real id.
if [ "$1" = "-u" ]; then
    case "$2" in
        crm)     echo 4242; exit 0 ;;
        staging) echo 1995; exit 0 ;;
    esac
fi
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
    # container inspect: \`podman inspect <container> --format '{{.Image}}'\` -> image id
    echo "\${STUB_IMAGE_ID:-c91da029deadbeefc91da029deadbeefc91da029deadbeefc91da029deadbeef}"
    exit 0 ;;
  image)
    # image inspect of the backend image's model label -> the model image tag
    # the revision names (STUB_MODEL_TAG; set it empty for a missing label).
    if [[ "\$*" == *org.personalcrm.name-model.tag* ]]; then
      echo "\${STUB_MODEL_TAG-$MODEL_TAG_OLD}"
      exit 0
    fi
    # image inspect: \`podman image inspect <id> --format '{{index .RepoDigests 0}}'\` -> repo digest
    echo "\${STUB_INSPECT_DIGEST:-ghcr.io/spengrah/personalcrm-backend@sha256:c91da029deadbeefc91da029deadbeefc91da029deadbeefc91da029deadbeef}"
    exit 0 ;;
  pull)
    # STUB_MODEL_PULL_RC fails only the model image's pull.
    if [[ "\$*" == *personalcrm-name-model* ]]; then exit "\${STUB_MODEL_PULL_RC:-0}"; fi
    exit "\${STUB_PULL_RC:-0}" ;;
  volume)
    echo "$SANDBOX/volume/_data"
    exit 0 ;;
  exec)
    # pg_isready -- STUB_PG_NOT_READY=1 simulates postgres never becoming ready.
    exit "\${STUB_PG_NOT_READY:-0}" ;;
  run)
    # A unit file read out of the model image: serve the repository's own
    # name-model/units, which is what the image holds.
    if [[ "\$*" == *"--entrypoint cat"* ]]; then
      cat "$MODEL_UNITS/\$(basename "\${@: -1}")"
      exit
    fi
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
    # The health stub emits the REAL bare-/health (liveness) payload shape: the
    # top-level "status" + "probe", the byte-identical components.database object
    # the gate's body-wide grep matches, the version.git_commit the gate's commit
    # check reads, AND the additive river/sync/disk components the server now
    # serves. The gate hits the LIVENESS route (bare /health), so top-level stays
    # "healthy" whenever the DB is healthy regardless of the new components. The
    # git_commit defaults to $VALID_SHA (expanded at stub-generation time) — the
    # SHA every run_deploy call passes — so every existing green-path test
    # exercises the commit-MATCH path. A test can override it per-run via
    # STUB_HEALTH_GIT_COMMIT (kept escaped here so it resolves at stub-execution
    # time, e.g. a mismatched SHA or "unknown"). Unhealthy => exit non-zero,
    # modelling the handler's HTTP 503 + `curl -sf`'s -f flag (non-zeroes on >=400).
    cat > "$SANDBOX/bin/curl" <<EOF
#!/usr/bin/env bash
echo "curl \$*" >> "$CALL_LOG"
url="\${@: -1}"
case "\$url" in
  *contacts*) echo -n "\${STUB_CADDY_CODE:-200}"; exit 0 ;;
  *8080/health*)
    if [ "\${STUB_HEALTH_OK:-1}" = "1" ]; then
      echo "{\"status\":\"healthy\",\"probe\":\"liveness\",\"timestamp\":\"t\",\"version\":{\"version\":\"dev\",\"build_time\":\"t\",\"git_commit\":\"\${STUB_HEALTH_GIT_COMMIT:-$VALID_SHA}\"},\"components\":{\"database\":{\"status\":\"healthy\",\"response_time\":\"1ms\"},\"river\":{\"status\":\"healthy\",\"details\":{\"discarded_count\":0}},\"sync\":{\"status\":\"healthy\",\"details\":{\"active_breach_count\":0}},\"disk\":{\"status\":\"healthy\",\"details\":{\"path\":\"/\",\"free_percent\":42.0,\"free_bytes\":1,\"total_bytes\":2}}}}"
      exit 0
    fi
    # Unhealthy: handler returns 503; with -sf curl writes nothing and non-zeroes.
    exit 22 ;;
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
    # Also records the tenant identity it INHERITED from deploy-artifact.sh's
    # `export CRM_USER CRM_HOME` (distinct `child-env-backup` prefix so it never
    # collides with the `backup-db.sh` argv assertions / log_lacks checks).
    cat > "$SANDBOX/bin/backup-db.sh" <<EOF
#!/usr/bin/env bash
echo "backup-db.sh \$*" >> "$CALL_LOG"
echo "child-env-backup CRM_USER=\${CRM_USER:-} CRM_HOME=\${CRM_HOME:-}" >> "$CALL_LOG"
if [ "\${STUB_BACKUP_RC:-0}" != "0" ]; then exit "\${STUB_BACKUP_RC}"; fi
echo "BACKUP_PATH=$SANDBOX/volume/_data.bak-20260611-000000"
exit 0
EOF

    # --- stub: restore-db.sh ---
    cat > "$SANDBOX/bin/restore-db.sh" <<EOF
#!/usr/bin/env bash
echo "restore-db.sh \$*" >> "$CALL_LOG"
echo "child-env-restore CRM_USER=\${CRM_USER:-} CRM_HOME=\${CRM_HOME:-}" >> "$CALL_LOG"
exit "\${STUB_RESTORE_RC:-0}"
EOF

    # --- stub: sleep (no-op so readiness/health retry loops run instantly) ---
    cat > "$SANDBOX/bin/sleep" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF

    chmod +x "$SANDBOX"/bin/*
    mkdir -p "$SANDBOX/volume"
    : > "$SANDBOX/volume/_data" 2>/dev/null || mkdir -p "$SANDBOX/volume/_data"
}

cleanup_sandbox() { [ -n "${SANDBOX:-}" ] && rm -rf "$SANDBOX"; }

# run_deploy <sha> : run deploy-artifact.sh in the sandbox; sets RC + OUT. The
# tenant defaults to prod (crm / $SANDBOX/home); a test can override CRM_USER /
# CRM_HOME via CRM_USER_OVERRIDE / CRM_HOME_OVERRIDE to exercise the staging path.
run_deploy() {
    OUT="$(
        PATH="$SANDBOX/bin:$PATH" \
        CRM_USER="${CRM_USER_OVERRIDE:-crm}" \
        CRM_HOME="${CRM_HOME_OVERRIDE:-$SANDBOX/home}" \
        BACKEND_UNIT="$BACKEND_UNIT" \
        FRONTEND_UNIT="$FRONTEND_UNIT" \
        MODEL_UNIT="$MODEL_UNIT" \
        MODEL_SOCKET_UNIT="$MODEL_SOCKET_UNIT" \
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
        if log_lacks "personalcrm-backup-predeploy.service"; then ok
        else fail "bad SHA '$bad' started the predeploy backup"; fi
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
    # :latest must resolve each container's image id, then read RepoDigests from the IMAGE.
    if log_has "podman inspect crm-backend"; then ok; else fail ":latest must inspect crm-backend for its image id"; fi
    if log_has "podman inspect crm-frontend"; then ok; else fail ":latest must inspect crm-frontend for its image id"; fi
    if log_has "podman image inspect"; then ok; else fail ":latest must resolve the digest via podman image inspect"; fi
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
    if grep -q 'crm-admin' <<<"$(grep -F 'podman exec' "$CALL_LOG")"; then fail "crm-admin must NOT run via podman exec"; else ok; fi
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
    if grep -q 'DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/4242/bus' <<<"$(grep -E '^sudo .*systemctl' "$CALL_LOG")"; then ok
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

test_predeploy_backup_ordering() {
    echo "test: predeploy backup runs after pulls and before migrate-check"
    make_sandbox
    STUB_MIGRATE_CHECK_RC=0 run_deploy "$VALID_SHA"
    local i_backend_pull i_frontend_pull i_backup i_migrate i_stop
    i_backend_pull="$(log_idx "podman pull ghcr.io/spengrah/personalcrm-backend:$NEW_SHA")"
    i_frontend_pull="$(log_idx "podman pull ghcr.io/spengrah/personalcrm-frontend:$NEW_SHA")"
    i_backup="$(log_idx 'systemctl --user start personalcrm-backup-predeploy.service')"
    i_migrate="$(grep -nF 'podman run' "$CALL_LOG" | grep -F -- '--migrate-check' | head -1 | cut -d: -f1)"
    if [ -n "$i_backend_pull" ] && [ -n "$i_frontend_pull" ] && [ -n "$i_backup" ] && \
        [ -n "$i_migrate" ] && [ "$i_backend_pull" -lt "$i_backup" ] && \
        [ "$i_frontend_pull" -lt "$i_backup" ] && [ "$i_backup" -lt "$i_migrate" ]; then ok
    else fail "up-to-date predeploy backup ordering is incorrect"; fi
    cleanup_sandbox

    make_sandbox
    STUB_MIGRATE_CHECK_RC=2 run_deploy "$VALID_SHA"
    i_backend_pull="$(log_idx "podman pull ghcr.io/spengrah/personalcrm-backend:$NEW_SHA")"
    i_frontend_pull="$(log_idx "podman pull ghcr.io/spengrah/personalcrm-frontend:$NEW_SHA")"
    i_backup="$(log_idx 'systemctl --user start personalcrm-backup-predeploy.service')"
    i_migrate="$(grep -nF 'podman run' "$CALL_LOG" | grep -F -- '--migrate-check' | head -1 | cut -d: -f1)"
    i_stop="$(log_idx 'systemctl --user stop personalcrm-backend.service personalcrm-frontend.service')"
    if [ -n "$i_backend_pull" ] && [ -n "$i_frontend_pull" ] && [ -n "$i_backup" ] && \
        [ -n "$i_migrate" ] && [ -n "$i_stop" ] && [ "$i_backend_pull" -lt "$i_backup" ] && \
        [ "$i_frontend_pull" -lt "$i_backup" ] && [ "$i_backup" -lt "$i_migrate" ] && \
        [ "$i_backup" -lt "$i_stop" ]; then ok
    else fail "pending predeploy backup ordering is incorrect"; fi
    cleanup_sandbox
}

test_predeploy_backup_failure_degrades_open() {
    echo "test: predeploy backup failure notifies and does not block deploy"
    make_sandbox
    printf 'NTFY_URL=https://ntfy.example\nNTFY_TOPIC=tok\n' > "$SANDBOX/ntfy.env"
    NTFY_ENV_FILE_OVERRIDE="$SANDBOX/ntfy.env" \
        STUB_SYSTEMCTL_FAIL=personalcrm-backup-predeploy.service STUB_MIGRATE_CHECK_RC=0 \
        run_deploy "$VALID_SHA"
    if [ "$RC" -eq 0 ]; then ok; else fail "predeploy backup failure should still deploy successfully (rc=$RC)"; fi
    if grep -q "Image=ghcr.io/spengrah/personalcrm-backend:$NEW_SHA" "$BACKEND_UNIT"; then ok
    else fail "deploy did not pin the new backend image after backup failure"; fi
    local failure_line failure_body failure_idx success_idx
    failure_line="$(grep -F 'Title: Pre-deploy backup failed' "$CALL_LOG" | head -1 || true)"
    if [ -n "$failure_line" ]; then ok; else fail "predeploy backup failure did not send its ntfy notification"; fi
    failure_body="$(sed -n 's/.* -d \(.*\) https:\/\/ntfy\.example\/tok$/\1/p' <<< "$failure_line")"
    if [ -n "$failure_body" ] && [[ "$failure_body" != *tok* ]]; then ok
    else fail "predeploy failure notification body contains the topic"; fi
    failure_idx="$(grep -nF 'Title: Pre-deploy backup failed' "$CALL_LOG" | head -1 | cut -d: -f1)"
    success_idx="$(grep -nF 'Title: Deploy OK' "$CALL_LOG" | head -1 | cut -d: -f1)"
    if [ -n "$failure_idx" ] && [ -n "$success_idx" ] && [ "$failure_idx" -lt "$success_idx" ]; then ok
    else fail "Deploy OK notification did not follow the predeploy failure notification"; fi
    cleanup_sandbox
}

test_pull_failure_aborts_before_predeploy_backup() {
    echo "test: image pull failure aborts before predeploy backup"
    make_sandbox
    STUB_PULL_RC=1 run_deploy "$VALID_SHA"
    if [ "$RC" -eq 1 ]; then ok; else fail "pull failure should exit 1, got $RC"; fi
    if log_lacks "personalcrm-backup-predeploy.service"; then ok
    else fail "pull failure started the predeploy backup"; fi
    cleanup_sandbox
}

test_health_gate_matches_real_payload() {
    echo "test: health-gate matches the REAL /health payload shape"
    # Healthy real payload (top-level status + nested components.database.status)
    # must PASS -> up-to-date deploy exits 0. If the matcher were wrong (e.g. the
    # old flat "database":"healthy" grep), this would fail because the stub now
    # emits the shape the server actually returns.
    #
    # The commit-MATCH path is also exercised here (and by every other green-path
    # test): the stub defaults version.git_commit to $VALID_SHA, which is the SHA
    # run_deploy passes, so the forward gate's commit check passes on a match.
    make_sandbox
    STUB_MIGRATE_CHECK_RC=0 STUB_HEALTH_OK=1 run_deploy "$VALID_SHA"
    if [ "$RC" -eq 0 ]; then ok; else fail "healthy real payload should pass deploy (exit 0), got $RC ($OUT)"; fi
    cleanup_sandbox

    # Unhealthy (HTTP 503 -> curl -sf non-zero) must FAIL the gate -> rollback.
    make_sandbox
    STUB_MIGRATE_CHECK_RC=0 STUB_HEALTH_OK=0 run_deploy "$VALID_SHA"
    if [ "$RC" -eq 1 ]; then ok; else fail "unhealthy /health should fail the gate -> rollback (exit 1), got $RC"; fi
    cleanup_sandbox
}

# A different (stamped-but-wrong) 40-hex commit: the wrong image is actually
# serving. Used by the mismatch + rollback-non-enforcement tests below.
WRONG_SHA="badc0ffee0123456789abcdef0123456789abcde"

test_health_gate_commit_mismatch_fails() {
    echo "test: forward gate FAILS when /health reports a different commit"
    # Up-to-date path: /health is overall-healthy but reports a commit != the
    # deployed SHA. The forward gate (passed "$SHA") must FAIL on the commit
    # check -> rollback the image (no DB touched). Use :latest fixtures so the
    # rollback re-pins the resolved @sha256 digest anchor.
    make_sandbox latest latest
    STUB_MIGRATE_CHECK_RC=0 STUB_HEALTH_OK=1 STUB_HEALTH_GIT_COMMIT="$WRONG_SHA" run_deploy "$VALID_SHA"
    if [ "$RC" -eq 1 ]; then ok; else fail "commit mismatch should fail the gate -> rollback (exit 1), got $RC ($OUT)"; fi
    # The forward gate must have logged the mismatch (not the unknown-skip).
    if grep -qF -- 'git_commit does not match' <<<"$OUT"; then ok; else fail "expected a commit-mismatch log line"; fi
    # Rollback re-pins the OLD digest anchor on both units.
    if grep -q 'Image=ghcr.io/spengrah/personalcrm-backend@sha256:c91da029' "$BACKEND_UNIT"; then ok
    else fail "mismatch must re-pin the rollback digest anchor"; fi
    cleanup_sandbox
}

test_health_gate_unknown_warns_and_passes() {
    echo "test: forward gate WARNS and PASSES when /health reports git_commit=unknown"
    # Re-deploying a pre-stamp image (binary predates the build stamp) reports
    # git_commit=unknown. The gate must degrade-open: log a skip warning, NOT
    # fail -> the up-to-date deploy still exits 0.
    make_sandbox
    STUB_MIGRATE_CHECK_RC=0 STUB_HEALTH_OK=1 STUB_HEALTH_GIT_COMMIT="unknown" run_deploy "$VALID_SHA"
    if [ "$RC" -eq 0 ]; then ok; else fail "unknown commit should warn + pass (exit 0), got $RC ($OUT)"; fi
    if grep -qF -- 'build not stamped (git_commit=unknown); skipping commit verification' <<<"$OUT"; then ok
    else fail "expected the unknown-skip warning in the deploy output"; fi
    cleanup_sandbox
}

test_rollback_gate_does_not_enforce_commit() {
    echo "test: rollback's best-effort gate does NOT enforce the commit (no ROLLBACK FAILED)"
    # Pending path: migrate OK, but the forward gate fails on a commit mismatch
    # (status healthy, wrong commit) -> ROLLBACK-WITH-RESTORE. The rolled-back
    # stack still serves the SAME mismatched commit, but the rollback gate is
    # bare (no expected_sha) so it must NOT enforce the commit and must NOT
    # convert a clean rollback into ROLLBACK FAILED. Expect the normal
    # "Rolled back" notification and a clean (restore + re-pin succeeded) exit 1.
    make_sandbox
    printf 'NTFY_URL=https://ntfy.example\nNTFY_TOPIC=tok\n' > "$SANDBOX/ntfy.env"
    NTFY_ENV_FILE_OVERRIDE="$SANDBOX/ntfy.env" \
      STUB_MIGRATE_CHECK_RC=2 STUB_MIGRATE_RC=0 STUB_HEALTH_OK=1 STUB_HEALTH_GIT_COMMIT="$WRONG_SHA" run_deploy "$VALID_SHA"
    if [ "$RC" -eq 1 ]; then ok; else fail "commit mismatch after migrate should roll back (exit 1), got $RC ($OUT)"; fi
    # Restore ran (forward-migrated DB undone).
    if log_has "restore-db.sh --local --no-app-start"; then ok; else fail "rollback must restore the DB"; fi
    # The rollback completed normally — "Rolled back", NOT "ROLLBACK FAILED".
    if grep -q 'Title: Rolled back' <<<"$(grep -F 'curl' "$CALL_LOG")"; then ok; else fail "expected a normal 'Rolled back' notification"; fi
    if grep -q 'Title: ROLLBACK FAILED' <<<"$(grep -F 'curl' "$CALL_LOG")"; then fail "rollback gate must NOT cause ROLLBACK FAILED"; else ok; fi
    cleanup_sandbox
}

test_exit0_uptodate_no_db() {
    echo "test: exit 0 (up-to-date) touches no DB, swaps image, restarts app"
    make_sandbox
    STUB_MIGRATE_CHECK_RC=0 run_deploy "$VALID_SHA"
    if [ "$RC" -eq 0 ]; then ok; else fail "up-to-date should exit 0, got $RC ($OUT)"; fi
    if log_lacks "backup-db.sh"; then ok; else fail "up-to-date must NOT snapshot the DB"; fi
    if log_lacks "podman run"; then fail "expected a migrate-check podman run"; else ok; fi
    if grep -q -- '--migrate$' <<<"$(grep -F 'podman run' "$CALL_LOG")"; then fail "up-to-date must NOT --migrate"; else ok; fi
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
    if grep -q -- '--migrate$' <<<"$(grep -F 'podman run' "$CALL_LOG")"; then fail "error path must NOT migrate"; else ok; fi
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

test_pg_not_ready_aborts_before_migrate() {
    echo "test: postgres never ready -> abort BEFORE migrate (no restore path)"
    make_sandbox
    printf 'NTFY_URL=https://ntfy.example\nNTFY_TOPIC=tok\n' > "$SANDBOX/ntfy.env"
    NTFY_ENV_FILE_OVERRIDE="$SANDBOX/ntfy.env" STUB_MIGRATE_CHECK_RC=2 STUB_PG_NOT_READY=1 run_deploy "$VALID_SHA"
    if [ "$RC" -eq 1 ]; then ok; else fail "pg-not-ready should exit 1, got $RC"; fi
    # Must NOT migrate (postgres never came up) and must NOT take the restore path.
    if grep -q -- '--migrate$' <<<"$(grep -F 'podman run' "$CALL_LOG")"; then fail "must NOT migrate when pg not ready"; else ok; fi
    if log_lacks "restore-db.sh"; then ok; else fail "pg-not-ready is NOT a restore path"; fi
    if grep -q 'Title: Deploy aborted' <<<"$(grep -F 'curl' "$CALL_LOG")"; then ok; else fail "pg-not-ready must ntfy 'Deploy aborted'"; fi
    cleanup_sandbox
}

test_backup_failure_aborts_with_ntfy() {
    echo "test: snapshot/backup non-zero exit -> abort + ntfy (no migrate)"
    make_sandbox
    printf 'NTFY_URL=https://ntfy.example\nNTFY_TOPIC=tok\n' > "$SANDBOX/ntfy.env"
    NTFY_ENV_FILE_OVERRIDE="$SANDBOX/ntfy.env" STUB_MIGRATE_CHECK_RC=2 STUB_BACKUP_RC=1 run_deploy "$VALID_SHA"
    if [ "$RC" -eq 1 ]; then ok; else fail "backup failure should exit 1, got $RC"; fi
    # Must NOT have migrated (backup failed first).
    if grep -q -- '--migrate$' <<<"$(grep -F 'podman run' "$CALL_LOG")"; then fail "must NOT migrate after backup failure"; else ok; fi
    # Must have sent a 'Deploy aborted' ntfy (not silently restart via the trap).
    if grep -q 'Title: Deploy aborted' <<<"$(grep -F 'curl' "$CALL_LOG")"; then ok; else fail "backup failure must ntfy 'Deploy aborted'"; fi
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
    if grep -q 'Title: ROLLBACK FAILED' <<<"$(grep -F 'curl' "$CALL_LOG")"; then ok; else fail "restore failure must ntfy ROLLBACK FAILED"; fi
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

test_ntfy_post_is_time_bounded() {
    echo "test: ntfy POSTs carry connect and total timeouts"
    # An ntfy server that accepts then stalls would hang the deploy mid-run.
    if grep -q -- '--connect-timeout' "$SCRIPT" && grep -q -- '--max-time' "$SCRIPT"; then ok
    else fail "ntfy() does not bound its POST duration"; fi
}

test_ntfy_degrade_open() {
    echo "test: ntfy absent env file -> skip + still deploy (degrade-open)"
    make_sandbox
    # NTFY_ENV_FILE points at a missing file by default; confirm a clean deploy.
    STUB_MIGRATE_CHECK_RC=0 run_deploy "$VALID_SHA"
    if [ "$RC" -eq 0 ]; then ok; else fail "degrade-open deploy should exit 0, got $RC"; fi
    # No ntfy POST should have been attempted (no curl to an ntfy URL).
    if grep -qi 'ntfy' <<<"$(grep -F 'curl' "$CALL_LOG")"; then fail "ntfy posted despite absent env file"; else ok; fi
    cleanup_sandbox
}

test_ntfy_present_outcomes() {
    echo "test: ntfy present -> correct Title/Priority/Tags per outcome"
    make_sandbox
    printf 'NTFY_URL=https://ntfy.example\nNTFY_TOPIC=tok\n' > "$SANDBOX/ntfy.env"
    NTFY_ENV_FILE_OVERRIDE="$SANDBOX/ntfy.env" STUB_MIGRATE_CHECK_RC=0 run_deploy "$VALID_SHA"
    # Deploy OK: Title 'Deploy OK', Tags white_check_mark.
    if grep -q 'Title: Deploy OK' <<<"$(grep -F 'curl' "$CALL_LOG")"; then ok; else fail "missing 'Deploy OK' ntfy title"; fi
    if grep -q 'Tags: white_check_mark' <<<"$(grep -F 'curl' "$CALL_LOG")"; then ok; else fail "missing white_check_mark tag"; fi
    cleanup_sandbox

    make_sandbox
    printf 'NTFY_URL=https://ntfy.example\nNTFY_TOPIC=tok\n' > "$SANDBOX/ntfy.env"
    NTFY_ENV_FILE_OVERRIDE="$SANDBOX/ntfy.env" STUB_MIGRATE_CHECK_RC=2 STUB_MIGRATE_RC=0 STUB_HEALTH_OK=0 STUB_RESTORE_RC=1 run_deploy "$VALID_SHA"
    if grep -q 'Title: ROLLBACK FAILED' <<<"$(grep -F 'curl' "$CALL_LOG")"; then ok; else fail "missing ROLLBACK FAILED ntfy title"; fi
    if grep -q 'Priority: urgent' <<<"$(grep -F 'curl' "$CALL_LOG")"; then ok; else fail "ROLLBACK FAILED must be urgent priority"; fi
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
  *8080/health*) echo "{\"status\":\"healthy\",\"version\":{\"git_commit\":\"$VALID_SHA\"},\"components\":{\"database\":{\"status\":\"healthy\"}}}"; exit 0 ;;
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
    if grep -q '_data.bak-' <<<"$(grep -F 'sudo rm -rf' "$CALL_LOG")"; then fail "snapshot pruned on a failure path"; else ok; fi
    cleanup_sandbox
}

test_export_propagates_crm_user_home_to_children() {
    echo "test: deploy-artifact exports CRM_USER/CRM_HOME to backup/restore children"
    # DEFAULT (prod) tenant: children inherit crm + the sandbox home. Drive the
    # pending+rollback path so BOTH backup (snapshot) and restore (rollback) run.
    make_sandbox
    STUB_MIGRATE_CHECK_RC=2 STUB_MIGRATE_RC=0 STUB_HEALTH_OK=0 run_deploy "$VALID_SHA"
    if grep -qF "child-env-backup CRM_USER=crm CRM_HOME=$SANDBOX/home" "$CALL_LOG"; then ok
    else fail "default backup child did not inherit crm/$SANDBOX/home"; fi
    if grep -qF "child-env-restore CRM_USER=crm CRM_HOME=$SANDBOX/home" "$CALL_LOG"; then ok
    else fail "default restore child did not inherit crm/$SANDBOX/home"; fi
    cleanup_sandbox

    # STAGING tenant: the `export CRM_USER CRM_HOME` carries staging values through
    # to the children (without it, the children would die on the staging tenant).
    make_sandbox
    CRM_USER_OVERRIDE=staging CRM_HOME_OVERRIDE=/var/lib/staging \
      STUB_MIGRATE_CHECK_RC=2 STUB_MIGRATE_RC=0 STUB_HEALTH_OK=0 run_deploy "$VALID_SHA"
    if grep -qF 'child-env-backup CRM_USER=staging CRM_HOME=/var/lib/staging' "$CALL_LOG"; then ok
    else fail "staging backup child did not inherit the exported CRM_USER/CRM_HOME"; fi
    if grep -qF 'child-env-restore CRM_USER=staging CRM_HOME=/var/lib/staging' "$CALL_LOG"; then ok
    else fail "staging restore child did not inherit the exported CRM_USER/CRM_HOME"; fi
    cleanup_sandbox
}

# ===========================================================================
# The name model (PR2.c5): installed beside the backend and frontend by this
# one script, on production and staging alike.
# ===========================================================================

# model_unit_for <tag>: the container unit a host should hold for <tag> — the
# image's own unit with its Image= pinned to <tag>.
model_unit_for() {
    sed "s|^Image=.*|Image=$MODEL_REPO:$1|" "$MODEL_UNITS/personalcrm-name-model.container"
}

assert_model_installed() {  # <tag> <container-unit> <socket-unit> <context>
    if cmp -s <(model_unit_for "$1") "$2"; then ok
    else fail "$4: model container unit is not the image's unit pinned to $1"; fi
    if cmp -s "$MODEL_UNITS/personalcrm-name-model.socket" "$3"; then ok
    else fail "$4: model socket unit is not the image's socket unit"; fi
}

test_model_change_pulls_and_installs_named_image() {
    echo "test: a revision that changes the model's sources pulls and installs the model image it names"
    local check
    for check in 0 2; do
        make_sandbox
        STUB_MODEL_TAG="$MODEL_TAG_NEW" STUB_MIGRATE_CHECK_RC=$check run_deploy "$VALID_SHA"
        if [ "$RC" -eq 0 ]; then ok; else fail "migrate-check $check: deploy should exit 0, got $RC ($OUT)"; fi
        if log_has "podman pull $MODEL_REPO:$MODEL_TAG_NEW"; then ok
        else fail "migrate-check $check: the model image the revision names was not pulled"; fi
        assert_model_installed "$MODEL_TAG_NEW" "$MODEL_UNIT" "$MODEL_SOCKET_UNIT" "migrate-check $check"
        # The running model (on the old image) stops; the socket restarts on the
        # new units, so the next request starts the new image.
        if log_has "stop personalcrm-name-model.service" && log_has "restart personalcrm-name-model.socket"; then ok
        else fail "migrate-check $check: the model was not restarted onto its new units"; fi
        cleanup_sandbox
    done
}

test_model_unchanged_pulls_nothing() {
    echo "test: a revision that changes no model source pulls no model image and leaves the installed one"
    local check before
    for check in 0 2; do
        make_sandbox
        before="$(cat "$MODEL_UNIT")"
        STUB_MIGRATE_CHECK_RC=$check run_deploy "$VALID_SHA"
        if [ "$RC" -eq 0 ]; then ok; else fail "migrate-check $check: deploy should exit 0, got $RC ($OUT)"; fi
        if log_lacks "pull $MODEL_REPO"; then ok; else fail "migrate-check $check: pulled a model image though the model is unchanged"; fi
        if [ "$(cat "$MODEL_UNIT")" = "$before" ]; then ok; else fail "migrate-check $check: the installed model unit changed"; fi
        if log_lacks "personalcrm-name-model.socket" && log_lacks "personalcrm-name-model.service"; then ok
        else fail "migrate-check $check: the unchanged model was stopped or restarted"; fi
        cleanup_sandbox
    done
}

test_model_staging_installs_what_prod_installs() {
    echo "test: under staging's configuration the deploy installs the same model image and units as under production's"
    local tenant home
    make_sandbox latest latest none
    for tenant in crm staging; do
        home="$SANDBOX/$tenant-home"
        : > "$CALL_LOG"
        # The tenant's own paths, derived from CRM_HOME by the script's defaults.
        MODEL_UNIT="" MODEL_SOCKET_UNIT="" CRM_USER_OVERRIDE=$tenant CRM_HOME_OVERRIDE="$home" \
            STUB_MODEL_TAG="$MODEL_TAG_NEW" STUB_MIGRATE_CHECK_RC=0 run_deploy "$VALID_SHA"
        if [ "$RC" -eq 0 ]; then ok; else fail "$tenant: deploy should exit 0, got $RC ($OUT)"; fi
        if log_has "podman pull $MODEL_REPO:$MODEL_TAG_NEW"; then ok; else fail "$tenant: model image not pulled"; fi
        assert_model_installed "$MODEL_TAG_NEW" \
            "$home/.config/containers/systemd/personalcrm-name-model.container" \
            "$home/.config/systemd/user/personalcrm-name-model.socket" "$tenant"
        if log_has "systemctl --user enable personalcrm-name-model.socket"; then ok
        else fail "$tenant: the model socket was not enabled"; fi
    done
    cleanup_sandbox
}

test_model_missing_image_fails_before_db() {
    echo "test: a missing model image fails the deploy before the database is touched"
    local label
    # A model image that will not pull, a backend image with no model label, and
    # a label that is no model image tag.
    for label in "pull-fails" "" "latest"; do
        make_sandbox
        if [ "$label" = pull-fails ]; then
            STUB_MODEL_TAG="$MODEL_TAG_NEW" STUB_MODEL_PULL_RC=1 STUB_MIGRATE_CHECK_RC=2 run_deploy "$VALID_SHA"
        else
            STUB_MODEL_TAG="$label" STUB_MIGRATE_CHECK_RC=2 run_deploy "$VALID_SHA"
        fi
        if [ "$RC" -eq 1 ]; then ok; else fail "[$label]: should exit 1, got $RC"; fi
        if log_lacks "--migrate-check" && log_lacks "backup-db.sh" && log_lacks "personalcrm-backup-predeploy.service"; then ok
        else fail "[$label]: the database was touched before the model image was secured"; fi
        if grep -q "Image=$MODEL_REPO:$MODEL_TAG_OLD" "$MODEL_UNIT" \
           && grep -q 'Image=ghcr.io/spengrah/personalcrm-backend:latest' "$BACKEND_UNIT"; then ok
        else fail "[$label]: an Image= line was rewritten on an aborted deploy"; fi
        cleanup_sandbox
    done
}

test_model_rollback_restores_previous_image() {
    echo "test: a rollback restores the model image the previous revision named"
    local check
    # Up-to-date path (image rollback) and pending path (rollback with restore).
    for check in 0 2; do
        make_sandbox
        STUB_MODEL_TAG="$MODEL_TAG_NEW" STUB_MIGRATE_CHECK_RC=$check STUB_HEALTH_OK=0 run_deploy "$VALID_SHA"
        if [ "$RC" -eq 1 ]; then ok; else fail "migrate-check $check: failed health should roll back (exit 1), got $RC"; fi
        if log_has "podman pull $MODEL_REPO:$MODEL_TAG_NEW"; then ok; else fail "migrate-check $check: new model never pulled"; fi
        assert_model_installed "$MODEL_TAG_OLD" "$MODEL_UNIT" "$MODEL_SOCKET_UNIT" "migrate-check $check rollback"
        if [ "$(grep -c '^systemctl --user restart personalcrm-name-model.socket' "$CALL_LOG")" -eq 2 ]; then ok
        else fail "migrate-check $check: the model socket was not restarted onto the restored units"; fi
        cleanup_sandbox
    done
}

test_health_gate_passes_without_starting_model() {
    echo "test: the health gate passes without starting the model, on either host"
    local tenant
    for tenant in crm staging; do
        make_sandbox
        CRM_USER_OVERRIDE=$tenant CRM_HOME_OVERRIDE="$SANDBOX/home" \
            STUB_MODEL_TAG="$MODEL_TAG_NEW" STUB_MIGRATE_CHECK_RC=0 run_deploy "$VALID_SHA"
        if [ "$RC" -eq 0 ]; then ok; else fail "$tenant: deploy should pass its health gate (exit 0), got $RC ($OUT)"; fi
        if grep -E 'systemctl --user (re)?start .*personalcrm-name-model\.service' "$CALL_LOG" >/dev/null; then
            fail "$tenant: the deploy started the model"
        else ok; fi
        if grep -F 'curl' "$CALL_LOG" | grep -qF 'name-model'; then fail "$tenant: the health gate reached the model"; else ok; fi
        cleanup_sandbox
    done
}

# ---------------------------------------------------------------------------
main() {
    test_sha_validation
    test_rollback_ref_sha_fixture
    test_rollback_ref_latest_digest
    test_migrate_command_line
    test_rootless_env_on_every_call
    test_predeploy_backup_ordering
    test_predeploy_backup_failure_degrades_open
    test_pull_failure_aborts_before_predeploy_backup
    test_health_gate_matches_real_payload
    test_health_gate_commit_mismatch_fails
    test_health_gate_unknown_warns_and_passes
    test_rollback_gate_does_not_enforce_commit
    test_exit0_uptodate_no_db
    test_exit1_abort_no_touch
    test_exit_other_abort
    test_pending_happy_path
    test_pg_not_ready_aborts_before_migrate
    test_backup_failure_aborts_with_ntfy
    test_restore_pg_not_ready_is_rollback_failed
    test_migrate_failure_restores
    test_post_migrate_swap_failure_restores
    test_rollback_restore_ordering
    test_restore_failure_is_rollback_failed
    test_repin_failure_is_rollback_failed
    test_ntfy_post_is_time_bounded
    test_ntfy_degrade_open
    test_ntfy_present_outcomes
    test_ntfy_post_failure_does_not_change_outcome
    test_snapshot_retained_on_failure
    test_export_propagates_crm_user_home_to_children
    test_model_change_pulls_and_installs_named_image
    test_model_unchanged_pulls_nothing
    test_model_staging_installs_what_prod_installs
    test_model_missing_image_fails_before_db
    test_model_rollback_restores_previous_image
    test_health_gate_passes_without_starting_model

    echo ""
    echo "===================="
    echo "PASS=$PASS FAIL=$FAIL"
    echo "===================="
    [ "$FAIL" -eq 0 ]
}

main "$@"
