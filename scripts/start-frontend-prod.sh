#!/bin/bash
# Start production frontend server detached from terminal

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$PROJECT_ROOT"

PORT=3001
PID_FILE="logs/frontend.pid"
LOG_FILE="logs/frontend.log"

# Kill any existing process on the port
echo "Checking for existing frontend process..."
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

# Get expected BUILD_ID for verification
BUILD_ID=$(cat frontend/.next/BUILD_ID 2>/dev/null || echo "unknown")
echo "Starting frontend with BUILD_ID: $BUILD_ID"

# Start the frontend
cd frontend
(nohup sh -c "PORT=$PORT bun run start" > "../$LOG_FILE" 2>&1 &)
cd ..

# Wait for startup and verify
sleep 3

# Verify the process is running
NEW_PID=$(lsof -ti:$PORT 2>/dev/null || true)
if [ -z "$NEW_PID" ]; then
    echo "❌ Frontend failed to start! Check $LOG_FILE"
    exit 1
fi

echo "$NEW_PID" > "$PID_FILE"

# Verify correct build is being served
SERVED_BUILD=$(curl -s "http://localhost:$PORT" 2>/dev/null | grep -o '"b":"[^"]*"' | cut -d'"' -f4 || true)
if [ -n "$SERVED_BUILD" ] && [ "$SERVED_BUILD" != "$BUILD_ID" ]; then
    echo "⚠️  Warning: Served BUILD_ID ($SERVED_BUILD) doesn't match expected ($BUILD_ID)"
fi

echo "✅ Frontend started with PID: $NEW_PID (BUILD_ID: $BUILD_ID)"
