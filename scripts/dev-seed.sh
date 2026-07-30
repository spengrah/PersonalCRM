#!/bin/bash
# Seed a synthetic world into local Postgres, with the dev backend NOT running
# (so the seed harness's River client is the only client on the default queue —
# no race). `make dev-seed` stops any detached backend (dev-api-stop) + brings up
# Postgres BEFORE calling this, then starts the servers AFTER.
#
# Exports DATABASE_URL exactly the way start-backend.sh computes it (from the
# .env component vars) and sets MIGRATIONS_PATH=migrations so it matches the dev
# backend. crm-admin runs migrations itself (the dev DB may be fresh) behind the
# CRM_ENV gate; --yes is mandatory.
#
# Knobs:
#   DEV_SEED_PROFILE  which world to seed (default: standard)
#   DEV_SEED_RESET=1  HARD-wipe first (--reset-and-seed) instead of the additive
#                     --seed. An additive seed leaves whatever world was there
#                     before, so anything MEASURING a world (a tour rehearsal, a
#                     marker audit) has to reset or it grades a mixture.
#   CRM_ENV           cadence semantics for the seed (default: testing). It has
#                     to be settable by the CALLER, because the seeder derives
#                     created ages and due dates from the active cadence table:
#                     seeding under `testing` and then serving under `staging`
#                     produces a world whose overdue-ness means something else.

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# Capture the CALLER's value BEFORE sourcing .env. `set -a` + source exports
# every line in .env, so a CRM_ENV line there would otherwise silently beat an
# explicit `CRM_ENV=staging bash scripts/dev-seed.sh` — the precedence has to be
# resolved from a value captured first. Order: caller → .env → testing.
CALLER_CRM_ENV="${CRM_ENV:-}"

set -a
source "$PROJECT_ROOT/.env"
set +a

export DATABASE_URL="postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@localhost:${POSTGRES_PORT:-5432}/${POSTGRES_DB}?sslmode=disable"
export CRM_ENV="${CALLER_CRM_ENV:-${CRM_ENV:-testing}}"
# crm-admin runs from backend/, so the migrations path is relative to backend/
# (NOT backend/migrations — that would resolve to backend/backend/migrations).
export MIGRATIONS_PATH="migrations"

PROFILE="${DEV_SEED_PROFILE:-standard}"

SEED_FLAG="--seed"
SEED_VERB="Seeding"
if [ "${DEV_SEED_RESET:-0}" = "1" ]; then
    SEED_FLAG="--reset-and-seed"
    SEED_VERB="WIPING local Postgres and reseeding"
fi

echo "$SEED_VERB the '$PROFILE' synthetic world into local Postgres (CRM_ENV=$CRM_ENV, backend must be stopped)..."
cd "$PROJECT_ROOT/backend"
go run ./cmd/crm-admin "$SEED_FLAG" --profile "$PROFILE" --yes
