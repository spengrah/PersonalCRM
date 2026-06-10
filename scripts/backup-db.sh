#!/bin/bash
# PersonalCRM Database Backup Script (rootless Podman edition).
# Creates a point-in-time physical copy of the PostgreSQL data volume on the Pi.
#
# Post-A0-cutover: the stack runs as rootless Podman Quadlets under the `crm`
# system user, so this stops the *user* systemd services and copies the Podman
# named volume (`personalcrm-db`) rather than the old Docker volume.
#
# Usage: ./scripts/backup-db.sh [--no-restart]
#   --no-restart  Stop services for backup but don't restart (e.g., before a deploy)
#
# Stops the writers + postgres for a clean copy, then restarts them. Backup is
# stored alongside the volume data as _data.bak-YYYYMMDD-HHMMSS.

set -e

PI_HOST="${PI_HOST:-raspberry-pi}"
TIMESTAMP=$(date +%Y%m%d-%H%M%S)

# Parse arguments
NO_RESTART=false
for arg in "$@"; do
    case $arg in
        --no-restart) NO_RESTART=true; shift ;;
    esac
done

echo "=== PersonalCRM Database Backup (Podman) ==="
echo "Target: $PI_HOST"
echo ""

# Verify Pi is reachable
echo "Checking connectivity to $PI_HOST..."
if ! ssh -q -o ConnectTimeout=5 "$PI_HOST" exit; then
    echo "Error: Cannot connect to $PI_HOST"
    exit 1
fi

# Resolve the crm user context for rootless systemctl --user / podman.
CRM_UID=$(ssh "$PI_HOST" "id -u crm")
USERENV="XDG_RUNTIME_DIR=/run/user/$CRM_UID DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/$CRM_UID/bus"
crm_ctl()    { ssh "$PI_HOST" "sudo -n -u crm $USERENV systemctl --user $*"; }
crm_podman() { ssh "$PI_HOST" "sudo -n -u crm XDG_RUNTIME_DIR=/run/user/$CRM_UID podman $*"; }

# Locate the Podman named volume mountpoint (.../volumes/personalcrm-db/_data).
VOLUME_PATH=$(crm_podman volume inspect personalcrm-db --format '{{.Mountpoint}}')
BACKUP_PATH="${VOLUME_PATH}.bak-${TIMESTAMP}"
echo "Volume: $VOLUME_PATH"
echo "Backup: $BACKUP_PATH"
echo ""

# Stop the writers, then postgres, to guarantee a consistent on-disk copy.
echo "Stopping app services (backend, frontend)..."
crm_ctl stop personalcrm-backend.service personalcrm-frontend.service

echo "Stopping postgres..."
crm_ctl stop personalcrm-database.service

# Copy the volume (owned by a mapped subuid; root can read it).
echo "Copying data volume (this may take a moment)..."
ssh "$PI_HOST" "sudo cp -a '$VOLUME_PATH' '$BACKUP_PATH'"

BACKUP_SIZE=$(ssh "$PI_HOST" "sudo du -sh '$BACKUP_PATH' | cut -f1")
echo "Backup created: $BACKUP_PATH ($BACKUP_SIZE)"

if [ "$NO_RESTART" = true ]; then
    echo ""
    echo "⚠️  --no-restart specified. Services are stopped."
    echo "   Restart manually with:"
    echo "   ssh $PI_HOST \"sudo -n -u crm $USERENV systemctl --user start personalcrm-database.service personalcrm-backend.service personalcrm-frontend.service\""
else
    echo "Restarting postgres..."
    crm_ctl start personalcrm-database.service

    echo "Waiting for postgres to accept connections..."
    for i in $(seq 1 15); do
        if crm_podman exec crm-postgres pg_isready -U crm_user >/dev/null 2>&1; then
            break
        fi
        if [ "$i" -eq 15 ]; then
            echo "Warning: postgres not ready after 15s, starting app anyway"
        fi
        sleep 1
    done

    echo "Restarting backend and frontend..."
    crm_ctl start personalcrm-backend.service personalcrm-frontend.service
    echo ""
    echo "✅ Backup complete. Services restarted."
fi
