# Shared configuration for the verify-personal-crm helpers. Sourced, not run.
#
# Every value derives from the checkout this file lives in, so two worktrees
# verifying at once get different ports and databases without coordination.

SKILL_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO_ROOT="$(git -C "$SKILL_DIR" rev-parse --show-toplevel)"
# Playwright browsers live at the MAIN checkout root, shared by all worktrees
# (same anchor as frontend/playwright.config.ts).
MAIN_ROOT="$(dirname "$(git -C "$REPO_ROOT" rev-parse --path-format=absolute --git-common-dir)")"

# Slot 0-399 from the checkout path. Override with VERIFY_SLOT on a collision.
VERIFY_SLOT="${VERIFY_SLOT:-$(( $(printf '%s' "$REPO_ROOT" | cksum | cut -d' ' -f1) % 400 ))}"
VERIFY_FRONTEND_PORT=$(( 3400 + VERIFY_SLOT ))
VERIFY_BACKEND_PORT=$(( 8400 + VERIFY_SLOT ))
# e2e-db-reset.sh only accepts personal_crm_test or personal_crm_test_<suffix>.
VERIFY_DB_NAME="personal_crm_test_verify_${VERIFY_SLOT}"
# scripts/e2e-pg.sh picks the PostgreSQL: the shared crm-postgres:5432 when a
# Docker daemon answers, else this checkout's own cluster, else native :5432.
# bin/launch re-reads this after it has started that PostgreSQL.
verify_database_url() { bash "$REPO_ROOT/scripts/e2e-pg.sh" url "$VERIFY_DB_NAME"; }
VERIFY_DATABASE_URL="$(verify_database_url)"
VERIFY_BASE_URL="http://127.0.0.1:${VERIFY_FRONTEND_PORT}"
VERIFY_API_URL="http://127.0.0.1:${VERIFY_BACKEND_PORT}"
# Same key the E2E stack uses (.env.example.testing). Never the real .env.
VERIFY_API_KEY="dev-api-key-change-in-production"

VERIFY_ROOT="$REPO_ROOT/.agents/log/verify"   # gitignored via .agents/log/*
INSTANCE_DIR="$VERIFY_ROOT/instance"       # pids, logs, binary: removed by cleanup
RUNS_DIR="$VERIFY_ROOT/runs"               # evidence: never removed by cleanup

export GOCACHE="${GOCACHE:-$REPO_ROOT/.gocache}"

port_pids() { bash "$REPO_ROOT/scripts/port-pids.sh" "$1" 2>/dev/null || true; }

# psql against the maintenance database or the verify database, in the mode
# scripts/e2e-pg.sh resolves (mirrors scripts/e2e-db-reset.sh).
verify_psql() {
  local db="$1"; shift
  local mode; mode="$(bash "$REPO_ROOT/scripts/e2e-pg.sh" mode)"
  if [ "$mode" = docker ]; then
    docker exec -i crm-postgres psql -v ON_ERROR_STOP=1 -U crm_user -d "$db" "$@"
  elif [ "$mode" = worktree ]; then
    "$(bash "$REPO_ROOT/scripts/worktree-test-pg.sh" psql-bin)" -v ON_ERROR_STOP=1 "$(bash "$REPO_ROOT/scripts/e2e-pg.sh" url "$db")" "$@"
  else
    sudo -u postgres psql -v ON_ERROR_STOP=1 -d "$db" "$@"
  fi
}

# True when PID is alive and its command line contains NEEDLE. Guards every
# kill against PID reuse.
pid_is() {
  local pid="$1" needle="$2"
  [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null && ps -o command= -p "$pid" | grep -qF -- "$needle"
}
