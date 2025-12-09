#!/bin/bash
# Start backend server detached from terminal

set -a
source ./.env
set +a

export DATABASE_URL="postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@localhost:${POSTGRES_PORT:-5432}/${POSTGRES_DB}?sslmode=disable"

cd backend
# Use nohup and immediately exit the subshell to detach
(nohup go run cmd/crm-api/main.go > ../logs/backend-dev.log 2>&1 &)
sleep 2
# Get the actual PID - try multiple patterns
ACTUAL_PID=$(pgrep -f "go run.*crm-api" | head -1)
if [ -z "$ACTUAL_PID" ]; then
    ACTUAL_PID=$(pgrep -f "crm-api" | head -1)
fi
if [ -n "$ACTUAL_PID" ]; then
    echo $ACTUAL_PID > ../logs/backend-dev.pid
    echo "Backend started with PID: $ACTUAL_PID"
else
    echo "Warning: Could not determine PID, but process may be running"
    echo "Check logs/backend-dev.log for details"
fi

