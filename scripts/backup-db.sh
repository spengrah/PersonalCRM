#!/bin/bash
# PersonalCRM Database Backup Script (rootless Podman edition).
# Creates a point-in-time physical copy of the PostgreSQL data volume on the Pi.
#
# Post-A0-cutover: the stack runs as rootless Podman Quadlets under a tenant
# system user (CRM_USER, default `crm`), so this stops the *user* systemd
# services and copies the Podman named volume (`personalcrm-db`) rather than the
# old Docker volume.
#
# The tenant user + its home are parameterized via CRM_USER / CRM_HOME so the
# same script drives prod (`crm` / `/var/lib/personalcrm`) and the staging tenant
# (`staging` / `/var/lib/staging`). Everything else is SHARED across tenants and
# stays literal: the `personalcrm-db` volume, the `personalcrm-*` services, the
# `crm-postgres` container, and the `crm_user` PG role.
#
# Usage: ./scripts/backup-db.sh [--no-restart] [--local]
#   --no-restart  Stop services for backup but don't restart (e.g., before a deploy)
#   --local       Run on the Pi as root (no ssh). The tenant podman/systemctl
#                 helpers run via `sudo -u "$CRM_USER" ...` locally instead of
#                 over ssh. Used by deploy-artifact.sh for the pre-migrate snapshot.
#
# Stops the writers + postgres for a clean copy, then restarts them. Backup is
# stored alongside the volume data as _data.bak-YYYYMMDD-HHMMSS.
#
# Default (ssh) mode is unchanged: all progress goes to stdout. In --local mode
# all progress goes to stderr and the ONLY stdout line is BACKUP_PATH=<path>, so
# a caller (deploy-artifact.sh) can capture the snapshot location cleanly.

set -e

PI_HOST="${PI_HOST:-raspberry-pi}"
TIMESTAMP=$(date +%Y%m%d-%H%M%S)

# Parse arguments
NO_RESTART=false
LOCAL=false
for arg in "$@"; do
    case $arg in
        --no-restart) NO_RESTART=true ;;
        --local) LOCAL=true ;;
    esac
done

# Tenant identity (overridable for staging; defaults are prod).
CRM_USER="${CRM_USER:-crm}"
CRM_HOME="${CRM_HOME:-/var/lib/personalcrm}"

# log: progress messages. ssh mode → stdout (unchanged behavior); local mode →
# stderr (so stdout carries only the captured BACKUP_PATH line).
if [ "$LOCAL" = true ]; then
    log() { echo "$@" >&2; }
else
    log() { echo "$@"; }
fi

if [ "$LOCAL" = true ]; then
    # On-Pi mode: no ssh. Resolve the tenant uid locally and wrap podman/systemctl
    # in `sudo -u "$CRM_USER"` so they hit the rootless tenant store, never root's.
    # The `cd /tmp` mirrors the ssh form: an interactive `sudo -u "$CRM_USER"`
    # inherits the caller's CWD, which the tenant can't access (rootless podman
    # fails to chdir).
    CRM_UID=$(id -u "$CRM_USER")
    USERENV="HOME=$CRM_HOME XDG_RUNTIME_DIR=/run/user/$CRM_UID DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$CRM_UID/bus"
    # USERENV is deliberately unquoted: it must word-split into separate KEY=val
    # arguments for env-style `sudo -u "$CRM_USER" KEY=val KEY=val ...`.
    # shellcheck disable=SC2086
    crm_ctl()    { cd /tmp && sudo -u "$CRM_USER" $USERENV systemctl --user "$@"; }
    crm_podman() { cd /tmp && sudo -u "$CRM_USER" HOME=$CRM_HOME XDG_RUNTIME_DIR=/run/user/"$CRM_UID" podman "$@"; }
    root_cp()    { sudo cp -a "$1" "$2"; }
    root_du()    { sudo du -sh "$1" | cut -f1; }

    log "=== PersonalCRM Database Backup (Podman, local) ==="
else
    log "=== PersonalCRM Database Backup (Podman) ==="
    log "Target: $PI_HOST"
    log ""

    # Verify Pi is reachable
    log "Checking connectivity to $PI_HOST..."
    if ! ssh -q -o ConnectTimeout=5 "$PI_HOST" exit; then
        echo "Error: Cannot connect to $PI_HOST"
        exit 1
    fi

    # Resolve the tenant user context for rootless systemctl --user / podman.
    # `cd /tmp` + explicit HOME: interactive `sudo -u "$CRM_USER"` inherits the
    # SSH user's CWD/HOME, which the tenant can't access (rootless podman then
    # fails to chdir). The Quadlet services themselves run under systemd and are
    # unaffected.
    CRM_UID=$(ssh "$PI_HOST" "id -u $CRM_USER")
    USERENV="HOME=$CRM_HOME XDG_RUNTIME_DIR=/run/user/$CRM_UID DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$CRM_UID/bus"
    # Vars below deliberately expand client-side (resolved here, then sent over
    # ssh as a literal remote command). SC2029 is the intended behavior.
    # shellcheck disable=SC2029
    crm_ctl()    { ssh "$PI_HOST" "cd /tmp && sudo -n -u $CRM_USER $USERENV systemctl --user $*"; }
    # shellcheck disable=SC2029
    crm_podman() { ssh "$PI_HOST" "cd /tmp && sudo -n -u $CRM_USER HOME=$CRM_HOME XDG_RUNTIME_DIR=/run/user/$CRM_UID podman $*"; }
    # shellcheck disable=SC2029
    root_cp()    { ssh "$PI_HOST" "sudo cp -a '$1' '$2'"; }
    # shellcheck disable=SC2029
    root_du()    { ssh "$PI_HOST" "sudo du -sh '$1' | cut -f1"; }
fi

# Safety net: if we error out (set -e) after stopping services, don't leave prod
# down silently -- try to restart, and print an explicit manual-recovery command.
SERVICES_STOPPED=false
on_exit() {
    local rc=$?
    if [ "$rc" -ne 0 ] && [ "$SERVICES_STOPPED" = true ]; then
        echo "" >&2
        echo "⚠️  ERROR (exit $rc) with services stopped — attempting restart..." >&2
        if ! crm_ctl start personalcrm-database.service personalcrm-backend.service personalcrm-frontend.service; then
            echo "   AUTO-RESTART FAILED. Restart manually:" >&2
            if [ "$LOCAL" = true ]; then
                echo "   sudo -u $CRM_USER $USERENV systemctl --user start personalcrm-database.service personalcrm-backend.service personalcrm-frontend.service" >&2
            else
                echo "   ssh $PI_HOST \"cd /tmp && sudo -n -u $CRM_USER $USERENV systemctl --user start personalcrm-database.service personalcrm-backend.service personalcrm-frontend.service\"" >&2
            fi
        fi
    fi
}
trap on_exit EXIT

# Locate the Podman named volume mountpoint (.../volumes/personalcrm-db/_data).
VOLUME_PATH=$(crm_podman volume inspect personalcrm-db --format '{{.Mountpoint}}')
BACKUP_PATH="${VOLUME_PATH}.bak-${TIMESTAMP}"
log "Volume: $VOLUME_PATH"
log "Backup: $BACKUP_PATH"
[ "$LOCAL" = true ] || log ""

# Stop the writers, then postgres, to guarantee a consistent on-disk copy.
log "Stopping app services (backend, frontend)..."
crm_ctl stop personalcrm-backend.service personalcrm-frontend.service
SERVICES_STOPPED=true

log "Stopping postgres..."
crm_ctl stop personalcrm-database.service

# Copy the volume (owned by a mapped subuid; root can read it).
log "Copying data volume (this may take a moment)..."
root_cp "$VOLUME_PATH" "$BACKUP_PATH"

BACKUP_SIZE=$(root_du "$BACKUP_PATH")
log "Backup created: $BACKUP_PATH ($BACKUP_SIZE)"

if [ "$NO_RESTART" = true ]; then
    log ""
    log "⚠️  --no-restart specified. Services are stopped."
    log "   Restart manually with:"
    if [ "$LOCAL" = true ]; then
        log "   sudo -u $CRM_USER $USERENV systemctl --user start personalcrm-database.service personalcrm-backend.service personalcrm-frontend.service"
    else
        log "   ssh $PI_HOST \"sudo -n -u $CRM_USER $USERENV systemctl --user start personalcrm-database.service personalcrm-backend.service personalcrm-frontend.service\""
    fi
else
    log "Restarting postgres..."
    crm_ctl start personalcrm-database.service

    log "Waiting for postgres to accept connections..."
    for i in $(seq 1 15); do
        if crm_podman exec crm-postgres pg_isready -U crm_user >/dev/null 2>&1; then
            break
        fi
        if [ "$i" -eq 15 ]; then
            log "Warning: postgres not ready after 15s, starting app anyway"
        fi
        sleep 1
    done

    log "Restarting backend and frontend..."
    crm_ctl start personalcrm-backend.service personalcrm-frontend.service
    SERVICES_STOPPED=false
    log ""
    log "✅ Backup complete. Services restarted."
fi

# In --local mode, emit the snapshot path on stdout (the ONLY stdout line) so a
# caller can capture it. In ssh mode the path is already in the human log above.
if [ "$LOCAL" = true ]; then
    echo "BACKUP_PATH=$BACKUP_PATH"
fi
