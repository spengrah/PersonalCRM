#!/usr/bin/env bash
# Real-cluster smoke for scripts/worktree-test-pg.sh (gh #433).
#
# This is the LOAD-BEARING path: it does an actual initdb + pg_ctl start +
# role/db/extension provisioning against a private cluster, then drives the
# REAL `make test-integration-fast` recipe through a throwaway temp linked
# worktree to prove the cold-first-run Make wiring (worktree-test-pg-ensure
# prereq -> $(TEST_DATABASE_URL) -> $(WORKTREE_TEST_DB_URL) -> ADAPTIVE_P) works
# end-to-end. NOT in pre-push (it owns a DB + binds a port). Run via
# `make test-pg-smoke`.
#
# Required-gate policy: when CRM_PG_SMOKE_REQUIRED=1 and no pg16 toolchain is
# present, the script EXITS NON-ZERO ("required smoke could not run") so a skip
# can never masquerade as a pass. Without that flag, a missing toolchain is a
# clean skip (exit 0 with a notice).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
RESOLVER="$REPO_ROOT/scripts/worktree-test-pg.sh"
REQUIRED="${CRM_PG_SMOKE_REQUIRED:-0}"

# Bound every lock this smoke takes: with the #600 fix all locks acquire
# instantly, so this is invisible on a healthy tree; a regression that re-leaks
# fd 200 into the postmaster would otherwise HANG here forever — instead it fails
# LOUD after the timeout. Overridable, defaults to a generous 60s.
export CRM_WORKTREE_PG_LOCK_TIMEOUT="${CRM_WORKTREE_PG_LOCK_TIMEOUT:-60}"

fail=0
ok()   { echo "ok: $1"; }
bad()  { echo "FAIL: $1"; fail=1; }
note() { echo "--- $1"; }

# --- Toolchain precondition -------------------------------------------------
# Reuse the resolver's own discovery so "present" means exactly what `ensure`
# will use. resolve_bindir is sourced indirectly via `status`/`port` not being
# enough, so probe directly with the same candidate logic.
find_bindir() {
  local d ver candidates=()
  command -v pg_config >/dev/null 2>&1 && candidates+=("$(pg_config --bindir 2>/dev/null || true)")
  candidates+=( "${PGBIN:-}" /opt/homebrew/opt/postgresql@16/bin \
                /usr/local/opt/postgresql@16/bin /usr/lib/postgresql/16/bin /usr/pgsql-16/bin )
  for d in "${candidates[@]}"; do
    [ -n "$d" ] && [ -x "$d/initdb" ] && [ -x "$d/pg_ctl" ] && [ -x "$d/postgres" ] || continue
    ver=$("$d/postgres" --version 2>/dev/null | grep -oE '[0-9]+' | head -1) || true
    [ "$ver" = "16" ] && { printf '%s' "$d"; return 0; }
  done
  return 1
}

if ! BINDIR=$(find_bindir); then
  if [ "$REQUIRED" = "1" ]; then
    echo "FAIL: required smoke could not run: no Postgres 16 toolchain (install pg16 + pgvector via 'make setup')." >&2
    exit 1
  fi
  echo "SKIP: no Postgres 16 toolchain found; smoke not run (set CRM_PG_SMOKE_REQUIRED=1 to make this a hard failure)."
  exit 0
fi
note "using Postgres 16 bindir: $BINDIR"

# Isolated state so the smoke can never touch a dev's real per-worktree pg.
TMP=$(mktemp -d)
SMOKE_PG_HOME="$TMP/pghome"
cleanup() {
  # Stop/teardown any instance the smoke started, then remove the temp tree.
  if [ -n "${SMOKE_WT:-}" ] && [ -d "$SMOKE_WT" ]; then
    CRM_WORKTREE_PG_HOME="$SMOKE_PG_HOME" bash "$RESOLVER" teardown >/dev/null 2>&1 || true
    ( cd "$SMOKE_WT" 2>/dev/null && CRM_WORKTREE_PG_HOME="$SMOKE_PG_HOME" bash "$RESOLVER" teardown >/dev/null 2>&1 ) || true
  fi
  # Final sweep: stop any postgres still bound under our temp pg home.
  shopt -s nullglob 2>/dev/null || true
  for dd in "$SMOKE_PG_HOME"/*/data; do
    [ -s "$dd/PG_VERSION" ] && "$BINDIR/pg_ctl" -D "$dd" -m immediate stop >/dev/null 2>&1 || true
  done
  rm -rf "$TMP" 2>/dev/null || true
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# Phase A: a throwaway temp LINKED worktree, so worktree detection fires for real.
# ---------------------------------------------------------------------------
note "creating throwaway linked worktree"
SMOKE_WT="$TMP/wt"
# `git worktree add --detach` off the current HEAD creates a real linked worktree
# pointing at THIS repo's .git — git-dir != git-common-dir there.
git -C "$REPO_ROOT" worktree add --detach "$SMOKE_WT" HEAD >/dev/null 2>&1
trap 'git -C "$REPO_ROOT" worktree remove --force "$SMOKE_WT" >/dev/null 2>&1 || true; cleanup' EXIT

run_in_wt() { ( cd "$SMOKE_WT" && CRM_WORKTREE_PG_HOME="$SMOKE_PG_HOME" "$@" ); }

# ---------------------------------------------------------------------------
# Phase B: ensure from cold + catalog assertions.
# ---------------------------------------------------------------------------
note "ensure (cold): initdb + start + provisioning"
run_in_wt env CRM_WORKTREE_PG=strict bash "$RESOLVER" ensure
URL=$(run_in_wt bash "$RESOLVER" url)
[ -n "$URL" ] && ok "ensure produced a per-worktree URL" || { bad "ensure produced no URL"; echo "FAILURES"; exit 1; }
PORT=$(run_in_wt bash "$RESOLVER" port)
note "instance up at 127.0.0.1:$PORT"

PSQL=( "$BINDIR/psql" "$URL" -tAX -q )

mc=$( "${PSQL[@]}" -c 'SHOW max_connections;' | tr -dc '0-9' )
[ "$mc" = "200" ] && ok "max_connections=200" || bad "max_connections=$mc (expected 200)"

enc=$( "${PSQL[@]}" -c 'SHOW server_encoding;' | tr -d '[:space:]' )
[ "$enc" = "UTF8" ] && ok "server_encoding=UTF8" || bad "server_encoding=$enc"

# Normalize the collation so en_US.UTF-8 (macOS) == en_US.utf8 (Linux/Docker):
# lowercase, then strip - . and _ (en_US.UTF-8 -> enusutf8, en_US.utf8 -> enusutf8).
# NOTE: the `--` guard is required — BSD/macOS `tr` parses a leading '-' in the
# delete set as an option flag and errors "illegal option".
norm() { printf '%s' "$1" | tr 'A-Z' 'a-z' | tr -d -- '-._'; }
coll=$( "${PSQL[@]}" -c "SELECT datcollate FROM pg_database WHERE datname='personal_crm_test';" | tr -d '[:space:]' )
[ "$(norm "$coll")" = "enusutf8" ] && ok "datcollate parity ($coll ~ en_US.utf8)" || bad "datcollate=$coll (expected en_US.UTF-8 / en_US.utf8)"

for ext in uuid-ossp vector pg_trgm; do
  got=$( "${PSQL[@]}" -c "SELECT 1 FROM pg_extension WHERE extname='$ext';" | tr -dc '01' )
  [ "$got" = "1" ] && ok "extension installed: $ext" || bad "extension MISSING: $ext"
done

# Direct trigram parity probe (self-contained companion to the Go test below).
# Use a robustly-similar pair: similarity('Jonathan','Jonathon') is ~0.5, well
# clear of any threshold, so this proves pg_trgm produces sensible
# collation-correct scores without hinging on a borderline value (e.g.
# 'Jon'/'John' is genuinely only ~0.29). The authoritative parity proof is the
# real TestFindSimilarContactsBatch_Integration run in Phase C below.
sim=$( "${PSQL[@]}" -c "SELECT (similarity('Jonathan','Jonathon') > 0.4);" | tr -d '[:space:]' )
[ "$sim" = "t" ] && ok "pg_trgm similarity('Jonathan','Jonathon')>0.4 (collation-correct scoring)" || bad "trigram probe returned '$sim'"

# --- #600 regression: the REAL postmaster must NOT have inherited fd 200 -----
# A leaked fd 200 keeps the per-instance flock held for the postmaster's whole
# life, wedging every later ensure/stop/teardown/reap. This is the exact leak
# signature from the issue (/proc/<postmaster>/fd/200 -> the instance lock). The
# shim suite proves this with fakes one inheritance level deep; this proves it
# against a real daemonized postmaster. Linux-only (/proc); a clean skip on macOS.
note "#600: real postmaster did not inherit the instance-lock fd"
if [ -d /proc ]; then
  DD_B=$(run_in_wt bash "$RESOLVER" status | grep -oE 'datadir=[^ ]+' | sed 's/datadir=//')
  LOCKPATH_B="$(dirname "$DD_B")/lock"
  PMPID_B=$(head -1 "$DD_B/postmaster.pid" 2>/dev/null || true)
  leaked=0
  if [ -n "$PMPID_B" ] && [ -d "/proc/$PMPID_B/fd" ]; then
    for fdlink in /proc/"$PMPID_B"/fd/*; do
      [ "$(readlink "$fdlink" 2>/dev/null || true)" = "$LOCKPATH_B" ] && leaked=1
    done
  fi
  [ "$leaked" -eq 0 ] && ok "#600: postmaster (pid $PMPID_B) did not inherit the instance-lock fd" \
    || bad "#600: postmaster leaked the instance-lock fd ($LOCKPATH_B) — the wedge bug is back"
else
  note "no /proc; skipping the postmaster fd-leak check (macOS)"
fi

# A second ensure must complete under the (bounded) lock timeout — the direct
# functional proof that the first ensure did not wedge the lock.
note "#600: a second ensure returns under the lock timeout"
run_in_wt env CRM_WORKTREE_PG=strict bash "$RESOLVER" ensure \
  && ok "#600: second ensure returned under the lock timeout (lock not wedged)" \
  || bad "#600: second ensure did not return under the lock timeout (lock wedged?)"

# ---------------------------------------------------------------------------
# Phase C: end-to-end through the REAL Make recipe (cold-first-run ordering).
# A FRESH worktree (no instance yet) drives `make test-integration-fast` so the
# worktree-test-pg-ensure prereq must bring the server up before
# $(TEST_DATABASE_URL) resolves. We narrow to one collation-sensitive test.
# ---------------------------------------------------------------------------
note "end-to-end: make test-integration-fast through the real recipe (cold)"
COLD_WT="$TMP/wt-cold"
git -C "$REPO_ROOT" worktree add --detach "$COLD_WT" HEAD >/dev/null 2>&1
COLD_PG_HOME="$TMP/pghome-cold"

set +e
OUT=$( cd "$COLD_WT" && CRM_WORKTREE_PG_HOME="$COLD_PG_HOME" CRM_WORKTREE_PG=strict \
  make test-integration-fast \
    INTEGRATION_RUN='TestFindSimilarContactsBatch_Integration' \
    INTEGRATION_PKGS='./tests/...' 2>&1 )
rc=$?
set -e
echo "$OUT" | tail -40

# (a) the harness actually ran (a package clone was created inside our instance)
echo "$OUT" | grep -qE 'testdb: .*(clone|template)' && ok "harness ran (testdb clone/template log present)" \
  || bad "no testdb harness log in make output (harness may not have used the per-worktree instance)"

# (b) HARD ASSERTION that the suite ran against the per-worktree instance, NOT
# shared :5432. The DSN is not logged, so we prove it positively:
# the testdb harness mints personal_crm_test_clone_*/_template_* databases
# inside whichever instance it used. This cold worktree's instance was created
# fresh for this run, so any such DB on its port is unambiguous proof the suite
# used it. (A regression to :5432 would leave the cold instance with ZERO
# clone/template DBs — failing this assertion even if the Go test went green.)
COLD_PORT=$( cd "$COLD_WT" && CRM_WORKTREE_PG_HOME="$COLD_PG_HOME" bash "$RESOLVER" port )
COLD_URL=$( cd "$COLD_WT" && CRM_WORKTREE_PG_HOME="$COLD_PG_HOME" bash "$RESOLVER" url )
harness_dbs=""
if [ -n "$COLD_URL" ]; then
  harness_dbs=$( "$BINDIR/psql" "$COLD_URL" -tAc \
    "SELECT count(*) FROM pg_database WHERE datname LIKE 'personal_crm_test_clone_%' OR datname LIKE 'personal_crm_test_template_%';" 2>/dev/null | tr -dc '0-9' )
fi
{ [ -n "$harness_dbs" ] && [ "$harness_dbs" -ge 1 ]; } \
  && ok "suite used the per-worktree instance (port $COLD_PORT has $harness_dbs harness clone/template DB(s))" \
  || bad "no harness clone/template DBs on the per-worktree instance (port $COLD_PORT) — the suite did NOT use it (ran against shared :5432?)"

# (c) the selected test passed.
[ "$rc" -eq 0 ] && ok "make test-integration-fast (trigram test) passed through the per-worktree instance" \
  || bad "make test-integration-fast failed (rc=$rc)"

# teardown the cold instance
( cd "$COLD_WT" && CRM_WORKTREE_PG_HOME="$COLD_PG_HOME" bash "$RESOLVER" teardown >/dev/null 2>&1 ) || true
git -C "$REPO_ROOT" worktree remove --force "$COLD_WT" >/dev/null 2>&1 || true

# ---------------------------------------------------------------------------
# Phase D: reuse + lifecycle (against the Phase B instance).
# ---------------------------------------------------------------------------
note "reuse + lifecycle"
run_in_wt bash "$RESOLVER" ensure   # second ensure: no-op
PORT2=$(run_in_wt bash "$RESOLVER" port)
[ "$PORT" = "$PORT2" ] && ok "reuse: same port after second ensure ($PORT2)" || bad "port changed on reuse: $PORT -> $PORT2"

run_in_wt bash "$RESOLVER" stop
run_in_wt bash "$RESOLVER" ensure   # restart on same port + datadir
PORT3=$(run_in_wt bash "$RESOLVER" port)
[ "$PORT" = "$PORT3" ] && ok "stop+ensure restarts on same port ($PORT3)" || bad "restart changed port: $PORT -> $PORT3"

DD=$(cd "$SMOKE_WT" && CRM_WORKTREE_PG_HOME="$SMOKE_PG_HOME" bash "$RESOLVER" status | grep -oE 'datadir=[^ ]+' | sed 's/datadir=//')
run_in_wt bash "$RESOLVER" teardown
[ ! -d "$DD" ] && ok "teardown removed the datadir" || bad "teardown left datadir: $DD"

echo ""
[ "$fail" -eq 0 ] && { echo "ALL PASS (worktree-test-pg smoke)"; exit 0; } || { echo "FAILURES (worktree-test-pg smoke)"; exit 1; }
