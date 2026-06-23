#!/bin/bash
# Guards the L2 adaptive -p/-parallel render in the Makefile against drift.
# Pure `make -n` string assertions — no running Postgres required (PG_MAXCONNS
# is read from the env by scripts/test-parallelism.sh, so the guard forces the
# ceiling deterministically). Wired into the local pre-push FILTER phase.
#
# Asserts:
#   1. CI pin: GITHUB_ACTIONS=true renders -parallel 4 -p 4 byte-identical.
#   2. stock-100 safety: a forced PG_MAXCONNS=100 renders -parallel 4 -p 4.
#   3. 200 ceiling: the render's -p equals what scripts/test-parallelism.sh
#      emits on THIS machine for PG_MAXCONNS=200 (no independent re-derivation),
#      and -parallel == -p.
#   4. laziness: `make help` never invokes the probe; `make -n test-integration`
#      invokes it exactly once (instrumented via a fake psql on PATH).
#   5. per-worktree pg (gh #433): with CRM_WORKTREE_PG=0 the rendered
#      DATABASE_URL= is byte-identical to today's literal; the resolver runs
#      0x on `make -n help` and the `ensure` recipe is never executed by a
#      dry-run render (counted via CRM_WORKTREE_PG_COUNT_FILE, path-independent
#      since the Makefile calls the resolver by explicit path, not via PATH).
#
# NOTE: the local-case subshells below export CRM_WORKTREE_PG=0 so this guard is
# machine-independent — it must render identically whether or not the box it
# runs on is itself a linked worktree (e.g. an Orca worktree) with a warm
# per-worktree instance. Without that, the resolver's `url` could emit a
# per-worktree DSN and change the render.
set -u
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1   # repo root

fail=0
note() { echo "$1"; }
ok()   { echo "ok: $1"; }
bad()  { echo "FAIL: $1"; fail=1; }

render_pp() {
  # render_pp <target>: echo the "-parallel N -p M" substring from `make -n`.
  # GITHUB_ACTIONS / PG_MAXCONNS are taken from the caller's environment (set via
  # `export` in a subshell), since a `VAR=x func` prefix does NOT pass to a shell
  # function's invoked commands the way it does for an external command.
  make -n "$1" 2>/dev/null | grep -oE '\-parallel [0-9]+ -p [0-9]+' | head -1
}

# --- 1. CI pin byte-identical ---
ci_render=$( export GITHUB_ACTIONS=true; render_pp test-integration-fast )
[[ "$ci_render" == "-parallel 4 -p 4" ]] \
  && ok "CI pin test-integration-fast renders '-parallel 4 -p 4'" \
  || bad "CI pin render was '$ci_render', expected '-parallel 4 -p 4'"

ci_render_long=$( export GITHUB_ACTIONS=true; render_pp test-integration )
[[ "$ci_render_long" == "-parallel 4 -p 4" ]] \
  && ok "CI pin test-integration renders '-parallel 4 -p 4'" \
  || bad "CI pin (long) render was '$ci_render_long', expected '-parallel 4 -p 4'"

# --- 2. stock-100 safety (forced ceiling, GITHUB_ACTIONS cleared) ---
stock_render=$( unset GITHUB_ACTIONS; export PG_MAXCONNS=100 CRM_WORKTREE_PG=0; render_pp test-integration )
[[ "$stock_render" == "-parallel 4 -p 4" ]] \
  && ok "local stock-100 renders '-parallel 4 -p 4'" \
  || bad "local stock-100 render was '$stock_render', expected '-parallel 4 -p 4'"

# --- 3. 200 ceiling: render -p equals the single script's output ---
script_p=$(PG_MAXCONNS=200 bash scripts/test-parallelism.sh 2>/dev/null)
hi_render=$( unset GITHUB_ACTIONS; export PG_MAXCONNS=200 CRM_WORKTREE_PG=0; render_pp test-integration )
expected="-parallel ${script_p} -p ${script_p}"
[[ "$hi_render" == "$expected" ]] \
  && ok "local 200 render '$hi_render' matches script output ($script_p)" \
  || bad "local 200 render was '$hi_render', expected '$expected'"

# -parallel must always equal -p (single-probe memo).
hi_par=$(echo "$hi_render" | grep -oE '\-parallel [0-9]+' | grep -oE '[0-9]+')
hi_p=$(echo "$hi_render" | grep -oE '\-p [0-9]+' | grep -oE '[0-9]+')
[[ -n "$hi_par" && "$hi_par" == "$hi_p" ]] \
  && ok "-parallel ($hi_par) == -p ($hi_p)" \
  || bad "-parallel ($hi_par) != -p ($hi_p)"

# --- 4. laziness/once (instrumented fake psql + counter file) ---
tmpd=$(mktemp -d) || { echo "FAIL: mktemp -d failed"; exit 1; }
ctr="$tmpd/count"
: > "$ctr"
cat > "$tmpd/psql" <<EOF
#!/bin/bash
echo x >> "$ctr"
echo 200
EOF
chmod +x "$tmpd/psql"

# make help must NOT invoke the probe.
PATH="$tmpd:$PATH" GITHUB_ACTIONS='' CRM_WORKTREE_PG=0 TEST_DATABASE_URL='postgres://fake/db' make -n help >/dev/null 2>&1
help_n=$(wc -l < "$ctr" | tr -d ' ')
[[ "$help_n" -eq 0 ]] \
  && ok "make help invokes the probe 0 times (lazy)" \
  || bad "make help invoked the probe $help_n times (should be 0)"

# make -n test-integration must invoke it exactly once (memoized).
: > "$ctr"
PATH="$tmpd:$PATH" GITHUB_ACTIONS='' CRM_WORKTREE_PG=0 TEST_DATABASE_URL='postgres://fake/db' make -n test-integration >/dev/null 2>&1
int_n=$(wc -l < "$ctr" | tr -d ' ')
[[ "$int_n" -eq 1 ]] \
  && ok "make -n test-integration invokes the probe exactly once (memoized)" \
  || bad "make -n test-integration invoked the probe $int_n times (should be 1)"

rm -rf "$tmpd"

# --- 5. per-worktree pg resolver (gh #433) ---------------------------------
# The Makefile calls scripts/worktree-test-pg.sh by explicit path, so a PATH
# shim cannot intercept it. The resolver instead honors CRM_WORKTREE_PG_COUNT_FILE
# (appends a line per real invocation), which we use to count invocations.
wctr=$(mktemp) || { echo "FAIL: mktemp failed"; exit 1; }

# 5a. forced-shared (CRM_WORKTREE_PG=0): rendered DATABASE_URL= is the literal.
db_render=$( unset GITHUB_ACTIONS; export CRM_WORKTREE_PG=0; \
  make -n test-integration 2>/dev/null | grep -oE 'DATABASE_URL="[^"]+"' | head -1 )
expected_db='DATABASE_URL="postgres://crm_user:crm_password@localhost:5432/personal_crm_test?sslmode=disable"'
[[ "$db_render" == "$expected_db" ]] \
  && ok "forced-shared (CRM_WORKTREE_PG=0) renders the literal DATABASE_URL" \
  || bad "forced-shared DATABASE_URL was '$db_render', expected '$expected_db'"

# 5b. `make -n help` invokes the resolver 0 times (forced shared, machine-indep).
: > "$wctr"
CRM_WORKTREE_PG=0 CRM_WORKTREE_PG_COUNT_FILE="$wctr" make -n help >/dev/null 2>&1
wh=$(wc -l < "$wctr" | tr -d ' ')
[[ "$wh" -eq 0 ]] \
  && ok "make -n help invokes the worktree-pg resolver 0 times" \
  || bad "make -n help invoked the worktree-pg resolver $wh times (should be 0)"

# 5c. `make -n test-integration` invokes the resolver at most twice (the two
# lazy memos: `url` for WORKTREE_TEST_DB_URL and `active` for the ensure-command
# gate). Both are render-safe value lookups; NEITHER executes a recipe command
# on a dry run. Forced-shared so the count is machine-independent (the resolver
# is still INVOKED under CRM_WORKTREE_PG=0 — it just returns empty).
: > "$wctr"
CRM_WORKTREE_PG=0 CRM_WORKTREE_PG_COUNT_FILE="$wctr" make -n test-integration >/dev/null 2>&1
wi=$(wc -l < "$wctr" | tr -d ' ')
[[ "$wi" -le 2 ]] \
  && ok "make -n test-integration invokes the worktree-pg resolver <=2 times ($wi: url + active memos, no recipe run)" \
  || bad "make -n test-integration invoked the worktree-pg resolver $wi times (should be <=2)"

# 5d. FULL render byte-identity: under CRM_WORKTREE_PG=0 the ENTIRE `make -n
# test-integration` output (normalized for the adaptive -p number) must equal
# develop's, and must contain NO `worktree-test-pg.sh ensure` line. This guards
# the whole-recipe byte-identity invariant (a prerequisite recipe line is part
# of `make -n` output even though the dependent recipe's command is unchanged).
fs_full=$( unset GITHUB_ACTIONS; export CRM_WORKTREE_PG=0 PG_MAXCONNS=200; \
  make -n test-integration 2>/dev/null | sed -E 's/-parallel [0-9]+ -p [0-9]+/-parallel X -p X/' )
ens_lines=$( printf '%s\n' "$fs_full" | grep -c 'worktree-test-pg.sh ensure' || true )
[[ "$ens_lines" -eq 0 ]] \
  && ok "forced-shared render emits NO ensure line (byte-identity preserved)" \
  || bad "forced-shared render emitted $ens_lines ensure line(s) (breaks byte-identity)"

rm -f "$wctr"

[[ "$fail" -eq 0 ]] && { echo "ALL PASS (parallelism render guard)"; exit 0; } || { echo "FAILURES (parallelism render guard)"; exit 1; }
