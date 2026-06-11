#!/bin/bash
# PersonalCRM Database Restore Script (rootless Podman edition).
# Replaces the live PostgreSQL data volume with a previously-taken physical
# snapshot (a `_data.bak-*` directory produced by backup-db.sh).
#
# Mirror of backup-db.sh: same rootless crm-user helpers, same ssh/local split.
# The cold copy requires postgres stopped, so this stops the writers + postgres,
# swaps the volume contents, then brings postgres (and optionally the app) back.
#
# Usage: ./scripts/restore-db.sh [--local] [--no-app-start] [<snapshot-path>]
#   --local         Run on the Pi as root (no ssh). crm-user podman/systemctl
#                   helpers run via `sudo -u crm ...` locally instead of over ssh.
#   --no-app-start  Restore the volume and bring Postgres up to pg_isready, but
#                   do NOT start backend/frontend. Used by deploy-artifact.sh's
#                   rollback path so it can re-pin the OLD Image= itself, then
#                   start the app on the OLD code against the restored OLD DB.
#   <snapshot-path> The exact `_data.bak-*` path to restore. If omitted, the
#                   newest `*.bak-*` alongside the live volume is used.
#
# The snapshot is NEVER deleted (it is the recovery point). The displaced live
# `_data` is moved aside (not removed) until the copy succeeds, then cleaned up.

set -e

PI_HOST="${PI_HOST:-raspberry-pi}"

# Parse arguments
LOCAL=false
NO_APP_START=false
SNAPSHOT_ARG=""
for arg in "$@"; do
    case $arg in
        --local) LOCAL=true ;;
        --no-app-start) NO_APP_START=true ;;
        --*) echo "Unknown flag: $arg" >&2; exit 2 ;;
        *) SNAPSHOT_ARG="$arg" ;;
    esac
done

CRM_HOME=/var/lib/personalcrm

# log: progress messages. ssh mode → stdout; local mode → stderr (so stdout stays
# clean for any future machine-readable output and matches backup-db.sh's split).
if [ "$LOCAL" = true ]; then
    log() { echo "$@" >&2; }
else
    log() { echo "$@"; }
fi

if [ "$LOCAL" = true ]; then
    # On-Pi mode: no ssh. Resolve the crm uid locally and wrap podman/systemctl
    # in `sudo -u crm` so they hit the rootless crm-user store, never root's.
    CRM_UID=$(id -u crm)
    USERENV="HOME=$CRM_HOME XDG_RUNTIME_DIR=/run/user/$CRM_UID DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$CRM_UID/bus"
    # USERENV is deliberately unquoted: it must word-split into separate KEY=val
    # arguments for env-style `sudo -u crm KEY=val KEY=val ...`.
    # shellcheck disable=SC2086
    crm_ctl()    { cd /tmp && sudo -u crm $USERENV systemctl --user "$@"; }
    crm_podman() { cd /tmp && sudo -u crm HOME=$CRM_HOME XDG_RUNTIME_DIR=/run/user/"$CRM_UID" podman "$@"; }
    root_mv()    { sudo mv "$1" "$2"; }
    root_cp()    { sudo cp -a "$1" "$2"; }
    root_rm()    { sudo rm -rf "$1"; }
    root_exists(){ sudo test -e "$1"; }

    log "=== PersonalCRM Database Restore (Podman, local) ==="
else
    log "=== PersonalCRM Database Restore (Podman) ==="
    log "Target: $PI_HOST"
    log ""

    # Verify Pi is reachable
    log "Checking connectivity to $PI_HOST..."
    if ! ssh -q -o ConnectTimeout=5 "$PI_HOST" exit; then
        echo "Error: Cannot connect to $PI_HOST"
        exit 1
    fi

    CRM_UID=$(ssh "$PI_HOST" "id -u crm")
    USERENV="HOME=$CRM_HOME XDG_RUNTIME_DIR=/run/user/$CRM_UID DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$CRM_UID/bus"
    # Vars below deliberately expand client-side (resolved here, then sent over
    # ssh as a literal remote command). SC2029 is the intended behavior.
    # shellcheck disable=SC2029
    crm_ctl()    { ssh "$PI_HOST" "cd /tmp && sudo -n -u crm $USERENV systemctl --user $*"; }
    # shellcheck disable=SC2029
    crm_podman() { ssh "$PI_HOST" "cd /tmp && sudo -n -u crm HOME=$CRM_HOME XDG_RUNTIME_DIR=/run/user/$CRM_UID podman $*"; }
    # shellcheck disable=SC2029
    root_mv()    { ssh "$PI_HOST" "sudo mv '$1' '$2'"; }
    # shellcheck disable=SC2029
    root_cp()    { ssh "$PI_HOST" "sudo cp -a '$1' '$2'"; }
    # shellcheck disable=SC2029
    root_rm()    { ssh "$PI_HOST" "sudo rm -rf '$1'"; }
    # shellcheck disable=SC2029
    root_exists(){ ssh "$PI_HOST" "sudo test -e '$1'"; }
fi

# Safety net: if we error out (set -e) after stopping services, don't leave prod
# down silently. Best-effort: bring the DB back so a manual recovery can proceed.
# We do NOT auto-restart the app here -- mid-restore the app image may be wrong
# (the caller, deploy-artifact.sh, owns the app re-pin + start decision).
SERVICES_STOPPED=false
on_exit() {
    local rc=$?
    if [ "$rc" -ne 0 ] && [ "$SERVICES_STOPPED" = true ]; then
        echo "" >&2
        echo "⚠️  ERROR (exit $rc) during restore with services stopped." >&2
        echo "   The snapshot is preserved. Inspect and recover manually." >&2
        echo "   Bring the database back with:" >&2
        if [ "$LOCAL" = true ]; then
            echo "   sudo -u crm $USERENV systemctl --user start personalcrm-database.service" >&2
        else
            echo "   ssh $PI_HOST \"sudo -n -u crm $USERENV systemctl --user start personalcrm-database.service\"" >&2
        fi
    fi
}
trap on_exit EXIT

# Locate the Podman named volume mountpoint (.../volumes/personalcrm-db/_data).
VOLUME_PATH=$(crm_podman volume inspect personalcrm-db --format '{{.Mountpoint}}')
log "Volume: $VOLUME_PATH"

# Resolve the snapshot to restore.
if [ -n "$SNAPSHOT_ARG" ]; then
    SNAPSHOT_PATH="$SNAPSHOT_ARG"
else
    # Newest *.bak-* alongside the volume. ls -d sorts the timestamped suffix
    # lexically, which is chronological for YYYYMMDD-HHMMSS.
    if [ "$LOCAL" = true ]; then
        SNAPSHOT_PATH=$(sudo bash -c "ls -d ${VOLUME_PATH}.bak-* 2>/dev/null | sort | tail -1" || true)
    else
        # shellcheck disable=SC2029
        SNAPSHOT_PATH=$(ssh "$PI_HOST" "sudo bash -c 'ls -d ${VOLUME_PATH}.bak-* 2>/dev/null | sort | tail -1'" || true)
    fi
fi

# REQUIRE the snapshot exists -- NEVER restore from nothing.
if [ -z "$SNAPSHOT_PATH" ]; then
    echo "Error: no snapshot specified and no ${VOLUME_PATH}.bak-* found" >&2
    exit 1
fi
if ! root_exists "$SNAPSHOT_PATH"; then
    echo "Error: snapshot does not exist: $SNAPSHOT_PATH" >&2
    exit 1
fi
log "Snapshot: $SNAPSHOT_PATH"

# Stop the writers, then postgres, before touching the volume on disk.
log "Stopping app services (backend, frontend)..."
crm_ctl stop personalcrm-backend.service personalcrm-frontend.service
SERVICES_STOPPED=true

log "Stopping postgres..."
crm_ctl stop personalcrm-database.service

# Replace the live _data with the snapshot. mv the live dir aside (do NOT rm) so
# it survives until the cp -a verifiably succeeds; clean it up only on success.
DISPLACED="${VOLUME_PATH}.restore-old-$(date +%Y%m%d-%H%M%S)"
log "Moving live data aside: $DISPLACED"
root_mv "$VOLUME_PATH" "$DISPLACED"

log "Copying snapshot into place (this may take a moment)..."
root_cp "$SNAPSHOT_PATH" "$VOLUME_PATH"

# Bring postgres back and wait for it to accept connections.
log "Starting postgres..."
crm_ctl start personalcrm-database.service

log "Waiting for postgres to accept connections..."
PG_READY=false
for _ in $(seq 1 15); do
    if crm_podman exec crm-postgres pg_isready -U crm_user >/dev/null 2>&1; then
        PG_READY=true
        break
    fi
    sleep 1
done
# A restore that cannot bring Postgres ready is a FAILED restore -- the caller
# (deploy-artifact.sh) must treat this as ROLLBACK FAILED, not a clean recovery.
# The displaced live dir is intentionally retained (not cleaned up) for forensics.
if [ "$PG_READY" != true ]; then
    echo "Error: postgres did not accept connections after 15s; restore failed" >&2
    echo "   Displaced live data retained at: $DISPLACED" >&2
    exit 1
fi

# The copy succeeded and postgres is back -- clean up the displaced live dir.
# (The SNAPSHOT is never touched; it remains the recovery point.)
log "Cleaning up displaced data: $DISPLACED"
root_rm "$DISPLACED"

if [ "$NO_APP_START" = true ]; then
    SERVICES_STOPPED=false
    log ""
    log "✅ Restore complete. Postgres is up; app NOT started (--no-app-start)."
    log "   The caller is responsible for pinning the image and starting the app."
else
    log "Starting backend and frontend..."
    crm_ctl start personalcrm-backend.service personalcrm-frontend.service
    SERVICES_STOPPED=false
    log ""
    log "✅ Restore complete. Services restarted on the currently-pinned image."
fi
