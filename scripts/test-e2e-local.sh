#!/usr/bin/env bash
# Runs Playwright E2E tests against the isolated E2E database: `make
# test-e2e-local` delegates here, and the script runs directly too.
#
#   bash scripts/test-e2e-local.sh [spec-path ...]
#
# Spec paths go to Playwright after --project=chromium, so they filter the
# suite as Playwright filters by path. PLAYWRIGHT_GREP, when set, narrows the
# run by test title. ENV_FILE overrides .env.example.testing.
#
# `make test-e2e-local` prepares the database and passes where it is
# (E2E_DATABASE_URL and the two ports). Run directly, the script asks make to
# prepare it and read the same values.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

if [ -z "${E2E_DATABASE_URL:-}" ]; then
  # A checkout's own PostgreSQL cluster lives as long as its owner process
  # (scripts/worktree-test-pg.sh): this script, not the make that prepares it.
  export CRM_WORKTREE_PG_OWNER="${CRM_WORKTREE_PG_OWNER:-$$}"
  make --no-print-directory -C "$REPO_ROOT" e2e-db >&2
  eval "$(make --no-print-directory -s -C "$REPO_ROOT" e2e-env)"
fi

ENV_FILE="${ENV_FILE:-$REPO_ROOT/.env.example.testing}"
if [ ! -f "$ENV_FILE" ]; then
  echo "❌ ENV file not found: $ENV_FILE"
  exit 1
fi
echo "Running Playwright E2E tests (local selection)..."
set -a
# shellcheck disable=SC1090
. "$ENV_FILE"
set +a
export DATABASE_URL="$E2E_DATABASE_URL"

ENV_LOCAL="$REPO_ROOT/frontend/.env.local"
restore_env_local() {
  rm -f "$ENV_LOCAL"
  if [ -f "$ENV_LOCAL.bak" ]; then mv "$ENV_LOCAL.bak" "$ENV_LOCAL"; fi
}
if [ -f "$ENV_LOCAL" ]; then mv "$ENV_LOCAL" "$ENV_LOCAL.bak"; fi
trap restore_env_local EXIT
echo "NEXT_PUBLIC_API_KEY=$API_KEY" > "$ENV_LOCAL"
echo "NEXT_PUBLIC_API_URL=http://localhost:$E2E_BACKEND_PORT" >> "$ENV_LOCAL"

args=(--project=chromium)
if [ -n "${PLAYWRIGHT_GREP:-}" ]; then args+=(--grep "$PLAYWRIGHT_GREP"); fi
args+=("$@")

export NEXT_PUBLIC_API_KEY="$API_KEY" NEXT_PUBLIC_API_URL="http://localhost:$E2E_BACKEND_PORT"
export E2E_FRONTEND_PORT E2E_BACKEND_PORT
cd "$REPO_ROOT/frontend"
./node_modules/.bin/playwright test "${args[@]}"
