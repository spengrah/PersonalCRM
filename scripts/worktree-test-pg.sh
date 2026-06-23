#!/usr/bin/env bash
# Per-worktree Postgres for cross-run integration-test isolation (gh #433, Thing 2).
#
# Each linked git worktree gets its own private Postgres CLUSTER — a plain
# `initdb`/`pg_ctl` process on a derived 127.0.0.1 port, with its own data dir,
# maintenance DB and advisory lock. The Go test harness (testdb.SetupPackage)
# then mints templates/clones inside THAT instance instead of competing with
# sibling worktrees for the shared Docker crm-postgres:5432. No container, no
# sudo, no system cluster — runs identically on macOS, the bare VPS host, and
# inside the rootless Podman sandbox (zero nesting, "never podman-in-podman").
#
# The base DB is named personal_crm_test (the harness refuses any other base),
# so isolation comes from a separate INSTANCE, not a separate DB name. No Go
# change is needed: pointing a worktree at its own instance is purely a matter
# of what DATABASE_URL/TEST_DATABASE_URL the harness sees, which the Makefile
# sets from `url` below.
#
# Subcommands:
#   url       Pure value, side-effect-FREE, render-safe. Emits the running
#             per-worktree TEST_DATABASE_URL or nothing. NEVER starts a server,
#             NEVER warns. Safe to expand during `make -n` / variable eval.
#   ensure    Side-effecting provisioner (stderr-visible). initdb + start +
#             role/db/extension provisioning. On failure: loud actionable
#             stderr + exit per CRM_WORKTREE_PG mode (non-strict: 0; strict: !=0).
#   stop      pg_ctl stop this worktree's instance (keep data dir).
#   teardown  stop + remove this worktree's data dir.
#   reap      Prune instances whose worktree no longer exists.
#   status    Human-readable state of this worktree's instance.
#   port      Print this worktree's resolved port (no side effects).
#
# Env knobs:
#   CRM_WORKTREE_PG       unset|1|auto -> auto + soft fallback (default)
#                         0            -> force shared instance (inert)
#                         strict|require -> auto, but `ensure` fails hard on error
#   CRM_WORKTREE_PG_HOME  base dir for instances (default: $XDG_CACHE_HOME/crm-test-pg
#                         else $HOME/.cache/crm-test-pg). Outside every worktree.
#   CRM_WORKTREE_PG_COUNT_FILE  test hook: append a line on every real invocation.
#   CRM_WORKTREE_PG_BINDIR      test hook: confine binary discovery to this single
#                               bindir (so a fake toolchain isn't defeated by a
#                               real pg16 install on the host).
#   CRM_WORKTREE_PG_BASE_PORT   port seed base (default 5440).
#   CRM_WORKTREE_PG_PORT_SPAN   port seed span (default 200).
set -euo pipefail

# --- Test hook: count real invocations (render-guard laziness assertion) ----
if [ -n "${CRM_WORKTREE_PG_COUNT_FILE:-}" ]; then
  echo x >> "$CRM_WORKTREE_PG_COUNT_FILE" 2>/dev/null || true
fi

BASE_PORT="${CRM_WORKTREE_PG_BASE_PORT:-5440}"
PORT_SPAN="${CRM_WORKTREE_PG_PORT_SPAN:-200}"
SHARED_PORT=5432
MAX_CONNECTIONS=200
LOCALE_NAME="en_US.UTF-8"
ENCODING="UTF8"

warn() { echo "worktree-test-pg: $*" >&2; }

# --- Mode resolution --------------------------------------------------------
# Returns the activation mode for this run: off | auto | strict.
pg_mode() {
  case "${CRM_WORKTREE_PG:-}" in
    0) echo off ;;
    strict|require) echo strict ;;
    *) echo auto ;;  # unset / 1 / auto / anything else
  esac
}

# --- Worktree detection (Fact 3) --------------------------------------------
# A linked worktree has git-dir != git-common-dir. The main checkout has them
# equal. Path-prefix-independent (survives an Orca workspace relocation).
is_linked_worktree() {
  local gd cd
  gd=$(git rev-parse --git-dir 2>/dev/null) || return 1
  cd=$(git rev-parse --git-common-dir 2>/dev/null) || return 1
  [ -n "$gd" ] && [ "$gd" != "$cd" ]
}

# Absolute git-dir for this worktree (stable + unique per worktree across life).
worktree_git_dir() {
  local gd
  gd=$(git rev-parse --absolute-git-dir 2>/dev/null) || return 1
  printf '%s' "$gd"
}

# --- Identity / paths -------------------------------------------------------
sha256_hex() {
  # Portable sha256: prefer sha256sum (Linux), fall back to shasum -a 256 (macOS).
  if command -v sha256sum >/dev/null 2>&1; then
    printf '%s' "$1" | sha256sum | awk '{print $1}'
  else
    printf '%s' "$1" | shasum -a 256 | awk '{print $1}'
  fi
}

pg_home() {
  if [ -n "${CRM_WORKTREE_PG_HOME:-}" ]; then
    printf '%s' "$CRM_WORKTREE_PG_HOME"
  elif [ -n "${XDG_CACHE_HOME:-}" ]; then
    printf '%s/crm-test-pg' "$XDG_CACHE_HOME"
  else
    printf '%s/.cache/crm-test-pg' "$HOME"
  fi
}

# Per-worktree id = first 16 hex of sha256(absolute git-dir) (64 bits).
worktree_id() {
  local gd
  gd=$(worktree_git_dir) || return 1
  sha256_hex "$gd" | head -c 16
}

# Port seed from the id (BASE_PORT + int(id[:4],16) % PORT_SPAN). The actual
# claim runs under the global allocator lock (allocate_port).
seed_port() {
  local id="$1" seed_hex seed
  seed_hex=$(printf '%s' "$id" | head -c 4)
  seed=$((0x$seed_hex % PORT_SPAN))
  echo $((BASE_PORT + seed))
}

instance_dir() { printf '%s/%s' "$(pg_home)" "$1"; }       # <home>/<id>
data_dir()     { printf '%s/%s/data' "$(pg_home)" "$1"; }  # <home>/<id>/data
meta_file()    { printf '%s/%s/meta' "$(pg_home)" "$1"; }   # <home>/<id>/meta
log_file()     { printf '%s/%s/server.log' "$(pg_home)" "$1"; }

# Unix-domain socket dir. The full socket path is <dir>/.s.PGSQL.<port> and
# Postgres caps it at ~103 bytes — a deep data dir (e.g. a nested
# CRM_WORKTREE_PG_HOME under macOS /var/folders) blows that. The harness
# connects over TCP (127.0.0.1:<port>), so the socket location is functionally
# irrelevant; we just need it SHORT (and per-instance) for pg_ctl -w + local
# psql. Use a short /tmp path keyed by id.
socket_dir() { printf '/tmp/crm-test-pg-%s' "$1"; }

# --- Meta read helpers (meta is KEY=VALUE lines: PORT, PID, DATADIR, STATE) --
meta_get() {
  # meta_get <id> <KEY>
  local mf; mf=$(meta_file "$1")
  [ -f "$mf" ] || return 1
  local line; line=$(grep -E "^$2=" "$mf" 2>/dev/null | tail -1) || return 1
  [ -n "$line" ] || return 1
  printf '%s' "${line#*=}"
}

# --- Binary discovery + validation (D5) -------------------------------------
# Resolve a Postgres 16 bindir holding initdb/pg_ctl/postgres/psql. Echoes the
# bindir on stdout; returns non-zero (and the caller warns) if none qualifies.
PG_BINDIR=""
resolve_bindir() {
  [ -n "$PG_BINDIR" ] && { printf '%s' "$PG_BINDIR"; return 0; }
  local candidates=() d
  # Test hook: when set, this is the ONLY candidate considered (so unit tests
  # with a fake bindir aren't defeated by a real pg16 install on the host).
  if [ -n "${CRM_WORKTREE_PG_BINDIR:-}" ]; then
    candidates=("$CRM_WORKTREE_PG_BINDIR")
  else
    if command -v pg_config >/dev/null 2>&1; then
      candidates+=("$(pg_config --bindir 2>/dev/null || true)")
    fi
    candidates+=(
      "${PGBIN:-}"
      "/opt/homebrew/opt/postgresql@16/bin"
      "/usr/local/opt/postgresql@16/bin"
      "/usr/lib/postgresql/16/bin"
      "/usr/pgsql-16/bin"
    )
  fi
  for d in "${candidates[@]}"; do
    [ -n "$d" ] || continue
    [ -x "$d/initdb" ] && [ -x "$d/pg_ctl" ] && [ -x "$d/postgres" ] || continue
    # Must be major 16.
    local ver
    ver=$("$d/postgres" --version 2>/dev/null | grep -oE '[0-9]+' | head -1) || true
    [ "$ver" = "16" ] || continue
    PG_BINDIR="$d"
    printf '%s' "$PG_BINDIR"
    return 0
  done
  return 1
}

# --- Password resolution (Fact 5b / D5) -------------------------------------
# Read POSTGRES_PASSWORD from .env if present, else crm_password. Returns the
# value on stdout. The value is NEVER echoed elsewhere (psql var only).
resolve_password() {
  local root pw=""
  root=$(git rev-parse --show-toplevel 2>/dev/null || true)
  if [ -n "$root" ] && [ -f "$root/.env" ]; then
    pw=$(grep -E '^POSTGRES_PASSWORD=' "$root/.env" 2>/dev/null | tail -1 | sed -E 's/^POSTGRES_PASSWORD=//' | sed -E 's/^"(.*)"$/\1/' | sed -E "s/^'(.*)'\$/\1/") || true
  fi
  [ -n "$pw" ] || pw="crm_password"
  printf '%s' "$pw"
}

# URL for this worktree's instance (no I/O; pure string from id+port).
instance_url() {
  local port="$1" pw
  pw=$(resolve_password)
  printf 'postgres://crm_user:%s@127.0.0.1:%s/personal_crm_test?sslmode=disable' "$pw" "$port"
}

# --- Liveness ---------------------------------------------------------------
pid_alive() { [ -n "${1:-}" ] && kill -0 "$1" 2>/dev/null; }

port_answers() {
  # True if something accepts a TCP connection on 127.0.0.1:<port>.
  local port="$1"
  local bindir; bindir=$(resolve_bindir 2>/dev/null || true)
  if [ -n "$bindir" ] && [ -x "$bindir/pg_isready" ]; then
    "$bindir/pg_isready" -h 127.0.0.1 -p "$port" >/dev/null 2>&1 && return 0
  fi
  # Fallback: bash /dev/tcp probe.
  (exec 3<>"/dev/tcp/127.0.0.1/$port") >/dev/null 2>&1 && { exec 3>&- 2>/dev/null || true; return 0; }
  return 1
}

instance_running() {
  # instance_running <id>: true if meta records a live pid AND the port answers.
  local id="$1" pid port
  pid=$(meta_get "$id" PID 2>/dev/null || true)
  port=$(meta_get "$id" PORT 2>/dev/null || true)
  [ -n "$port" ] || return 1
  pid_alive "$pid" && port_answers "$port"
}

# --- Locking (D1) -----------------------------------------------------------
# flock when available (Linux util-linux, macOS if /usr/bin/flock exists); else
# a portable mkdir-based lock with a stale breaker. The lock body runs as a
# callback so both backends share one call shape.
LOCK_STALE_SECS=120

with_lock() {
  # with_lock <lockpath> <command...>
  local lockpath="$1"; shift
  mkdir -p "$(dirname "$lockpath")"
  if command -v flock >/dev/null 2>&1; then
    # Fixed fd 200 (not the `{fd}>` auto-alloc form, which is unsupported in
    # bash 3.2 — macOS's default bash — where it errors `exec: {fd}: not found`).
    local rc=0
    {
      flock 200
      "$@" || rc=$?
    } 200>"$lockpath"
    return $rc
  fi
  # mkdir-based fallback (macOS default has no flock).
  local dir="$lockpath.d" waited=0
  while ! mkdir "$dir" 2>/dev/null; do
    # Stale breaker: a lock dir older than LOCK_STALE_SECS whose holder pid is
    # dead is reclaimable.
    if [ -f "$dir/pid" ]; then
      local holder age now mtime
      holder=$(cat "$dir/pid" 2>/dev/null || true)
      now=$(date +%s)
      mtime=$(stat -f %m "$dir" 2>/dev/null || stat -c %Y "$dir" 2>/dev/null || echo "$now")
      age=$((now - mtime))
      if [ "$age" -gt "$LOCK_STALE_SECS" ] && ! pid_alive "$holder"; then
        rm -rf "$dir" 2>/dev/null || true
        continue
      fi
    fi
    sleep 0.2
    waited=$((waited + 1))
    if [ "$waited" -gt 600 ]; then  # ~120s ceiling
      warn "timed out acquiring lock $lockpath"
      return 1
    fi
  done
  echo "$$" > "$dir/pid" 2>/dev/null || true
  local rc=0
  "$@" || rc=$?
  rm -rf "$dir" 2>/dev/null || true
  return $rc
}

# --- Port allocation (global allocator lock, D1) ----------------------------
# Picks (or reuses) this worktree's port and writes PORT + STATE=claimed to its
# meta, all under the GLOBAL allocator lock so two distinct worktrees can never
# claim the same port. Echoes the chosen port. MUST be called under no other
# lock (it takes the global lock itself).
allocate_port() {
  local id="$1"
  with_lock "$(pg_home)/.alloc.lock" _allocate_port_locked "$id"
}

_allocate_port_locked() {
  local id="$1" seed p
  # Reuse a persisted port if this worktree already has one recorded.
  local existing; existing=$(meta_get "$id" PORT 2>/dev/null || true)
  if [ -n "$existing" ]; then
    printf '%s' "$existing"
    return 0
  fi
  seed=$(seed_port "$id")
  p=$seed
  while [ "$p" -lt $((BASE_PORT + PORT_SPAN + 64)) ]; do
    if [ "$p" -ne "$SHARED_PORT" ] && ! port_claimed_by_sibling "$id" "$p" && ! port_answers "$p"; then
      write_meta_claim "$id" "$p"
      printf '%s' "$p"
      return 0
    fi
    p=$((p + 1))
  done
  warn "no free port in range $BASE_PORT-$((BASE_PORT + PORT_SPAN)) for this worktree"
  return 1
}

# True if some OTHER worktree's meta records port <p>. CONSERVATIVE BY DESIGN:
# any sibling meta with PORT=p reserves the port, whether it is running,
# claimed-and-starting, or stopped-but-persisted-for-restart. We do NOT
# opportunistically reclaim a "looks dead" sibling claim here — that was a race:
# a freshly-claimed sibling (STATE=claimed, no PID yet, server not yet
# listening) looks dead in the gap between its meta-write and its pg_ctl start,
# so a second worktree could steal the same port. A sibling's own failed start
# clears ITS OWN meta (clear_meta_claim), and a sibling whose WORKTREE is gone is
# pruned by `reap` — so a conservative reservation never permanently leaks a
# port, and never double-allocates one.
port_claimed_by_sibling() {
  local self_id="$1" p="$2" mf other_id other_port
  shopt -s nullglob
  for mf in "$(pg_home)"/*/meta; do
    other_id=$(basename "$(dirname "$mf")")
    [ "$other_id" = "$self_id" ] && continue
    other_port=$(grep -E '^PORT=' "$mf" 2>/dev/null | tail -1); other_port="${other_port#*=}"
    if [ "$other_port" = "$p" ]; then
      shopt -u nullglob
      return 0
    fi
  done
  shopt -u nullglob
  return 1
}

write_meta_claim() {
  local id="$1" port="$2" mf; mf=$(meta_file "$id")
  mkdir -p "$(dirname "$mf")"
  {
    echo "PORT=$port"
    echo "DATADIR=$(data_dir "$id")"
    echo "STATE=claimed"
  } > "$mf"
}

write_meta_running() {
  local id="$1" port="$2" pid="$3" mf; mf=$(meta_file "$id")
  mkdir -p "$(dirname "$mf")"
  {
    echo "PORT=$port"
    echo "PID=$pid"
    echo "DATADIR=$(data_dir "$id")"
    echo "STATE=running"
  } > "$mf"
}

# Failed-start cleanup: clear PORT/STATE so the port is not permanently held.
clear_meta_claim() {
  local id="$1" mf; mf=$(meta_file "$id")
  rm -f "$mf" 2>/dev/null || true
}

# ===========================================================================
# Subcommand: url  (pure value, side-effect-free, render-safe)
# ===========================================================================
cmd_url() {
  [ "$(pg_mode)" = off ] && return 0
  [ "${GITHUB_ACTIONS:-}" = "true" ] && return 0
  is_linked_worktree || return 0
  local id; id=$(worktree_id) || return 0
  instance_running "$id" || return 0   # not yet ensured -> emit nothing
  local port; port=$(meta_get "$id" PORT) || return 0
  instance_url "$port"
}

# ===========================================================================
# Subcommand: ensure  (side-effecting provisioner, stderr-visible)
# ===========================================================================
cmd_ensure() {
  local mode; mode=$(pg_mode)
  # Inert cases: not active. No warning (these are normal, not failures).
  [ "$mode" = off ] && return 0
  [ "${GITHUB_ACTIONS:-}" = "true" ] && return 0
  is_linked_worktree || return 0

  local id; id=$(worktree_id) || { _ensure_fail "$mode" "could not derive worktree id"; return $?; }
  # Serialize same-worktree ensure/stop/teardown under the per-worktree lock.
  with_lock "$(instance_dir "$id")/lock" _ensure_locked "$id" "$mode"
}

_ensure_locked() {
  local id="$1" mode="$2"

  # Already up? no-op (reuse, D7).
  if instance_running "$id"; then
    return 0
  fi

  # Preconditions (D5): binaries (major 16), locale, then provision.
  local bindir
  if ! bindir=$(resolve_bindir 2>/dev/null); then
    _ensure_fail "$mode" \
      "no Postgres 16 toolchain found (need initdb/pg_ctl/postgres major 16 with pgvector). Install via 'make setup' (brew install postgresql@16 pgvector). Falling back to the shared instance."
    return $?
  fi
  if ! locale_present; then
    _ensure_fail "$mode" \
      "locale '$LOCALE_NAME' not available (locale -a). Generate it (Linux: 'sudo locale-gen $LOCALE_NAME' or 'localedef -i en_US -f UTF-8 $LOCALE_NAME'); macOS ships it by default. Falling back to the shared instance."
    return $?
  fi

  local datadir; datadir=$(data_dir "$id")

  # initdb if the cluster isn't initialized yet. The log goes to a SIBLING of
  # the data dir — initdb refuses a non-empty -D target, so we must not write
  # anything (not even the log) inside $datadir before it runs. initdb creates
  # $datadir itself.
  if [ ! -s "$datadir/PG_VERSION" ]; then
    local initlog; initlog="$(instance_dir "$id")/initdb.log"
    mkdir -p "$(instance_dir "$id")"
    rm -rf "$datadir" 2>/dev/null || true   # clear a partial/failed prior init
    if ! "$bindir/initdb" --encoding="$ENCODING" --locale="$LOCALE_NAME" \
        -U "$(id -un)" -D "$datadir" >/dev/null 2>"$initlog"; then
      warn "initdb failed (see $initlog)"
      _ensure_fail "$mode" "initdb failed for $datadir. Falling back to the shared instance."
      return $?
    fi
  fi

  # Claim a port (global allocator lock) and start.
  local port
  if ! port=$(allocate_port "$id"); then
    _ensure_fail "$mode" "port allocation failed. Falling back to the shared instance."
    return $?
  fi

  local logf sockdir; logf=$(log_file "$id"); sockdir=$(socket_dir "$id")
  mkdir -p "$sockdir"
  if ! "$bindir/pg_ctl" -D "$datadir" -w -t 30 \
      -o "-p $port -c max_connections=$MAX_CONNECTIONS -c listen_addresses=127.0.0.1 -c unix_socket_directories=$sockdir" \
      -l "$logf" start >/dev/null 2>&1; then
    warn "pg_ctl start failed on port $port (see $logf)"
    clear_meta_claim "$id"   # failed-start cleanup: release the port claim
    _ensure_fail "$mode" "Postgres failed to start. Falling back to the shared instance."
    return $?
  fi

  # Record the running pid+port.
  local pid
  pid=$(head -1 "$datadir/postmaster.pid" 2>/dev/null || true)
  write_meta_running "$id" "$port" "$pid"

  # Provision role + base DB + the three extensions.
  if ! provision_instance "$bindir" "$port"; then
    warn "provisioning role/db/extensions failed on port $port"
    "$bindir/pg_ctl" -D "$datadir" -m fast stop >/dev/null 2>&1 || true
    clear_meta_claim "$id"
    _ensure_fail "$mode" "provisioning failed. Falling back to the shared instance."
    return $?
  fi
  return 0
}

# Emit the loud actionable warning and exit per mode. Returns the desired exit
# code so callers can `return $?` to propagate it.
_ensure_fail() {
  local mode="$1" msg="$2"
  warn "$msg"
  if [ "$mode" = strict ]; then
    return 1
  fi
  return 0   # non-strict: degrade to shared instance, do not fail the run.
}

locale_present() {
  locale -a 2>/dev/null | tr 'A-Z' 'a-z' | tr -d '-' | grep -q 'en_us.utf8'
}

# Create role crm_user (SUPERUSER) + base DB personal_crm_test + extensions.
# Connects to the maintenance DB as the bootstrap superuser (the OS user from
# initdb). The password is passed via a psql variable and interpolated with
# :'pw' (which server-side-quotes/escapes it, so quotes/backslashes/$ cannot
# break the statement or inject SQL). CRITICAL: psql performs :'pw' substitution
# ONLY on SQL read from stdin/-f, NOT on -c command strings — and never inside a
# $do$...$do$ dollar-quoted body. So the role SQL is fed via STDIN (a here-doc),
# as a plain CREATE/ALTER chosen by a prior existence check (no DO block).
provision_instance() {
  local bindir="$1" port="$2" pw
  pw=$(resolve_password)
  local psql=("$bindir/psql" -v ON_ERROR_STOP=1 -h 127.0.0.1 -p "$port" -U "$(id -un)" -d postgres -X -q)

  # Role: create-or-alter as SUPERUSER LOGIN with the resolved password.
  local role_exists
  role_exists=$("${psql[@]}" -tAc "SELECT 1 FROM pg_roles WHERE rolname='crm_user'" 2>/dev/null | tr -dc '01') || true
  if [ "$role_exists" = "1" ]; then
    "${psql[@]}" -v pw="$pw" <<'SQL' >/dev/null 2>&1 || return 1
ALTER ROLE crm_user WITH SUPERUSER LOGIN PASSWORD :'pw';
SQL
  else
    "${psql[@]}" -v pw="$pw" <<'SQL' >/dev/null 2>&1 || return 1
CREATE ROLE crm_user WITH SUPERUSER LOGIN PASSWORD :'pw';
SQL
  fi

  # Base DB personal_crm_test (owned by crm_user) if absent.
  local exists
  exists=$("${psql[@]}" -tAc "SELECT 1 FROM pg_database WHERE datname='personal_crm_test'" 2>/dev/null | tr -dc '01') || true
  if [ "$exists" != "1" ]; then
    "${psql[@]}" -c "CREATE DATABASE personal_crm_test OWNER crm_user" >/dev/null 2>&1 || return 1
  fi

  # The three extensions the schema needs (Fact 12): uuid-ossp, vector, pg_trgm.
  local dbpsql=("$bindir/psql" -v ON_ERROR_STOP=1 -h 127.0.0.1 -p "$port" -U "$(id -un)" -d personal_crm_test -X -q)
  "${dbpsql[@]}" -c 'CREATE EXTENSION IF NOT EXISTS "uuid-ossp";' >/dev/null 2>&1 || return 1
  "${dbpsql[@]}" -c 'CREATE EXTENSION IF NOT EXISTS vector;'      >/dev/null 2>&1 || return 1
  "${dbpsql[@]}" -c 'CREATE EXTENSION IF NOT EXISTS pg_trgm;'     >/dev/null 2>&1 || return 1
  return 0
}

# ===========================================================================
# Subcommand: stop
# ===========================================================================
cmd_stop() {
  is_linked_worktree || { warn "not a linked worktree; nothing to stop"; return 0; }
  local id; id=$(worktree_id) || return 0
  with_lock "$(instance_dir "$id")/lock" _stop_locked "$id"
}

_stop_locked() {
  local id="$1" datadir; datadir=$(data_dir "$id")
  local bindir; bindir=$(resolve_bindir 2>/dev/null || true)
  if [ -n "$bindir" ] && [ -s "$datadir/PG_VERSION" ]; then
    "$bindir/pg_ctl" -D "$datadir" -m fast stop >/dev/null 2>&1 || true
  fi
  # Demote meta to claimed (port persisted for fast restart, server not running).
  local port; port=$(meta_get "$id" PORT 2>/dev/null || true)
  if [ -n "$port" ]; then
    write_meta_claim "$id" "$port"
  fi
  return 0
}

# ===========================================================================
# Subcommand: teardown
# ===========================================================================
cmd_teardown() {
  is_linked_worktree || { warn "not a linked worktree; nothing to tear down"; return 0; }
  local id; id=$(worktree_id) || return 0
  with_lock "$(instance_dir "$id")/lock" _teardown_locked "$id"
}

_teardown_locked() {
  local id="$1"
  _stop_locked "$id"
  rm -rf "$(instance_dir "$id")" "$(socket_dir "$id")" 2>/dev/null || true
}

# ===========================================================================
# Subcommand: reap  (prune instances whose worktree no longer exists)
# ===========================================================================
cmd_reap() {
  local home; home=$(pg_home)
  [ -d "$home" ] || return 0
  # Compute the id of every LIVE worktree.
  local live_ids="" gd
  while IFS= read -r line; do
    case "$line" in
      worktree\ *)
        local wt="${line#worktree }"
        # GIT_DIR= clears an inherited GIT_DIR so `git -C <wt>` resolves the
        # target worktree's own git-dir, not this process's.
        # shellcheck disable=SC1007
        gd=$(GIT_DIR= git -C "$wt" rev-parse --absolute-git-dir 2>/dev/null || true)
        [ -n "$gd" ] && live_ids="$live_ids $(sha256_hex "$gd" | head -c 16)"
        ;;
    esac
  done < <(git worktree list --porcelain 2>/dev/null || true)

  local bindir; bindir=$(resolve_bindir 2>/dev/null || true)
  local mf id
  shopt -s nullglob
  for mf in "$home"/*/meta; do
    id=$(basename "$(dirname "$mf")")
    case " $live_ids " in
      *" $id "*) continue ;;   # still live; keep
    esac
    # Dead worktree: stop (if running) + remove the data dir. Operates ONLY
    # under $home — never :5432, never Docker.
    local datadir; datadir=$(data_dir "$id")
    if [ -n "$bindir" ] && [ -s "$datadir/PG_VERSION" ]; then
      "$bindir/pg_ctl" -D "$datadir" -m fast stop >/dev/null 2>&1 || true
    fi
    rm -rf "$(instance_dir "$id")" "$(socket_dir "$id")" 2>/dev/null || true
    echo "reaped per-worktree pg instance $id (worktree gone)"
  done
  shopt -u nullglob
}

# ===========================================================================
# Subcommand: status / port
# ===========================================================================
cmd_status() {
  if ! is_linked_worktree; then echo "main checkout (uses shared crm-postgres:5432)"; return 0; fi
  local id; id=$(worktree_id) || { echo "could not derive worktree id"; return 0; }
  local port; port=$(meta_get "$id" PORT 2>/dev/null || true)
  if [ -z "$port" ]; then echo "id=$id not provisioned"; return 0; fi
  if instance_running "$id"; then
    echo "id=$id port=$port RUNNING datadir=$(data_dir "$id")"
  else
    echo "id=$id port=$port stopped datadir=$(data_dir "$id")"
  fi
}

cmd_port() {
  is_linked_worktree || return 0
  local id; id=$(worktree_id) || return 0
  meta_get "$id" PORT 2>/dev/null || seed_port "$id"
}

# ===========================================================================
# Dispatch
# ===========================================================================
main() {
  local sub="${1:-}"
  case "$sub" in
    url)      cmd_url ;;
    ensure)   cmd_ensure ;;
    stop)     cmd_stop ;;
    teardown) cmd_teardown ;;
    reap)     cmd_reap ;;
    status)   cmd_status ;;
    port)     cmd_port ;;
    *)
      echo "usage: worktree-test-pg.sh {url|ensure|stop|teardown|reap|status|port}" >&2
      return 2
      ;;
  esac
}

main "$@"
