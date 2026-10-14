#!/usr/bin/env bash
# The PostgreSQL that local E2E (make e2e-db, make test-e2e*) and the verify
# skill (.agents/skills/verify-personal-crm) run against. The rule lives here
# and every caller asks this script:
#
#   docker    A Docker daemon answers: the shared crm-postgres container on
#             localhost:5432, started with Docker Compose.
#   worktree  No Docker daemon answers, and this checkout's own cluster from
#             scripts/worktree-test-pg.sh is running: that cluster.
#   native    Otherwise: the system PostgreSQL on localhost:5432
#             (scripts/start-postgres-native.sh, Debian only).
#
# In worktree mode the integration-test harness owns personal_crm_test as its
# base database in the same cluster, so local E2E uses personal_crm_test_e2e
# there and scripts/e2e-db-reset.sh refuses to reset personal_crm_test.
#
# Subcommands:
#   ensure        Start the PostgreSQL the rule picks: Docker Compose when a
#                 daemon answers, else this checkout's cluster, else native.
#   mode          Print docker, worktree or native. Starts nothing.
#   e2e-db-name   Print the default E2E database name for the mode.
#   url <db>      Print the connection URL for database <db>. Starts nothing.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
# shellcheck source=lib/docker-answers.sh
source "$SCRIPT_DIR/lib/docker-answers.sh"

# Sets MODE, and CLUSTER_URL (this checkout's cluster URL) in worktree mode.
resolve() {
  CLUSTER_URL=""
  if docker_answers; then
    MODE=docker
    return
  fi
  CLUSTER_URL="$(bash "$SCRIPT_DIR/worktree-test-pg.sh" url)"
  if [ -n "$CLUSTER_URL" ]; then MODE=worktree; else MODE=native; fi
}

cmd_ensure() {
  echo "🔍 Ensuring PostgreSQL is available for tests..."
  if docker_answers; then
    echo "🐳 Docker is available; starting database via Docker Compose..."
    (cd "$PROJECT_ROOT/infra" && docker compose up -d)
    bash "$PROJECT_ROOT/scripts/sync-postgres-auth.sh"
    return 0
  fi
  if [ -n "$(bash "$SCRIPT_DIR/worktree-test-pg.sh" active)" ]; then
    echo "🐘 No Docker daemon; starting this checkout's own PostgreSQL cluster (scripts/worktree-test-pg.sh)..."
    if CRM_WORKTREE_PG=strict bash "$SCRIPT_DIR/worktree-test-pg.sh" ensure; then
      return 0
    fi
    echo "⚠️  This checkout's cluster did not start; falling back to native PostgreSQL..."
  elif command -v docker >/dev/null 2>&1; then
    echo "⚠️  Docker is installed but not available; falling back to native PostgreSQL..."
  else
    echo "⚠️  Docker is not installed; falling back to native PostgreSQL..."
  fi
  bash "$PROJECT_ROOT/scripts/start-postgres-native.sh"
}

case "${1:-}" in
  ensure) cmd_ensure ;;
  mode) resolve; echo "$MODE" ;;
  e2e-db-name)
    resolve
    if [ "$MODE" = worktree ]; then echo personal_crm_test_e2e; else echo personal_crm_test; fi ;;
  url)
    [ -n "${2:-}" ] || { echo "usage: e2e-pg.sh url <db>" >&2; exit 2; }
    resolve
    if [ "$MODE" = worktree ]; then
      printf '%s/%s?sslmode=disable\n' "${CLUSTER_URL%/*}" "$2"
    else
      printf 'postgres://crm_user:crm_password@localhost:5432/%s?sslmode=disable\n' "$2"
    fi ;;
  *) echo "usage: e2e-pg.sh {ensure|mode|e2e-db-name|url <db>}" >&2; exit 2 ;;
esac
