#!/bin/bash
# Start backend dev server detached from terminal.
#
# CRM_ENV is resolvable by the CALLER (order: caller → .env → testing) because a
# world seeded under one cadence table has to be SERVED under the same one — a
# staging-cadence world behind a testing-cadence backend renders overdue-ness
# that means something else entirely.

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Captured BEFORE sourcing .env: `set -a` + source exports every line there, so
# a CRM_ENV line in .env would otherwise silently beat the caller's value.
CALLER_CRM_ENV="${CRM_ENV:-}"

set -a
source "$PROJECT_ROOT/.env"
set +a

export DATABASE_URL="postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@localhost:${POSTGRES_PORT:-5432}/${POSTGRES_DB}?sslmode=disable"

# Override settings for dev mode
export MIGRATIONS_PATH="migrations"
export FRONTEND_URL="http://localhost:3000"
export CORS_ALLOW_ALL="true"
export CRM_ENV="${CALLER_CRM_ENV:-${CRM_ENV:-testing}}"
export NODE_ENV="development"
export GIN_MODE="debug"

cd "$PROJECT_ROOT/backend"

# Use nohup to detach - run directly without subshell to preserve env
# CRM_ENV valid values: production, prod, staging, accelerated, test, testing
# The resolved value is applied at BOTH pin sites — the export above and this
# explicit env list — or the detached process would silently fall back.
nohup env CRM_ENV="$CRM_ENV" NODE_ENV="development" GIN_MODE="debug" \
    CORS_ALLOW_ALL="true" FRONTEND_URL="http://localhost:3000" \
    MIGRATIONS_PATH="migrations" DATABASE_URL="$DATABASE_URL" \
    API_KEY="$API_KEY" SESSION_SECRET="$SESSION_SECRET" \
    go run ./cmd/crm-api > "$PROJECT_ROOT/logs/backend-dev.log" 2>&1 &
sleep 2

# Get the actual PID - try multiple patterns
ACTUAL_PID=$(pgrep -f "go run.*crm-api" | head -1)
if [ -z "$ACTUAL_PID" ]; then
    ACTUAL_PID=$(pgrep -f "crm-api" | head -1)
fi
if [ -n "$ACTUAL_PID" ]; then
    echo $ACTUAL_PID > "$PROJECT_ROOT/logs/backend-dev.pid"
    echo "Backend started with PID: $ACTUAL_PID"
else
    echo "Warning: Could not determine PID, but process may be running"
    echo "Check logs/backend-dev.log for details"
fi

