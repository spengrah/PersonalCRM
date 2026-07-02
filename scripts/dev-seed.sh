#!/bin/bash
# Seed the `dev` synthetic world into local Postgres, with the dev backend NOT
# running (so the seed harness's River client is the only client on the default
# queue — no race). `make dev-seed` stops any detached backend (dev-api-stop) +
# brings up Postgres BEFORE calling this, then starts the servers AFTER.
#
# Exports DATABASE_URL exactly the way start-backend.sh computes it (from the
# .env component vars) and sets CRM_ENV=testing + MIGRATIONS_PATH=migrations so
# they match the dev backend. crm-admin --seed runs migrations itself (the dev DB
# may be fresh) behind the CRM_ENV gate; --yes is mandatory.

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

set -a
source "$PROJECT_ROOT/.env"
set +a

export DATABASE_URL="postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@localhost:${POSTGRES_PORT:-5432}/${POSTGRES_DB}?sslmode=disable"
export CRM_ENV="testing"
# crm-admin runs from backend/, so the migrations path is relative to backend/
# (NOT backend/migrations — that would resolve to backend/backend/migrations).
export MIGRATIONS_PATH="migrations"

PROFILE="${DEV_SEED_PROFILE:-dev}"

echo "Seeding the '$PROFILE' synthetic world into local Postgres (backend must be stopped)..."
cd "$PROJECT_ROOT/backend"
go run ./cmd/crm-admin --seed --profile "$PROFILE" --yes
