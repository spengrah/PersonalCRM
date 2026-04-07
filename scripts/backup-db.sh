#!/bin/bash
# PersonalCRM Database Backup Script
# Creates a point-in-time copy of the PostgreSQL data volume on the Pi.
#
# Usage: ./scripts/backup-db.sh [--no-restart]
#
# Options:
#   --no-restart  Stop services for backup but don't restart (e.g., before a deploy)
#
# The script stops postgres to ensure a clean copy, then restarts it.
# Backup is stored alongside the volume as _data.bak-YYYYMMDD-HHMMSS.

set -e

PI_HOST="${PI_HOST:-raspberry-pi}"
PI_DEPLOY_DIR="${PI_DEPLOY_DIR:?PI_DEPLOY_DIR must be set (e.g. /srv/personalcrm)}"
VOLUME_PATH="/var/lib/docker/volumes/infra_postgres_data/_data"
COMPOSE_DIR="$PI_DEPLOY_DIR/infra"
BACKEND_SERVICE="personalcrm-backend"
FRONTEND_SERVICE="personalcrm-frontend"
TIMESTAMP=$(date +%Y%m%d-%H%M%S)
BACKUP_PATH="${VOLUME_PATH}.bak-${TIMESTAMP}"

# Parse arguments
NO_RESTART=false
for arg in "$@"; do
    case $arg in
        --no-restart)
            NO_RESTART=true
            shift
            ;;
    esac
done

echo "=== PersonalCRM Database Backup ==="
echo "Target: $PI_HOST"
echo "Backup: $BACKUP_PATH"
echo ""

# Verify Pi is reachable
echo "Checking connectivity to $PI_HOST..."
if ! ssh -q -o ConnectTimeout=5 "$PI_HOST" exit; then
    echo "Error: Cannot connect to $PI_HOST"
    exit 1
fi

# Stop services to prevent writes
echo "Stopping services..."
ssh "$PI_HOST" "sudo systemctl stop $BACKEND_SERVICE $FRONTEND_SERVICE"

# Stop postgres for clean copy
echo "Stopping postgres container..."
ssh "$PI_HOST" "cd $COMPOSE_DIR && docker compose stop postgres"

# Copy the volume
echo "Copying data volume (this may take a moment)..."
ssh "$PI_HOST" "sudo cp -a $VOLUME_PATH $BACKUP_PATH"

# Verify
BACKUP_SIZE=$(ssh "$PI_HOST" "sudo du -sh $BACKUP_PATH | cut -f1")
echo "Backup created: $BACKUP_PATH ($BACKUP_SIZE)"

if [ "$NO_RESTART" = true ]; then
    echo ""
    echo "⚠️  --no-restart specified. Services are stopped."
    echo "   Restart manually with:"
    echo "   ssh $PI_HOST \"cd $COMPOSE_DIR && docker compose start postgres && sudo systemctl start $BACKEND_SERVICE $FRONTEND_SERVICE\""
else
    # Restart services
    echo "Restarting postgres..."
    ssh "$PI_HOST" "cd $COMPOSE_DIR && docker compose start postgres"

    # Wait for postgres to be ready
    echo "Waiting for postgres to accept connections..."
    for i in $(seq 1 15); do
        if ssh "$PI_HOST" "docker exec crm-postgres pg_isready -U crm_user" >/dev/null 2>&1; then
            break
        fi
        if [ "$i" -eq 15 ]; then
            echo "Warning: postgres not ready after 15s, starting backend anyway"
        fi
        sleep 1
    done

    echo "Restarting backend and frontend..."
    ssh "$PI_HOST" "sudo systemctl start $BACKEND_SERVICE $FRONTEND_SERVICE"
    echo ""
    echo "✅ Backup complete. Services restarted."
fi
