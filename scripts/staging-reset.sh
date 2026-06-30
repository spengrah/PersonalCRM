#!/bin/bash
# HARD reset + reseed the STAGING tenant to the prod-shaped synthetic world.
#
# Drives the rootless `staging` tenant (CRM_USER/CRM_HOME, defaults staging /
# /var/lib/staging) the way deploy-artifact.sh does: stop the backend service via
# `sudo -u <tenant> ... systemctl --user`, run crm-admin --reset-and-seed from a
# SEPARATE ephemeral container off the DEPLOYED backend image (NOT `podman exec`
# into the running one — crm-admin requires the crm-api service stopped, and you
# cannot exec into a stopped container; the wipe truncates river_job with no drain
# guard, so exec-without-stop would race the live worker), then start the backend
# again. crm-postgres and the `crm` network stay up throughout (the wipe/reseed
# connects to the DB over the network).
#
# crm-admin --reset-and-seed HARD-wipes every live data table (incl. river_job,
# oauth_credential, sync-state); the migration-seeded curated catalog survives,
# only its provisional rows are cleared. STAGING ONLY — refuses if CRM_ENV is a
# production alias OR empty/unset (fail-closed, mirroring config.IsProductionCRMEnv
# which treats unset as production), BEFORE stopping anything. crm-admin's own
# SeedAllowed gate is the authoritative in-process guard; this shell check is the
# earlier, stricter backstop.
#
# Usage: ./scripts/staging-reset.sh [--local] [--require-oauth-empty]
#   --local                Run on the VPS as root (no ssh): the tenant podman/
#                          systemctl helpers run via `sudo -u <tenant>` locally.
#                          Default (ssh) mode targets STAGING_HOST (default
#                          stovepipes) so `make staging-reset` and the QA harness
#                          (#380) can drive it from the Mac.
#   --require-oauth-empty  Auto-path guard (deploy-staging.yml). BEFORE stopping
#                          anything, count live oauth_credential rows; skip the
#                          reseed cleanly (exit 0, service untouched, stable
#                          "skipping auto-reseed" log marker) if any exist —
#                          connecting a sync account to staging disables the
#                          destructive wipe so the connection survives. Fail-closed
#                          (exit non-zero) if the count cannot be verified. Manual
#                          `make staging-reset` omits this (operator force = full
#                          wipe regardless of oauth).
#
# Dev seeding is a SEPARATE tool: `make dev-seed` -> scripts/dev-seed.sh. Running
# this on a dev Mac fails loudly (no staging tenant / no STAGING_HOST) — by design.

set -euo pipefail

# Parse arguments
LOCAL=false
REQUIRE_OAUTH_EMPTY=false
for arg in "$@"; do
    case "$arg" in
        --local) LOCAL=true ;;
        --require-oauth-empty) REQUIRE_OAUTH_EMPTY=true ;;
        # The fail-loud drift guard: a stale host copy that predates a newer flag
        # hits this branch and reds the deploy reseed step instead of silently
        # wiping. Keep the message actionable (reinstall the host copy).
        *) echo "staging-reset: unknown argument: $arg (host copy out of date? reinstall /usr/local/sbin/staging-reset.sh)" >&2; exit 2 ;;
    esac
done

# Tenant identity + config (overridable; defaults are the staging tenant). Shared
# resources stay literal: the `crm` network, the crm-postgres container, and the
# personalcrm-backend.service unit name.
CRM_USER="${CRM_USER:-staging}"
CRM_HOME="${CRM_HOME:-/var/lib/staging}"
STAGING_HOST="${STAGING_HOST:-stovepipes}"
ENV_FILE="${STAGING_ENV_FILE:-/srv/personalcrm/.env}"
BACKEND_UNIT="${STAGING_BACKEND_UNIT:-$CRM_HOME/.config/containers/systemd/personalcrm-backend.container}"
PROFILE="${STAGING_RESET_PROFILE:-prod-shaped}"
PODMAN_NETWORK="${STAGING_PODMAN_NETWORK:-crm}"
MIGRATIONS_PATH="${STAGING_MIGRATIONS_PATH:-/migrations}"
CRM_ADMIN="${STAGING_CRM_ADMIN:-/usr/local/bin/crm-admin}"

# Resolve the tenant uid + rootless helpers (mirror backup-db.sh's shapes). The
# `cd /tmp` mirrors the ssh form: an interactive `sudo -u <tenant>` inherits the
# caller's CWD, which the tenant can't access (rootless podman fails to chdir).
if [ "$LOCAL" = true ]; then
    CRM_UID=$(id -u "$CRM_USER")
    USERENV="HOME=$CRM_HOME XDG_RUNTIME_DIR=/run/user/$CRM_UID DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$CRM_UID/bus"
    # USERENV is deliberately unquoted: it must word-split into separate KEY=val
    # arguments for env-style `sudo -u <tenant> KEY=val KEY=val ...`.
    # shellcheck disable=SC2086
    staging_ctl()    { cd /tmp && sudo -u "$CRM_USER" $USERENV systemctl --user "$@"; }
    staging_podman() { cd /tmp && sudo -u "$CRM_USER" HOME=$CRM_HOME XDG_RUNTIME_DIR=/run/user/"$CRM_UID" podman "$@"; }
    read_crm_env()   { sudo -u "$CRM_USER" sed -n 's/^CRM_ENV=//p' "$ENV_FILE" 2>/dev/null | head -1; }
    read_image_ref() { sudo -u "$CRM_USER" sed -n 's/^Image=//p' "$BACKEND_UNIT" 2>/dev/null | head -1; }
    read_database_url(){ sudo -u "$CRM_USER" sed -n 's/^DATABASE_URL=//p' "$ENV_FILE" 2>/dev/null | head -1; }
    env_file_exists(){ sudo -u "$CRM_USER" test -e "$ENV_FILE"; }
else
    if ! ssh -q -o ConnectTimeout=5 "$STAGING_HOST" exit; then
        echo "staging-reset: cannot reach STAGING_HOST '$STAGING_HOST'" >&2
        exit 1
    fi
    CRM_UID=$(ssh "$STAGING_HOST" "id -u $CRM_USER")
    USERENV="HOME=$CRM_HOME XDG_RUNTIME_DIR=/run/user/$CRM_UID DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$CRM_UID/bus"
    # Vars below deliberately expand client-side (resolved here, then sent over ssh
    # as a literal remote command). SC2029 is the intended behavior.
    # shellcheck disable=SC2029
    staging_ctl()    { ssh "$STAGING_HOST" "cd /tmp && sudo -n -u $CRM_USER $USERENV systemctl --user $*"; }
    # shellcheck disable=SC2029
    staging_podman() { ssh "$STAGING_HOST" "cd /tmp && sudo -n -u $CRM_USER HOME=$CRM_HOME XDG_RUNTIME_DIR=/run/user/$CRM_UID podman $*"; }
    # shellcheck disable=SC2029
    read_crm_env()   { ssh "$STAGING_HOST" "sudo -n -u $CRM_USER sed -n 's/^CRM_ENV=//p' '$ENV_FILE' 2>/dev/null | head -1"; }
    # shellcheck disable=SC2029
    read_image_ref() { ssh "$STAGING_HOST" "sudo -n -u $CRM_USER sed -n 's/^Image=//p' '$BACKEND_UNIT' 2>/dev/null | head -1"; }
    # shellcheck disable=SC2029
    read_database_url(){ ssh "$STAGING_HOST" "sudo -n -u $CRM_USER sed -n 's/^DATABASE_URL=//p' '$ENV_FILE' 2>/dev/null | head -1"; }
    # shellcheck disable=SC2029
    env_file_exists(){ ssh "$STAGING_HOST" "sudo -n -u $CRM_USER test -e '$ENV_FILE'"; }
fi

# oauth_credential_count: echo the live oauth_credential row count on stdout, or
# fail-closed (return non-zero + a loud, specific stderr reason) on ANY uncertainty.
# The auto path gates on this: a non-empty oauth_credential means a sync account is
# connected to staging, so the destructive --reset-and-seed (which hard-wipes
# oauth_credential) must be skipped to preserve that connection.
#
# DATABASE_URL is the SOLE authoritative source for user/dbname/host: it is the
# only guaranteed .env contract AND the exact DB the app/reset connects with.
# POSTGRES_USER/POSTGRES_DB are NEVER consulted — they are baked into the DB unit,
# are not a .env contract, and can DISAGREE with the live DB; counting an empty
# wrong DB would wipe real oauth rows. The count runs INSIDE the crm-postgres
# container over its local socket (default pg_hba `local ... trust` -> no password
# in podman argv/env, no -h/TCP), so it always counts THAT container's DB. We
# therefore require DATABASE_URL's host to BE that local target before counting,
# else the count would target a different DB than the reset wipes.
oauth_credential_count() {
    local url rest userinfo user after_at hostport host dbname count
    url="$(read_database_url || true)"
    # Strip surrounding quotes (a quoted .env value).
    url="${url%\"}"; url="${url#\"}"
    url="${url%\'}"; url="${url#\'}"
    if [ -z "$url" ]; then
        echo "staging-reset: REFUSING — DATABASE_URL not found in $ENV_FILE; cannot verify oauth_credential count" >&2
        return 1
    fi
    # Parse postgres://user[:pass]@host[:port]/dbname[?query] authoritatively.
    rest="${url#*://}"          # strip scheme
    rest="${rest%%\?*}"         # strip ?query
    userinfo="${rest%%@*}"      # user[:pass]
    user="${userinfo%%:*}"      # user
    after_at="${rest#*@}"       # host[:port]/dbname
    hostport="${after_at%%/*}"  # host[:port]
    host="${hostport%%:*}"      # host
    dbname="${after_at#*/}"     # dbname (path after the first '/')
    dbname="${dbname%%/*}"      # defensively drop any extra path segment
    if [ -z "$user" ] || [ -z "$host" ] || [ -z "$dbname" ]; then
        echo "staging-reset: REFUSING — could not parse user/host/dbname from DATABASE_URL; not reseeding" >&2
        return 1
    fi
    # The in-container count only matches the DB the reset wipes when DATABASE_URL's
    # host IS that local target. Refuse on a foreign host (we'd count the local DB
    # while the reset wipes a remote one). localhost/127.0.0.1 accepted for native.
    case "$host" in
        crm-postgres|localhost|127.0.0.1) ;;
        *)
            echo "staging-reset: REFUSING — DATABASE_URL host '$host' is not the local crm-postgres target; refusing to verify a different DB than the reset wipes" >&2
            return 1
            ;;
    esac
    # Count over the in-container local socket (trust auth). Capture the command's
    # exit status EXPLICITLY and reject any non-zero BEFORE validating the numeric
    # output — a count command that fails yet still prints "0" (partial error, psql
    # quirk) must NOT be read as verified-empty and proceed to the destructive wipe.
    # The `if cmd; then ... else rc=$?` form keeps the failure from tripping set -e
    # while preserving the real status (no `|| true` to swallow it).
    local rc trimmed
    if count="$(staging_podman exec crm-postgres psql -U "$user" -d "$dbname" -tAc "SELECT count(*) FROM oauth_credential" 2>/dev/null)"; then
        rc=0
    else
        rc=$?
    fi
    if [ "$rc" -ne 0 ]; then
        echo "staging-reset: REFUSING — oauth_credential count command failed (rc=$rc); not reseeding (host script/DB state?)" >&2
        return 1
    fi
    trimmed="$(printf '%s' "$count" | tr -d '[:space:]')"
    if [[ "$trimmed" =~ ^[0-9]+$ ]]; then
        printf '%s\n' "$trimmed"
        return 0
    fi
    echo "staging-reset: REFUSING — could not verify oauth_credential count (got '$trimmed'); not reseeding (host script/DB state?)" >&2
    return 1
}

# 1. The deployed staging .env must exist (read as the tenant; perms hold).
if ! env_file_exists; then
    echo "staging-reset: staging env file not found: $ENV_FILE" >&2
    exit 1
fi

# 2. Production refuse — FAIL-CLOSED, BEFORE installing the trap or stopping
#    anything. Strip surrounding quotes so a quoted `"production"` can't slip
#    past (stricter than the in-container SeedAllowed gate). Refuse on a
#    production alias OR empty/unset (config treats unset CRM_ENV as production).
CRM_ENV="$(read_crm_env || true)"
CRM_ENV="${CRM_ENV%\"}"; CRM_ENV="${CRM_ENV#\"}"
CRM_ENV="${CRM_ENV%\'}"; CRM_ENV="${CRM_ENV#\'}"
case "$CRM_ENV" in
    production|prod|"")
        echo "staging-reset: REFUSING — CRM_ENV='$CRM_ENV' is a production alias or empty; this is STAGING-only." >&2
        exit 1
        ;;
esac

# 3. Resolve the DEPLOYED backend image from the live unit's PINNED Image= line
#    (after the first automated deploy this is :<sha> / @sha256:<digest>, never the
#    mutable :latest), so the reseed runs the SAME image currently deployed.
IMAGE_REF="$(read_image_ref || true)"
if [ -z "$IMAGE_REF" ]; then
    echo "staging-reset: could not read a pinned Image= from $BACKEND_UNIT" >&2
    exit 1
fi

# 3b. OAuth guard (auto path only). BEFORE the trap/stop, so every branch is a true
#     no-op on the running service. --reset-and-seed hard-wipes oauth_credential, so
#     a connected sync account (non-empty oauth_credential) disables the destructive
#     path — persistence wins exactly when a sync test is in flight. Unverifiable
#     count ⇒ fail-closed (exit non-zero, loud) — never wipe on uncertainty.
if [ "$REQUIRE_OAUTH_EMPTY" = true ]; then
    if ! OAUTH_COUNT="$(oauth_credential_count)"; then
        # oauth_credential_count already logged the specific, loud refuse reason.
        exit 1
    fi
    if [ "$OAUTH_COUNT" -gt 0 ]; then
        echo "staging-reset: oauth present ($OAUTH_COUNT credential(s)); skipping auto-reseed to preserve sync connections" >&2
        exit 0
    fi
    echo "staging-reset: oauth_credential empty; proceeding with auto-reseed" >&2
fi

# 4. Install the EXIT trap that restarts the backend — BEFORE the stop. If the
#    stop itself errors/times out under set -e, the trap must already exist or
#    the script exits with staging stopped and no restart. The restart is
#    idempotent (a no-op if the service is already up). The refuse above runs
#    BEFORE this, so a refusal never installs the trap or touches any service.
restart_backend() { staging_ctl start personalcrm-backend.service; }
trap 'echo "staging-reset: restarting backend after early exit..." >&2; restart_backend || true' EXIT

# 5. Stop the backend; leave crm-postgres + the crm network up (the reseed
#    connects to the DB over the network).
echo "staging-reset: stopping personalcrm-backend.service ($CRM_USER tenant)..." >&2
staging_ctl stop personalcrm-backend.service

# 6. Reset + reseed from the ephemeral container off the deployed image. The
#    --env-file carries CRM_ENV=staging (-> SeedAllowed passes), DATABASE_URL
#    (host crm-postgres), and TIME_BASE/TIME_ACCELERATION (so seeded cadence /
#    overdue semantics track the app clock). The image ENTRYPOINT is crm-api, so
#    --entrypoint is required to run crm-admin.
echo "staging-reset: reset + reseed ('$PROFILE') from $IMAGE_REF..." >&2
staging_podman run --rm \
    --network "$PODMAN_NETWORK" \
    --env-file "$ENV_FILE" \
    -e "MIGRATIONS_PATH=$MIGRATIONS_PATH" \
    --entrypoint "$CRM_ADMIN" \
    "$IMAGE_REF" \
    --reset-and-seed --profile "$PROFILE" --yes

# 7. Start the backend, THEN clear the trap (start-before-clear): if the start
#    fails, the EXIT trap still fires its restart attempt; only a successful start
#    clears the trap.
echo "staging-reset: starting personalcrm-backend.service..." >&2
staging_ctl start personalcrm-backend.service
trap - EXIT
echo "staging-reset: done." >&2
