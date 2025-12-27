#!/bin/bash
# Start production backend server detached from terminal

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$PROJECT_ROOT"

PORT=8080
PID_FILE="logs/backend.pid"
LOG_FILE="logs/backend.log"
BINARY="./backend/bin/crm-api"

# Load environment
set -a
source ./.env
set +a

export DATABASE_URL="postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@localhost:${POSTGRES_PORT:-5432}/${POSTGRES_DB}?sslmode=disable"

# Kill any existing process on the port
echo "Checking for existing backend process..."
EXISTING_PID=$(lsof -ti:$PORT 2>/dev/null || true)
if [ -n "$EXISTING_PID" ]; then
    echo "Killing existing process on port $PORT (PID: $EXISTING_PID)"
    kill -9 $EXISTING_PID 2>/dev/null || true
    sleep 1
fi

# Also kill any process from PID file
if [ -f "$PID_FILE" ]; then
    OLD_PID=$(cat "$PID_FILE" 2>/dev/null || true)
    if [ -n "$OLD_PID" ] && kill -0 "$OLD_PID" 2>/dev/null; then
        echo "Killing process from PID file (PID: $OLD_PID)"
        kill -9 "$OLD_PID" 2>/dev/null || true
        sleep 1
    fi
    rm -f "$PID_FILE"
fi

# Also kill any crm-api processes that might be orphaned
pkill -9 -f "crm-api" 2>/dev/null || true
sleep 1

# Verify binary exists
if [ ! -f "$BINARY" ]; then
    echo "❌ Backend binary not found at $BINARY"
    echo "   Run 'make build' first"
    exit 1
fi

# Get binary build time for verification
BINARY_TIME=$(stat -f "%Sm" -t "%Y-%m-%d %H:%M:%S" "$BINARY" 2>/dev/null || stat -c "%y" "$BINARY" 2>/dev/null | cut -d'.' -f1 || echo "unknown")
echo "Starting backend (binary built: $BINARY_TIME)"

# Start the backend
(nohup $BINARY > "$LOG_FILE" 2>&1 &)

# Wait for startup
sleep 2

# Verify the process is running and healthy
NEW_PID=$(lsof -ti:$PORT 2>/dev/null || true)
if [ -z "$NEW_PID" ]; then
    echo "❌ Backend failed to start! Check $LOG_FILE"
    tail -20 "$LOG_FILE"
    exit 1
fi

echo "$NEW_PID" > "$PID_FILE"

# Health check
HEALTH=$(curl -s "http://localhost:$PORT/health" 2>/dev/null || echo "")
if echo "$HEALTH" | grep -q '"status":"healthy"'; then
    echo "✅ Backend started with PID: $NEW_PID (healthy)"
else
    echo "⚠️  Backend started with PID: $NEW_PID but health check inconclusive"
fi
