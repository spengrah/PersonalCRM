#!/usr/bin/env bash
# Emits the adaptive -p / -parallel value for LOCAL integration tests.
#
# This is the SINGLE source of truth for the adaptive parallelism formula.
# Both the Makefile integration recipes and the pre-push guard test call THIS
# script — there is no second formula to drift from.
#
# Ceiling resolution (in order):
#   1. $PG_MAXCONNS if set (the guard test forces this for deterministic renders)
#   2. the LIVE max_connections of the test target, probed via psql
#      ($TEST_DATABASE_URL first, then `docker exec crm-postgres`)
#   3. fall back to 100 (pessimistic stock Postgres) on any probe failure
#
# The probe targets the EXACT test DSN first, but ONLY if a DSN is actually
# present — never `psql ""`, which could hit a default/socket Postgres.
#
# Formula: -p = min(cores-2, (ceiling - 13) / est), floored at 4.
#   - 13 = superuser_reserved_connections(3) + safety margin(10)
#   - est = per-test-binary connection estimate (default 20), locked by an
#     empirical pg_stat_activity peak measurement (see the PR body).
set -euo pipefail

# Validate a value is a positive integer; else echo the fallback.
posint() { case "$1" in (''|*[!0-9]*|0) echo "$2" ;; (*) echo "$1" ;; esac; }

pg="${PG_MAXCONNS:-}"
if [ -z "$pg" ]; then
  # `|| true` on EACH assignment: under `set -euo pipefail` a failing
  # psql/docker in a command substitution returns non-zero (pipefail propagates
  # it) and would abort the script before the next fallback. `|| true`
  # neutralizes that, so a missing/unreachable Postgres or absent docker falls
  # through to the 100 fallback.
  if [ -n "${TEST_DATABASE_URL:-}" ]; then
    pg=$( psql "$TEST_DATABASE_URL" -tAc 'SHOW max_connections;' 2>/dev/null | tr -dc '0-9' | head -c 6 ) || true
  fi
  if [ -z "${pg:-}" ]; then
    pg=$( docker exec crm-postgres psql -U crm_user -d postgres -tAc 'SHOW max_connections;' 2>/dev/null | tr -dc '0-9' | head -c 6 ) || true
  fi
fi

pg=$( posint "${pg:-}" 100 )                       # any garbage/empty/zero => 100
est=$( posint "${PER_BINARY_CONN_EST:-20}" 20 )    # clamp est — locked by measurement
ncore=$( getconf _NPROCESSORS_ONLN 2>/dev/null || sysctl -n hw.ncpu 2>/dev/null || echo 4 )
ncore=$( posint "$ncore" 4 )                       # clamp core count (no div/garbage)

usable=$(( pg - 13 ))                              # - superuser_reserved(3) - margin(10)
budget=$(( usable / est )); [ "$budget" -lt 4 ] && budget=4
np=$(( ncore - 2 )); [ "$np" -lt 4 ] && np=4
[ "$np" -lt "$budget" ] && echo "$np" || echo "$budget"
