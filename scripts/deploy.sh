#!/bin/bash
# PersonalCRM Deploy Script
# Builds locally and deploys to Raspberry Pi via rsync
#
# Usage: ./scripts/deploy.sh [--skip-build]
#
# Prerequisites:
# - Run 'make setup-pi' first (one-time)
# - Production secrets configured on Pi at /srv/personalcrm/.env

set -e

PI_HOST="${PI_HOST:-raspberry-pi}"
PI_DIR="/srv/personalcrm"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

# Parse arguments
SKIP_BUILD=false
for arg in "$@"; do
    case $arg in
        --skip-build)
            SKIP_BUILD=true
            shift
            ;;
    esac
done

cd "$PROJECT_DIR"

echo "=== PersonalCRM Deploy ==="
echo "Target: $PI_HOST:$PI_DIR"
echo ""

# Verify Pi is reachable
echo "Checking connectivity to $PI_HOST..."
if ! ssh -q -o ConnectTimeout=5 "$PI_HOST" exit; then
    echo "Error: Cannot connect to $PI_HOST"
    echo "Ensure the Pi is on your Tailnet and SSH is configured."
    exit 1
fi
echo "OK"
echo ""

# Build for ARM64
if [ "$SKIP_BUILD" = false ]; then
    echo "=== Building for ARM64 ==="

    # Fetch production env vars from Pi for frontend build
    echo "Fetching production config from $PI_HOST..."
    API_KEY=$(ssh "$PI_HOST" 'sudo grep "^API_KEY=" /srv/personalcrm/.env | cut -d= -f2')
    API_URL=$(ssh "$PI_HOST" 'sudo grep "^NEXT_PUBLIC_API_URL=" /srv/personalcrm/.env | cut -d= -f2')
    TIME_TRACKING=$(ssh "$PI_HOST" 'sudo grep "^NEXT_PUBLIC_ENABLE_TIME_TRACKING=" /srv/personalcrm/.env | cut -d= -f2')

    if [ -z "$API_KEY" ]; then
        echo "Error: Could not fetch API_KEY from $PI_HOST"
        echo "Ensure /srv/personalcrm/.env exists and contains API_KEY"
        exit 1
    fi

    # Build with production values injected
    # NEXT_PUBLIC_API_URL defaults to empty for same-origin requests (works with Tailscale Serve)
    NEXT_PUBLIC_API_KEY="$API_KEY" \
    NEXT_PUBLIC_API_URL="${API_URL:-}" \
    NEXT_PUBLIC_ENABLE_TIME_TRACKING="${TIME_TRACKING:-false}" \
    make ci-build
    echo ""
fi

echo "=== Deploying to $PI_HOST ==="

# rsync flags: -rltz (recursive, links, times, compress) + --no-perms to avoid permission errors
RSYNC_OPTS="-rltvz --omit-dir-times --no-perms"

# Backend binary
echo "Deploying backend binary..."
rsync $RSYNC_OPTS --progress backend/bin/crm-api "$PI_HOST:$PI_DIR/backend/bin/"

# Migrations
echo "Deploying migrations..."
rsync $RSYNC_OPTS --delete backend/migrations/ "$PI_HOST:$PI_DIR/backend/migrations/"

# Frontend (standalone build - no node_modules needed)
echo "Deploying frontend (standalone)..."
rsync $RSYNC_OPTS --delete frontend/.next/standalone/ "$PI_HOST:$PI_DIR/frontend/"
rsync $RSYNC_OPTS --delete frontend/.next/static/ "$PI_HOST:$PI_DIR/frontend/.next/static/"
rsync $RSYNC_OPTS --delete frontend/public/ "$PI_HOST:$PI_DIR/frontend/public/"

# Infrastructure
echo "Deploying infrastructure files..."
rsync $RSYNC_OPTS infra/docker-compose.yml infra/init-db.sql "$PI_HOST:$PI_DIR/infra/"

# Systemd services (copy to temp, then sudo install)
echo "Deploying systemd service files..."
rsync -avz infra/*.service infra/*.target "$PI_HOST:/tmp/"
ssh "$PI_HOST" 'sudo cp /tmp/personalcrm*.service /tmp/personalcrm*.target /etc/systemd/system/ && sudo systemctl daemon-reload'

echo ""
echo "=== Restarting services ==="
ssh "$PI_HOST" 'sudo systemctl restart personalcrm.target'

echo ""
echo "=== Verifying deployment ==="
echo "Waiting for services to start..."
sleep 5

# Health checks
BACKEND_OK=false
FRONTEND_OK=false

if ssh "$PI_HOST" 'curl -sf http://127.0.0.1:8080/health' > /dev/null 2>&1; then
    echo "Backend:  OK"
    BACKEND_OK=true
else
    echo "Backend:  FAILED"
fi

if ssh "$PI_HOST" 'curl -sf http://127.0.0.1:3001' > /dev/null 2>&1; then
    echo "Frontend: OK"
    FRONTEND_OK=true
else
    echo "Frontend: FAILED"
fi

echo ""

if [ "$BACKEND_OK" = true ] && [ "$FRONTEND_OK" = true ]; then
    echo "=== Deploy complete ==="
    echo ""
    echo "Access your CRM at: http://$PI_HOST:3001"
else
    echo "=== Deploy completed with warnings ==="
    echo "Some services may not have started correctly."
    echo ""
    echo "Check logs:"
    echo "  ssh $PI_HOST 'sudo journalctl -u personalcrm-backend -n 50'"
    echo "  ssh $PI_HOST 'sudo journalctl -u personalcrm-frontend -n 50'"
    exit 1
fi
