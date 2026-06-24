#!/bin/bash
# HARD reset + reseed a STAGING instance to the prod-shaped synthetic world.
#
# Stops the backend service, sources the staging env (so DATABASE_URL / CRM_ENV /
# TIME_BASE / TIME_ACCELERATION / MIGRATIONS_PATH come along — the seed's
# cadence/overdue/time semantics then track the running app's clock), refuses if
# CRM_ENV is a production alias (defense-in-depth before crm-admin's own PRE-DB
# gate), runs crm-admin --reset-and-seed, and restarts the service. The
# reset-and-seed itself hard-wipes every live data table (incl. oauth_credential
# + sync-state); the migration-seeded curated catalog (predicate/entity_type)
# survives, only its provisional rows are cleared — STAGING ONLY; never point
# this at production.
#
# The QA harness (#380) either calls this script or replicates the
# stop -> reset -> start sequence over SSH (the documented crm-admin operator
# pattern). Service-stopped is enforced HERE (the stop step) + crm-admin's
# mandatory --yes.

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Env file: STAGING_ENV_FILE override, else the project .env.
ENV_FILE="${STAGING_ENV_FILE:-$PROJECT_ROOT/.env}"
if [ ! -f "$ENV_FILE" ]; then
    echo "staging-reset: env file not found: $ENV_FILE" >&2
    exit 1
fi

set -a
# shellcheck disable=SC1090
source "$ENV_FILE"
set +a

# Defense-in-depth: refuse production BEFORE building/running anything
# (crm-admin's PRE-DB SeedAllowed gate is the authoritative guard; this is the
# shell-layer backstop).
case "${CRM_ENV:-}" in
    production|prod)
        echo "staging-reset: refusing — CRM_ENV is '$CRM_ENV' (production). This script is STAGING-only." >&2
        exit 1
        ;;
esac

# Compute DATABASE_URL from components if the env file does not define it
# directly (mirrors start-backend.sh).
if [ -z "${DATABASE_URL:-}" ]; then
    export DATABASE_URL="postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@localhost:${POSTGRES_PORT:-5432}/${POSTGRES_DB}?sslmode=disable"
fi
# crm-admin runs from backend/ below, so the migrations path is relative to it.
export MIGRATIONS_PATH="${MIGRATIONS_PATH:-migrations}"

PROFILE="${STAGING_RESET_PROFILE:-prod-shaped}"
SERVICE="${STAGING_BACKEND_SERVICE:-personalcrm-backend}"

# Stop the backend service. On a systemd host (the Pi) use systemctl; otherwise
# fall back to the dev stop (kills crm-api / frees port 8080).
stop_service() {
    if command -v systemctl >/dev/null 2>&1 && systemctl list-unit-files "$SERVICE.service" >/dev/null 2>&1; then
        echo "staging-reset: stopping $SERVICE via systemctl..."
        sudo systemctl stop "$SERVICE"
    else
        echo "staging-reset: stopping local dev backend..."
        ( cd "$PROJECT_ROOT" && make dev-api-stop )
    fi
}

start_service() {
    if command -v systemctl >/dev/null 2>&1 && systemctl list-unit-files "$SERVICE.service" >/dev/null 2>&1; then
        echo "staging-reset: starting $SERVICE via systemctl..."
        sudo systemctl start "$SERVICE"
    else
        echo "staging-reset: starting local dev backend..."
        ( cd "$PROJECT_ROOT" && bash scripts/start-backend.sh )
    fi
}

stop_service

# Restart the backend on ANY exit after the stop — including a migration/profile
# failure — so a failed reset never leaves staging stopped (set -e would
# otherwise bail before start_service). The trap is cleared on the success path.
trap 'echo "staging-reset: restarting service after early exit..."; start_service' EXIT

echo "staging-reset: hard reset + reseed ('$PROFILE') against CRM_ENV=$CRM_ENV..."
( cd "$PROJECT_ROOT/backend" && go run cmd/crm-admin/main.go --reset-and-seed --profile "$PROFILE" --yes )

trap - EXIT
start_service
echo "staging-reset: done."
