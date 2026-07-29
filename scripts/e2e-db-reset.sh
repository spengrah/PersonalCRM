#!/usr/bin/env bash
# Reset the isolated E2E database, then refuse to continue if another process
# is still attached to it. Run by `make e2e-db`, the prerequisite of every
# test-e2e* target.
#
# Two invariants, each of which fails silently if not enforced here:
#
# 1. THE RESET MUST ACTUALLY HAPPEN. psql exits 0 on a SQL error unless
#    ON_ERROR_STOP is set, so a DROP DATABASE that fails on "database is being
#    accessed by other users" looks exactly like a successful one: the CREATE
#    then fails on "already exists", and the suite runs against the previous
#    run's schema and rows while the target reports success. Terminate the
#    holders first, and run every statement under ON_ERROR_STOP=1 so a reset
#    that did not happen fails loudly.
#
# 2. EXACTLY ONE crm-api MAY OWN THIS DATABASE. The backend keeps rematch job
#    state in memory (service.RematchService) and hands the actual work to
#    River. A second crm-api pointed at the same database — a leftover from a
#    killed E2E run, or another worktree's stack, on any port — runs its own
#    River client, so it can dequeue the rematch_dispatcher job and complete it
#    against ITS OWN in-memory map. river_job then reads `completed` while the
#    API under test reports the job `running` forever, and
#    rematch-on-add-email.spec.ts fails at "rematch job should reach a terminal
#    state" with no diagnostic in any log. CI cannot hit this (one container,
#    one process), so it presents purely as local flake, and a bigger timeout
#    does nothing — the job itself finishes in ~10ms.
#
#    Detection: after the reset, watch for a client reconnecting. A live
#    consumer's pool comes back in well under a second (measured 0.27s-0.69s
#    over three trials); a stale psql session that was just terminated does
#    not come back at all.
set -euo pipefail

# `-` not `:-`: an explicitly EMPTY name must reach the validation below and be
# rejected, not silently fall back to the default.
DB_NAME="${E2E_DATABASE_NAME-personal_crm_test}"
# How long to watch for a foreign client reconnecting after the reset. Set to 0
# to skip the check entirely (escape hatch; the reset itself still runs).
SETTLE_SECONDS="${E2E_DB_SETTLE_SECONDS:-3}"

case "$DB_NAME" in
  '' | *[!A-Za-z0-9_]*)
    echo "Invalid E2E_DATABASE_NAME: $DB_NAME" >&2; exit 1 ;;
esac
case "$DB_NAME" in
  personal_crm_test | personal_crm_test_[A-Za-z0-9_]*) ;;
  *)
    echo "Invalid E2E_DATABASE_NAME: $DB_NAME (must be personal_crm_test or personal_crm_test_<suffix>)" >&2
    exit 1 ;;
esac

if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
  MODE=docker
else
  MODE=native
  if ! sudo -u postgres psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='crm_user';" | grep -q 1; then
    echo "crm_user role missing; run scripts/start-postgres-native.sh first" >&2
    exit 1
  fi
fi

# psql against the maintenance database (for DROP/CREATE and pg_stat_activity).
psql_admin() {
  if [ "$MODE" = docker ]; then
    docker exec crm-postgres psql -v ON_ERROR_STOP=1 -U crm_user -d postgres "$@"
  else
    sudo -u postgres psql -v ON_ERROR_STOP=1 "$@"
  fi
}

# psql against the E2E database itself (for extensions/grants).
psql_target() {
  if [ "$MODE" = docker ]; then
    docker exec crm-postgres psql -v ON_ERROR_STOP=1 -U crm_user -d "$DB_NAME" "$@"
  else
    sudo -u postgres psql -v ON_ERROR_STOP=1 -d "$DB_NAME" "$@"
  fi
}

attached_count() {
  psql_admin -tAc "SELECT count(*) FROM pg_stat_activity WHERE datname = '$DB_NAME' AND pid <> pg_backend_pid();"
}

# Terminate every holder so the DROP below cannot fail on "database is being
# accessed by other users". crm_user may signal its own backends, which is all
# of them; the native path runs as the postgres superuser.
psql_admin -tAc \
  "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '$DB_NAME' AND pid <> pg_backend_pid();" \
  >/dev/null

psql_admin -c "DROP DATABASE IF EXISTS \"$DB_NAME\";" >/dev/null
if [ "$MODE" = docker ]; then
  psql_admin -c "CREATE DATABASE \"$DB_NAME\";" >/dev/null
  psql_target -c 'CREATE EXTENSION IF NOT EXISTS "uuid-ossp"; CREATE EXTENSION IF NOT EXISTS vector;' >/dev/null
else
  psql_admin -c "CREATE DATABASE \"$DB_NAME\" OWNER crm_user;" >/dev/null
  psql_target -c 'CREATE EXTENSION IF NOT EXISTS "uuid-ossp"; CREATE EXTENSION IF NOT EXISTS vector;' >/dev/null
  psql_target -c "GRANT ALL ON SCHEMA public TO crm_user;" >/dev/null
fi

# Watch for a reconnect. Nothing legitimate is attached at this point: e2e-db is
# a prerequisite of the test-e2e* targets, so Playwright has not started the
# backend yet, and the pre-push hook runs E2E exclusively after every other lane
# has finished.
if [ "$SETTLE_SECONDS" != "0" ]; then
  deadline=$(( $(date +%s) + SETTLE_SECONDS ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if [ "$(attached_count)" -gt 0 ]; then
      echo "" >&2
      echo "✗ Another process is connected to $DB_NAME." >&2
      echo "" >&2
      psql_admin -c \
        "SELECT pid, application_name, client_addr, client_port, state, backend_start FROM pg_stat_activity WHERE datname = '$DB_NAME' AND pid <> pg_backend_pid();" >&2 || true
      echo "" >&2
      echo "  A second crm-api on this database runs its own River client and will" >&2
      echo "  steal jobs from the backend under test — rematch and follow-up specs" >&2
      echo "  then fail on state that never becomes terminal. Stop it and re-run." >&2
      echo "" >&2
      # -x matches the process NAME, so it finds both a built ./crm-api and the
      # `go run ./cmd/crm-api` child Playwright starts (whose name is crm-api),
      # without matching every shell whose command line mentions the string.
      echo "  Candidate processes:" >&2
      { pgrep -a -x crm-api 2>/dev/null ||
        pgrep -x crm-api 2>/dev/null ||
        echo "    (none named 'crm-api'; check other clients)"; } >&2
      echo "" >&2
      exit 1
    fi
    sleep 0.25
  done
fi
