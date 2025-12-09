#!/bin/bash
# Start production backend server detached from terminal

set -a
source ./.env
set +a

export DATABASE_URL="postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@localhost:${POSTGRES_PORT:-5432}/${POSTGRES_DB}?sslmode=disable"

# Use nohup in a subshell that exits immediately
(nohup ./backend/bin/crm-api > logs/backend.log 2>&1 &)
sleep 1
# Get the actual PID
ACTUAL_PID=$(pgrep -f "./backend/bin/crm-api" | head -1)
if [ -n "$ACTUAL_PID" ]; then
    echo $ACTUAL_PID > logs/backend.pid
    echo "Backend started with PID: $ACTUAL_PID"
else
    echo "Warning: Could not determine PID"
fi

