#!/usr/bin/env bash
# Shim-only unit tests for scripts/worktree-test-pg.sh (gh #433, Thing 2).
#
# DB-FREE + PORT-FREE: every external dependency (git, initdb, pg_ctl, postgres,
# psql, pg_isready, pg_config, locale) is a PATH-shimmed fake. No real initdb,
# no pg_ctl start, no port bind. Safe for the pre-push FILTER lane.
#
# Invoked from scripts/hooks/test/test-pre-push-filters.sh (like the render
# guard), not as a top-level pre-push command.
set -u
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 1   # repo root
SCRIPT="$PWD/scripts/worktree-test-pg.sh"

fail=0
ok()  { echo "ok: $1"; }
bad() { echo "FAIL: $1"; fail=1; }

# --- Fake toolchain factory -------------------------------------------------
# Builds a temp dir with: a fake git (configurable git-dir/common-dir + worktree
# list), a fake bindir (initdb/pg_ctl/postgres/psql/pg_isready/pg_config), a
# fake locale, and a clean CRM_WORKTREE_PG_HOME. Each test gets its own.
make_env() {
  local d; d=$(mktemp -d)
  local bin="$d/bin"; mkdir -p "$bin"
  local home="$d/pghome"; mkdir -p "$home"
  echo "$d"

  # --- fake git: reads $d/git-dir, $d/common-dir, $d/worktrees ---
  cat > "$bin/git" <<EOF
#!/usr/bin/env bash
case "\$*" in
  "rev-parse --git-dir")            cat "$d/git-dir" 2>/dev/null ;;
  "rev-parse --git-common-dir")     cat "$d/common-dir" 2>/dev/null ;;
  "rev-parse --absolute-git-dir")   cat "$d/git-dir" 2>/dev/null ;;
  "rev-parse --show-toplevel")      echo "$d/repo" ;;
  "worktree list --porcelain")      cat "$d/worktrees" 2>/dev/null ;;
  *"rev-parse --absolute-git-dir")  echo "/nonexistent/.git" ;;  # git -C <dead-wt>
  *) exit 0 ;;
esac
EOF
  chmod +x "$bin/git"

  # --- fake pg_config: points resolve_bindir at our fake bindir ---
  cat > "$bin/pg_config" <<EOF
#!/usr/bin/env bash
[ "\$1" = "--bindir" ] && echo "$bin"
EOF
  chmod +x "$bin/pg_config"

  # --- fake postgres: major version from \$d/pg_major (default 16) ---
  cat > "$bin/postgres" <<EOF
#!/usr/bin/env bash
maj=\$(cat "$d/pg_major" 2>/dev/null || echo 16)
[ "\$1" = "--version" ] && echo "postgres (PostgreSQL) \$maj.2"
EOF
  chmod +x "$bin/postgres"

  # --- fake initdb: records invocation; fails if \$d/initdb_fail exists ---
  cat > "$bin/initdb" <<EOF
#!/usr/bin/env bash
echo "initdb \$*" >> "$d/calls"
[ -f "$d/initdb_fail" ] && exit 1
# Honor -D <datadir>: write a PG_VERSION so the script sees an inited cluster.
for a in "\$@"; do dd=\$prev; prev=\$a; done
dd=""
while [ \$# -gt 0 ]; do [ "\$1" = "-D" ] && { dd="\$2"; }; shift; done
[ -n "\$dd" ] && { mkdir -p "\$dd"; echo 16 > "\$dd/PG_VERSION"; }
exit 0
EOF
  chmod +x "$bin/initdb"

  # --- fake pg_ctl: records; start writes postmaster.pid + marks the port
  # "busy" (so the fake pg_isready reports the instance up); stop unmarks it.
  # Never binds a real port. The port comes from the -o "-p <port> ..." arg. ---
  cat > "$bin/pg_ctl" <<EOF
#!/usr/bin/env bash
echo "pg_ctl \$*" >> "$d/calls"
[ -f "$d/pgctl_fail" ] && exit 1
dd=""; opts=""; mode=""
while [ \$# -gt 0 ]; do
  case "\$1" in -D) dd="\$2"; shift ;; -o) opts="\$2"; shift ;; start|stop|restart) mode="\$1" ;; esac
  shift
done
port=\$(echo "\$opts" | grep -oE -- '-p [0-9]+' | grep -oE '[0-9]+')
case "\$mode" in
  start) [ -n "\$dd" ] && echo "\${FAKE_PG_PID:-\$\$}" > "\$dd/postmaster.pid"; [ -n "\$port" ] && echo "\$port" >> "$d/port_busy" ;;
  stop)  [ -n "\$port" ] && { grep -vx "\$port" "$d/port_busy" 2>/dev/null > "$d/port_busy.tmp" || true; mv "$d/port_busy.tmp" "$d/port_busy" 2>/dev/null || true; } ;;
esac
exit 0
EOF
  chmod +x "$bin/pg_ctl"

  # --- fake psql: provisioning always succeeds; SELECT returns nothing ---
  cat > "$bin/psql" <<EOF
#!/usr/bin/env bash
echo "psql \$*" >> "$d/calls"
exit 0
EOF
  chmod +x "$bin/psql"

  # --- fake pg_isready: port "answers" only if \$d/port_busy lists it ---
  cat > "$bin/pg_isready" <<EOF
#!/usr/bin/env bash
port=""
while [ \$# -gt 0 ]; do [ "\$1" = "-p" ] && port="\$2"; shift; done
grep -qx "\$port" "$d/port_busy" 2>/dev/null && exit 0
exit 1
EOF
  chmod +x "$bin/pg_isready"

  # --- fake locale: prints en_US.UTF-8 unless \$d/no_locale exists ---
  cat > "$bin/locale" <<EOF
#!/usr/bin/env bash
[ -f "$d/no_locale" ] && { echo C; exit 0; }
[ "\$1" = "-a" ] && printf 'C\nen_US.UTF-8\nPOSIX\n'
EOF
  chmod +x "$bin/locale"
}

# Configure a temp env as a LINKED worktree (git-dir != common-dir).
set_linked() {
  local d="$1" id="${2:-wt-alpha}"
  echo "$d/gitdir-$id" > "$d/git-dir"
  echo "$d/common"     > "$d/common-dir"
}
# Configure as MAIN checkout (git-dir == common-dir).
set_main() {
  local d="$1"
  echo "$d/.git" > "$d/git-dir"
  echo "$d/.git" > "$d/common-dir"
}

# Run the script with the fake PATH + isolated pg home. FAKE_PG_PID is the test
# runner's own pid (alive for the whole suite) so the fake pg_ctl writes a
# postmaster.pid that pid_alive() sees as a live server across invocations.
# CRM_WORKTREE_PG_BINDIR confines binary discovery to the fake bindir so a real
# pg16 install on the host can't defeat the wrong-major / missing-binary fakes.
run() {
  local d="$1"; shift
  PATH="$d/bin:$PATH" \
  CRM_WORKTREE_PG_HOME="$d/pghome" \
  CRM_WORKTREE_PG_BINDIR="$d/bin" \
  FAKE_PG_PID="$$" \
    bash "$SCRIPT" "$@"
}

# ---------------------------------------------------------------------------
# 1. Detection: main checkout -> url empty
# ---------------------------------------------------------------------------
d=$(make_env | tail -1); set_main "$d"
out=$(run "$d" url)
[ -z "$out" ] && ok "main checkout: url empty" || bad "main checkout: url='$out' (expected empty)"

# 2. Detection: linked worktree, not yet ensured -> url empty (does NOT start)
d=$(make_env | tail -1); set_linked "$d"
out=$(run "$d" url 2>"$d/err")
[ -z "$out" ] && ok "linked, unprovisioned: url empty" || bad "linked unprovisioned: url='$out'"
[ ! -s "$d/calls" ] && ok "url starts nothing (no initdb/pg_ctl)" || bad "url invoked a binary: $(cat "$d/calls")"
[ ! -s "$d/err" ] && ok "url silent on stderr" || bad "url wrote stderr: $(cat "$d/err")"

# 3. GITHUB_ACTIONS=true -> url empty even in a linked worktree
d=$(make_env | tail -1); set_linked "$d"
out=$(GITHUB_ACTIONS=true run "$d" url)
[ -z "$out" ] && ok "CI: url empty" || bad "CI: url='$out'"

# 4. CRM_WORKTREE_PG=0 -> url empty in a linked worktree
d=$(make_env | tail -1); set_linked "$d"
out=$(CRM_WORKTREE_PG=0 run "$d" url)
[ -z "$out" ] && ok "opt-out (=0): url empty" || bad "opt-out: url='$out'"

# 5. port: linked worktree returns a seed in [BASE_PORT, BASE_PORT+SPAN)
d=$(make_env | tail -1); set_linked "$d"
p=$(run "$d" port)
{ [ "$p" -ge 5440 ] && [ "$p" -lt 5640 ]; } && ok "port seed in range ($p)" || bad "port out of range: $p"

# 6. Two distinct worktrees -> two distinct ids (=> distinct data dirs/seeds)
d1=$(make_env | tail -1); set_linked "$d1" alpha
d2=$(make_env | tail -1); set_linked "$d2" beta
id1=$(run "$d1" status | grep -oE 'id=[0-9a-f]+' | head -1)
id2=$(run "$d2" status | grep -oE 'id=[0-9a-f]+' | head -1)
[ -n "$id1" ] && [ "$id1" != "$id2" ] && ok "distinct worktrees -> distinct ids ($id1 != $id2)" \
  || bad "ids collided or empty: '$id1' vs '$id2'"

# 7. ensure (happy path, fakes): initdb + start + provisioning called; url now set
d=$(make_env | tail -1); set_linked "$d"
run "$d" ensure 2>"$d/err"; rc=$?
[ "$rc" -eq 0 ] && ok "ensure (fakes) exits 0" || bad "ensure exit=$rc"
grep -q '^initdb ' "$d/calls" && ok "ensure ran initdb" || bad "ensure did not run initdb"
grep -q '^pg_ctl .*start' "$d/calls" && ok "ensure ran pg_ctl start" || bad "ensure did not start"
grep -q '^psql .*CREATE EXTENSION IF NOT EXISTS "uuid-ossp"' "$d/calls" && ok "ensure created uuid-ossp" || bad "no uuid-ossp ext"
grep -q '^psql .*CREATE EXTENSION IF NOT EXISTS vector'      "$d/calls" && ok "ensure created vector"    || bad "no vector ext"
grep -q '^psql .*CREATE EXTENSION IF NOT EXISTS pg_trgm'     "$d/calls" && ok "ensure created pg_trgm"   || bad "no pg_trgm ext"
url=$(run "$d" url)
case "$url" in
  postgres://crm_user:*@127.0.0.1:*/personal_crm_test?sslmode=disable) ok "url after ensure has expected shape" ;;
  *) bad "url after ensure unexpected: '$url'" ;;
esac

# 8. ensure idempotent: second ensure is a no-op (no second initdb)
:> "$d/calls"
run "$d" ensure >/dev/null 2>&1
[ ! -s "$d/calls" ] && ok "second ensure is a no-op (reuse)" || bad "second ensure re-ran: $(cat "$d/calls")"

# 9. Port reuse: persisted port survives a stop (claimed, server down)
port_before=$(run "$d" port)
run "$d" stop >/dev/null 2>&1
port_after=$(run "$d" port)
[ "$port_before" = "$port_after" ] && ok "port persists across stop ($port_after)" || bad "port changed on stop: $port_before -> $port_after"

# 10. Port collision: a sibling already on the seed -> probe advances
d=$(make_env | tail -1); set_linked "$d" gamma
seed=$(run "$d" port)
# Pre-claim the seed port via a sibling meta (running pid) + mark port busy.
sib="$d/pghome/ffffffffffffffff"; mkdir -p "$sib"
printf 'PORT=%s\nPID=%s\nSTATE=running\n' "$seed" "$$" > "$sib/meta"
echo "$seed" > "$d/port_busy"
run "$d" ensure >/dev/null 2>&1
chosen=$(run "$d" port)
[ "$chosen" != "$seed" ] && ok "collision: probe advanced off seed ($seed -> $chosen)" || bad "collision: stuck on seed $seed"
[ "$chosen" != "5432" ] && ok "collision: never chose 5432" || bad "chose 5432"

# 11. ensure failure: wrong major (15) -> non-strict warn + exit 0
d=$(make_env | tail -1); set_linked "$d"; echo 15 > "$d/pg_major"
run "$d" ensure 2>"$d/err"; rc=$?
[ "$rc" -eq 0 ] && ok "wrong-major non-strict: exit 0" || bad "wrong-major non-strict exit=$rc"
grep -qi 'postgres 16' "$d/err" && ok "wrong-major: loud stderr warning" || bad "wrong-major: no warning ($(cat "$d/err"))"
[ -z "$(run "$d" url)" ] && ok "wrong-major: url empty (no instance)" || bad "wrong-major: url not empty"

# 12. ensure failure strict: wrong major -> exit non-zero
d=$(make_env | tail -1); set_linked "$d"; echo 15 > "$d/pg_major"
CRM_WORKTREE_PG=strict run "$d" ensure 2>"$d/err"; rc=$?
[ "$rc" -ne 0 ] && ok "wrong-major strict: exit non-zero ($rc)" || bad "wrong-major strict: exit 0"

# 13. ensure failure: missing locale -> loud warn, non-strict exit 0
d=$(make_env | tail -1); set_linked "$d"; touch "$d/no_locale"
run "$d" ensure 2>"$d/err"; rc=$?
[ "$rc" -eq 0 ] && ok "missing-locale non-strict: exit 0" || bad "missing-locale exit=$rc"
grep -qi 'locale' "$d/err" && ok "missing-locale: loud warning" || bad "missing-locale: no warning"

# 14. ensure failure: initdb fails -> warn + (non-strict) exit 0
d=$(make_env | tail -1); set_linked "$d"; touch "$d/initdb_fail"
run "$d" ensure 2>"$d/err"; rc=$?
[ "$rc" -eq 0 ] && ok "initdb-fail non-strict: exit 0" || bad "initdb-fail exit=$rc"
grep -qi 'initdb' "$d/err" && ok "initdb-fail: loud warning" || bad "initdb-fail: no warning"

# 15. ensure failure: pg_ctl start fails -> port claim cleared (failed-start cleanup)
d=$(make_env | tail -1); set_linked "$d"; touch "$d/pgctl_fail"
run "$d" ensure 2>"$d/err"; rc=$?
[ "$rc" -eq 0 ] && ok "start-fail non-strict: exit 0" || bad "start-fail exit=$rc"
id=$(run "$d" status | grep -oE 'id=[0-9a-f]+' | sed 's/id=//')
[ ! -f "$d/pghome/$id/meta" ] && ok "start-fail: meta cleared (port released)" || bad "start-fail: meta left: $(cat "$d/pghome/$id/meta" 2>/dev/null)"

# 16. Reaper: dead-worktree meta pruned; live-worktree meta kept; never outside HOME
d=$(make_env | tail -1); set_linked "$d" reaplive
# Make the live worktree's own instance + record it in the fake worktree list.
livegd=$(cat "$d/git-dir")
liveid=$(printf '%s' "$livegd" | { command -v sha256sum >/dev/null && sha256sum || shasum -a 256; } | awk '{print $1}' | head -c 16)
printf 'worktree %s\n' "$d/repo" > "$d/worktrees"
# Patch fake git so `git -C <wt> rev-parse --absolute-git-dir` returns the live git-dir.
cat > "$d/bin/git" <<EOF
#!/usr/bin/env bash
case "\$*" in
  "rev-parse --git-dir")          cat "$d/git-dir" ;;
  "rev-parse --git-common-dir")   cat "$d/common-dir" ;;
  "rev-parse --absolute-git-dir") cat "$d/git-dir" ;;
  "rev-parse --show-toplevel")    echo "$d/repo" ;;
  "worktree list --porcelain")    cat "$d/worktrees" ;;
  "-C $d/repo rev-parse --absolute-git-dir") cat "$d/git-dir" ;;
  *"rev-parse --absolute-git-dir") echo "/nonexistent/.git" ;;
  *) exit 0 ;;
esac
EOF
chmod +x "$d/bin/git"
mkdir -p "$d/pghome/$liveid" "$d/pghome/deadbeefdeadbeef"
printf 'PORT=5599\nPID=1\nSTATE=running\nDATADIR=%s\n' "$d/pghome/$liveid/data" > "$d/pghome/$liveid/meta"
printf 'PORT=5600\nPID=1\nSTATE=running\nDATADIR=%s\n' "$d/pghome/deadbeefdeadbeef/data" > "$d/pghome/deadbeefdeadbeef/meta"
# A sentinel OUTSIDE the home to prove reap never touches it.
sentinel="$d/SENTINEL"; touch "$sentinel"
run "$d" reap >/dev/null 2>&1
[ -d "$d/pghome/$liveid" ] && ok "reap kept live-worktree instance" || bad "reap removed live instance"
[ ! -d "$d/pghome/deadbeefdeadbeef" ] && ok "reap pruned dead-worktree instance" || bad "reap left dead instance"
[ -f "$sentinel" ] && ok "reap never touched anything outside HOME" || bad "reap removed the sentinel!"

# 17. Test-hook counter: increments once per invocation
d=$(make_env | tail -1); set_linked "$d"
ctr="$d/ctr"; :> "$ctr"
CRM_WORKTREE_PG_COUNT_FILE="$ctr" run "$d" url >/dev/null 2>&1
n=$(wc -l < "$ctr" | tr -d ' ')
[ "$n" -eq 1 ] && ok "count-file: one line per invocation" || bad "count-file: $n lines (expected 1)"

echo ""
[ "$fail" -eq 0 ] && { echo "ALL PASS (worktree-test-pg unit)"; exit 0; } || { echo "FAILURES (worktree-test-pg unit)"; exit 1; }
