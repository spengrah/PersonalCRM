#!/usr/bin/env bash
# Per-worktree Postgres for cross-run integration-test isolation (gh #433).
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
#   CRM_WORKTREE_PG_LOCK_TIMEOUT  seconds to wait for a per-instance lock before
#                               failing LOUD instead of blocking forever (default
#                               120). Positive integer; empty/zero/negative/non-
#                               numeric values warn and fall back to 120. Mainly a
#                               test hook. On timeout `ensure` degrades per mode
#                               (proceeds against a running instance, else falls
#                               back to shared); stop/teardown/reap fail loud.
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

# --- Worktree detection ------------------------------------------------------
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

# --- Binary discovery + validation ------------------------------------------
# Resolve a Postgres 16 bindir holding initdb/pg_ctl/postgres/psql. Echoes the
# bindir on stdout; returns non-zero (and the caller warns) if none qualifies.
PG_BINDIR=""

# Candidate Postgres bindirs, most-specific first. Single source of truth for
# both resolve_bindir (full toolchain) and resolve_pg_ctl (pg_ctl only). Honors
# the CRM_WORKTREE_PG_BINDIR test hook (then it is the ONLY candidate).
pg_bin_candidates() {
  if [ -n "${CRM_WORKTREE_PG_BINDIR:-}" ]; then
    printf '%s\n' "$CRM_WORKTREE_PG_BINDIR"
    return 0
  fi
  command -v pg_config >/dev/null 2>&1 && printf '%s\n' "$(pg_config --bindir 2>/dev/null || true)"
  printf '%s\n' \
    "${PGBIN:-}" \
    "/opt/homebrew/opt/postgresql@16/bin" \
    "/usr/local/opt/postgresql@16/bin" \
    "/usr/lib/postgresql/16/bin" \
    "/usr/pgsql-16/bin"
}

resolve_bindir() {
  [ -n "$PG_BINDIR" ] && { printf '%s' "$PG_BINDIR"; return 0; }
  local d ver
  while IFS= read -r d; do
    [ -n "$d" ] || continue
    # psql is required for provisioning + the warm-instance password reconcile.
    [ -x "$d/initdb" ] && [ -x "$d/pg_ctl" ] && [ -x "$d/postgres" ] && [ -x "$d/psql" ] || continue
    ver=$("$d/postgres" --version 2>/dev/null | grep -oE '[0-9]+' | head -1) || true
    [ "$ver" = "16" ] || continue
    PG_BINDIR="$d"; printf '%s' "$PG_BINDIR"; return 0
  done < <(pg_bin_candidates)
  return 1
}

# Resolve JUST a pg_ctl executable, INDEPENDENTLY of resolve_bindir's full
# initdb/postgres/psql requirement. Used only to force-stop a warm instance when
# reconcile failed — psql may be the very binary that's missing, and stopping a
# running server needs only pg_ctl. Prefers the already-resolved full bindir (a
# cached PG_BINDIR is already major-16-validated); when scanning fresh it
# validates the pg_ctl is major 16 too, so a non-16 pg_config/PGBIN ahead of the
# real pg16 bindir can't hand back the wrong control binary.
resolve_pg_ctl() {
  [ -n "$PG_BINDIR" ] && [ -x "$PG_BINDIR/pg_ctl" ] && { printf '%s' "$PG_BINDIR/pg_ctl"; return 0; }
  local d ver
  while IFS= read -r d; do
    [ -n "$d" ] && [ -x "$d/pg_ctl" ] || continue
    ver=$("$d/pg_ctl" --version 2>/dev/null | grep -oE '[0-9]+' | head -1) || true
    [ "$ver" = "16" ] || continue
    printf '%s' "$d/pg_ctl"; return 0
  done < <(pg_bin_candidates)
  return 1
}

# --- Password resolution ----------------------------------------------------
# The per-worktree test cluster's crm_user password. Deliberately a FIXED test
# credential (the same literal the Makefile's shared-instance TEST_DATABASE_URL
# default already embeds) — NOT sourced from .env. Rationale: the cluster is a
# throwaway, loopback-only instance we provision ourselves, so we set this
# role's password to whatever we like; using a fixed safe-to-print value means a
# real/prod POSTGRES_PASSWORD in .env can NEVER leak into the per-worktree
# DATABASE_URL that a routine `make -n` dry run prints. It also keeps the
# per-worktree URL byte-identical (modulo host:port) to the shared default.
resolve_password() {
  printf '%s' "crm_password"
}

# Percent-encode a string for the userinfo component of a URI, so a password
# containing URI-reserved chars (/ @ : ? # % & + space, etc. — e.g. a base64
# password, which can include /) does not corrupt the postgres:// URL. Encodes
# every BYTE that is not an RFC 3986 unreserved char (A-Z a-z 0-9 - _ . ~). The
# loop runs under LC_ALL=C so ${#s}/${s:i:1} operate on single bytes — a
# multibyte (UTF-8) password is then percent-encoded byte-by-byte correctly,
# not mangled by per-character iteration.
urlencode() {
  local s="$1" i c out="" n
  local LC_ALL=C
  for ((i = 0; i < ${#s}; i++)); do
    c="${s:i:1}"
    case "$c" in
      [A-Za-z0-9._~-]) out+="$c" ;;
      # printf "'$c" yields the byte's numeric value, but high bytes (>=0x80)
      # come back sign-extended (negative) — mask to the low 8 bits before %02X.
      *) printf -v n '%d' "'$c"; out+=$(printf '%%%02X' "$(( n & 0xFF ))") ;;
    esac
  done
  printf '%s' "$out"
}

# URL for this worktree's instance (no I/O; pure string from id+port). The
# password is the same loopback dev credential the shared-instance Makefile
# default embeds; it is percent-encoded so reserved chars can't corrupt the URL.
instance_url() {
  local port="$1" pw
  pw=$(urlencode "$(resolve_password)")
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

# --- Locking ----------------------------------------------------------------
# flock when available (Linux util-linux, macOS if /usr/bin/flock exists); else
# a portable mkdir-based lock with a stale breaker. The lock body runs as a
# callback so both backends share one call shape.
LOCK_STALE_SECS=120

# Seconds to wait for a per-instance lock before failing LOUD (rc 124) instead
# of blocking forever. Resolved ONCE, at top-level scope (NOT local to with_lock)
# so both lock backends AND cmd_ensure's rc-124 messages can reference it without
# tripping `set -u`. rc 124 is the timeout sentinel (matches timeout(1)); the
# locked callbacks (_ensure_locked/_stop_locked/_teardown_locked/_reap_one) return
# only 0/1 today, so 124 can't collide — a future callback returning 124 would be
# misread as a lock timeout.
LOCK_TIMEOUT_SECS=120
case "${CRM_WORKTREE_PG_LOCK_TIMEOUT:-}" in
  '') ;;                                       # unset/empty -> default
  *[!0-9]*)                                    # negative / non-numeric
    warn "ignoring invalid CRM_WORKTREE_PG_LOCK_TIMEOUT='${CRM_WORKTREE_PG_LOCK_TIMEOUT}' (want a positive integer); using ${LOCK_TIMEOUT_SECS}s" ;;
  *)                                           # all-digits, non-empty
    if [ "$CRM_WORKTREE_PG_LOCK_TIMEOUT" -gt 0 ]; then
      LOCK_TIMEOUT_SECS="$CRM_WORKTREE_PG_LOCK_TIMEOUT"
    else
      warn "ignoring invalid CRM_WORKTREE_PG_LOCK_TIMEOUT='${CRM_WORKTREE_PG_LOCK_TIMEOUT}' (want a positive integer); using ${LOCK_TIMEOUT_SECS}s"
    fi ;;
esac

# Loud, actionable warning shared by both lock backends on a wait timeout. Names
# the DIRECT pg_ctl recovery: stop/teardown/reap all contend on this same lock,
# so they cannot recover a leaked one.
_lock_timeout_warn() {
  warn "timed out after ${LOCK_TIMEOUT_SECS}s waiting for lock $1; if a per-worktree postmaster leaked this lock, stop it DIRECTLY: pg_ctl -D <datadir> -m fast stop (make test-pg-teardown/stop would block on this same lock)."
}

with_lock() {
  # with_lock <lockpath> <command...>. Returns the callback's rc, or 124 if the
  # lock could not be acquired within LOCK_TIMEOUT_SECS (fail loud, never wedge).
  local lockpath="$1"; shift
  mkdir -p "$(dirname "$lockpath")"
  if command -v flock >/dev/null 2>&1; then
    # Fixed fd 200 (not the `{fd}>` auto-alloc form, which is unsupported in
    # bash 3.2 — macOS's default bash — where it errors `exec: {fd}: not found`).
    local rc=0
    {
      if flock -w "$LOCK_TIMEOUT_SECS" 200; then
        "$@" || rc=$?
      else
        _lock_timeout_warn "$lockpath"
        rc=124
      fi
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
    # Ceiling derived from the same knob: 0.2s steps => 5 iterations/second.
    if [ "$waited" -gt $((LOCK_TIMEOUT_SECS * 5)) ]; then
      _lock_timeout_warn "$lockpath"
      return 124
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
# claimed-and-starting, or stopped-but-persisted-for-restart. It deliberately
# does NOT try to reclaim a "looks dead" sibling claim: a freshly-claimed
# sibling (STATE=claimed, no PID yet, server not yet listening) is
# indistinguishable from a dead one in the window between its meta-write and its
# pg_ctl start, so reclaiming on "looks dead" would let two worktrees pick the
# same port. Reclamation is handled by the owners instead: a worktree's own
# failed start clears ITS OWN meta (clear_meta_claim), and a worktree that no
# longer exists is pruned by `reap` — so a conservative reservation neither
# permanently leaks a port nor double-allocates one.
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
# Subcommand: active  (pure boolean, side-effect-free, render-safe)
# Echoes "1" iff `ensure` would provision a per-worktree instance (active
# linked worktree, not CI, not opt-out); else nothing. Lets the Makefile decide
# at expansion time whether to EMIT the ensure recipe command at all, so the
# main-checkout / forced-shared / CI render stays byte-identical (the ensure
# line is not even printed by `make -n` when inactive).
# ===========================================================================
cmd_active() {
  [ "$(pg_mode)" = off ] && return 0
  [ "${GITHUB_ACTIONS:-}" = "true" ] && return 0
  is_linked_worktree || return 0
  printf '1'
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
  # Capture rc (|| rc=$? keeps set -e from aborting on a non-zero return) so a
  # lock timeout (124) maps to an HONEST outcome instead of hard-failing the
  # Makefile ensure prereq even in non-strict mode.
  local rc=0
  with_lock "$(instance_dir "$id")/lock" _ensure_locked "$id" "$mode" || rc=$?
  if [ "$rc" -eq 124 ]; then
    if instance_running "$id"; then
      # Lock busy but the server is up and serving. url reads meta lock-free and
      # emits this instance's URL regardless, so the only skipped work is the
      # idempotent password reconcile (a stale password surfaces as an auth
      # failure in the tests, not silently). NON-STRICT: proceed against it.
      # STRICT: fail — an expired wait on a running instance signals a leaked
      # lock or an abnormally long sibling ensure, both worth failing over.
      # Mode-specific wording so strict stderr does not claim "proceeding".
      if [ "$mode" = strict ]; then
        _ensure_fail "$mode" "instance lock busy after ${LOCK_TIMEOUT_SECS}s with the server up; strict mode fails rather than proceed unreconciled. If this repeats: pg_ctl -D $(data_dir "$id") -m fast stop"
      else
        _ensure_fail "$mode" "instance lock busy after ${LOCK_TIMEOUT_SECS}s but the server is up; proceeding against it. If this repeats: pg_ctl -D $(data_dir "$id") -m fast stop"
      fi
      return $?
    fi
    # Not running: the fallback claim is genuinely true — url emits nothing and
    # the Makefile falls back to the shared instance.
    _ensure_fail "$mode" "timed out waiting for the instance lock and no server is running; falling back to the shared instance. Recover: pg_ctl -D $(data_dir "$id") -m fast stop (make test-pg-teardown would block on the same lock)."
    return $?
  fi
  return $rc
}

_ensure_locked() {
  local id="$1" mode="$2"

  # Already up? Reuse — but RECONCILE the crm_user password to the current
  # fixed credential first. A warm instance may have been provisioned by an
  # older version that sourced .env, so its role password could differ from the
  # crm_password the URL now emits; without this re-assert, the recipe's DSN
  # would fail to authenticate against the still-running cluster. The ALTER is
  # idempotent and cheap (local loopback). Reconcile is REQUIRED, not
  # best-effort: if it fails (psql missing, SQL error) the running instance is
  # left with a password the URL won't match, so we must NOT emit it. Stop the
  # instance + clear its meta so `url` returns nothing (→ recipe falls back to
  # the shared instance), then take the D4 warn/exit path (hard-fail in strict).
  if instance_running "$id"; then
    local port bindir; port=$(meta_get "$id" PORT 2>/dev/null || true)
    bindir=$(resolve_bindir 2>/dev/null || true)
    if [ -z "$bindir" ] || [ -z "$port" ] || ! reconcile_role_password "$bindir" "$port"; then
      # Attempt to force the server down before clearing meta: if we cleared meta
      # while the postmaster stayed up, the next ensure would cold-start against a
      # live data dir ("lock file already exists") and wedge. force_stop_instance
      # resolves pg_ctl independently of psql, so it also covers the "psql
      # missing" entry (bindir empty) — a bindir-gated stop would skip it. We
      # clear meta REGARDLESS of the stop result (`|| true`): meta is cheap and
      # reversible, and clearing it enables graceful shared-instance fallback; a
      # residual un-stoppable postmaster is the documented wedge, recoverable via
      # `make test-pg-teardown`. (Contrast teardown/reap, which gate the
      # DESTRUCTIVE rm on a verified stop.)
      force_stop_instance "$id" || true
      clear_meta_claim "$id"
      _ensure_fail "$mode" "could not reconcile the per-worktree crm_user password on the warm instance; attempted to force-stop it and falling back to the shared instance."
      return $?
    fi
    return 0
  fi

  # Preconditions: binaries (major 16), locale, then provision.
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
  # Close fd 200 (the per-instance lock held by the enclosing with_lock) for
  # pg_ctl and, crucially, for the postmaster it daemonizes. flock locks live on
  # the OPEN FILE DESCRIPTION, so a postmaster that inherited fd 200 would hold
  # this instance's lock for its ENTIRE lifetime — wedging every later ensure/
  # stop/teardown/reap on that instance. `200>&-` closes it only for this child;
  # the shell's own fd 200 (the lock holder) is untouched.
  if ! "$bindir/pg_ctl" -D "$datadir" -w -t 30 \
      -o "-p $port -c max_connections=$MAX_CONNECTIONS -c listen_addresses=127.0.0.1 -c unix_socket_directories=$sockdir" \
      -l "$logf" start >/dev/null 2>&1 200>&-; then
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

# Create-or-alter role crm_user (SUPERUSER LOGIN) with the resolved password,
# against the maintenance DB as the bootstrap superuser (the OS user from
# initdb). Idempotent — safe to call on a cold OR a warm instance (the latter to
# reconcile a stale password). The password is handled so it is BOTH
# SQL-injection-safe AND never on a command line (not in this script's argv, not
# in the spawned psql's argv — so it can't leak via `ps auxww`): it is exported
# as an environment variable that psql pulls into a variable with `\getenv`,
# then interpolated with :'pw' (which server-side-quotes/escapes it). psql
# performs :'pw' substitution ONLY on SQL read from stdin/-f (NOT on -c, NOT
# inside a $do$ body), so the role SQL is fed via a here-doc, as a plain
# CREATE/ALTER chosen by a prior existence check.
reconcile_role_password() {
  local bindir="$1" port="$2"
  local psql=("$bindir/psql" -v ON_ERROR_STOP=1 -h 127.0.0.1 -p "$port" -U "$(id -un)" -d postgres -X -q)
  local CRM_WORKTREE_PG_PW; CRM_WORKTREE_PG_PW=$(resolve_password)
  export CRM_WORKTREE_PG_PW
  local role_exists rc=0
  role_exists=$("${psql[@]}" -tAc "SELECT 1 FROM pg_roles WHERE rolname='crm_user'" 2>/dev/null | tr -dc '01') || true
  if [ "$role_exists" = "1" ]; then
    "${psql[@]}" <<'SQL' >/dev/null 2>&1 || rc=1
\getenv pw CRM_WORKTREE_PG_PW
ALTER ROLE crm_user WITH SUPERUSER LOGIN PASSWORD :'pw';
SQL
  else
    "${psql[@]}" <<'SQL' >/dev/null 2>&1 || rc=1
\getenv pw CRM_WORKTREE_PG_PW
CREATE ROLE crm_user WITH SUPERUSER LOGIN PASSWORD :'pw';
SQL
  fi
  unset CRM_WORKTREE_PG_PW
  return $rc
}

# Create role crm_user (SUPERUSER) + base DB personal_crm_test + extensions.
provision_instance() {
  local bindir="$1" port="$2"
  local psql=("$bindir/psql" -v ON_ERROR_STOP=1 -h 127.0.0.1 -p "$port" -U "$(id -un)" -d postgres -X -q)

  # Role: create-or-alter as SUPERUSER LOGIN with the resolved password.
  reconcile_role_password "$bindir" "$port" || return 1

  # Base DB personal_crm_test (owned by crm_user) if absent.
  local exists
  exists=$("${psql[@]}" -tAc "SELECT 1 FROM pg_database WHERE datname='personal_crm_test'" 2>/dev/null | tr -dc '01') || true
  if [ "$exists" != "1" ]; then
    "${psql[@]}" -c "CREATE DATABASE personal_crm_test OWNER crm_user" >/dev/null 2>&1 || return 1
  fi

  # The three extensions the schema needs: uuid-ossp, vector, pg_trgm.
  local dbpsql=("$bindir/psql" -v ON_ERROR_STOP=1 -h 127.0.0.1 -p "$port" -U "$(id -un)" -d personal_crm_test -X -q)
  "${dbpsql[@]}" -c 'CREATE EXTENSION IF NOT EXISTS "uuid-ossp";' >/dev/null 2>&1 || return 1
  "${dbpsql[@]}" -c 'CREATE EXTENSION IF NOT EXISTS vector;'      >/dev/null 2>&1 || return 1
  "${dbpsql[@]}" -c 'CREATE EXTENSION IF NOT EXISTS pg_trgm;'     >/dev/null 2>&1 || return 1
  return 0
}

# ===========================================================================
# Force-stop helper (shared by the warm-failure block, teardown, and reap)
# ===========================================================================
# True iff the instance's postmaster is still alive (reads postmaster.pid, which
# real pg_ctl removes on a clean stop; a stale/dead pid reads as not-alive).
postmaster_alive() {
  local datadir="$1" pid
  pid=$(head -1 "$datadir/postmaster.pid" 2>/dev/null || true)
  [ -n "$pid" ] && pid_alive "$pid"
}

# Attempt to FORCE-STOP a per-worktree instance's postmaster by DATA DIR, then
# VERIFY. Uses `pg_ctl -m immediate` (SIGQUIT): the most reliable stop; the next
# start just crash-recovers — safe for a throwaway test cluster. Resolves pg_ctl
# INDEPENDENTLY of the full toolchain (resolve_pg_ctl), so the "psql missing"
# reconcile-failure case still stops the server instead of orphaning it (a
# bindir-gated stop would silently skip it). The stop is best-effort, but the
# RETURN VALUE is verified: 0 iff the postmaster is confirmed gone (or was never
# up), non-zero if it is still alive (no resolvable pg_ctl, or it ignored
# SIGQUIT). Callers that delete the data dir MUST gate on this: deleting under a
# live server is worse than the wedge and defeats manual `pg_ctl -D` recovery.
force_stop_instance() {
  local id="$1" datadir; datadir=$(data_dir "$id")
  [ -s "$datadir/PG_VERSION" ] || return 0     # never initialized -> nothing to stop
  local pgctl; pgctl=$(resolve_pg_ctl 2>/dev/null || true)
  if [ -n "$pgctl" ]; then
    "$pgctl" -D "$datadir" -m immediate stop >/dev/null 2>&1 || true
  fi
  postmaster_alive "$datadir" && return 1      # still up -> report failure
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
  # Force-stop and VERIFY the server is down before deleting its data dir.
  # _stop_locked is a best-effort `-m fast` (skipped entirely when the toolchain
  # doesn't resolve, e.g. psql missing). Deleting a live server's data dir is
  # worse than the wedge and defeats manual `pg_ctl -D <datadir>` recovery — so
  # only rm on a verified stop; otherwise keep the data dir and warn.
  if force_stop_instance "$id"; then
    rm -rf "$(instance_dir "$id")" "$(socket_dir "$id")" 2>/dev/null || true
  else
    warn "teardown: could not stop the postmaster for instance $id; keeping its data dir ($(data_dir "$id")) intact. Stop it manually ('$(resolve_pg_ctl 2>/dev/null || echo pg_ctl) -D <datadir> -m immediate stop' or kill the postmaster pid), then re-run 'make test-pg-teardown'."
  fi
}

# ===========================================================================
# Subcommand: reap  (prune instances whose worktree no longer exists)
# ===========================================================================
cmd_reap() {
  local home; home=$(pg_home)
  [ -d "$home" ] || return 0

  # Enumerate live worktrees. `git worktree list` must be run from inside a git
  # repo; if it fails (non-worktree cwd) we MUST abort rather than treat the
  # empty result as "all worktrees gone" — that would reap every instance.
  local porcelain
  if ! porcelain=$(git worktree list --porcelain 2>/dev/null); then
    warn "reap aborted: not inside a git repository (run from a checkout/worktree)"
    return 1
  fi

  # Compute the id of every live worktree. Use `git -C <wt>` with NO inherited
  # GIT_DIR — a normal reap has none, and an empty GIT_DIR='' is a FATAL error
  # to git ("not a git repository: ''"), so `env -u GIT_DIR` guarantees the
  # query resolves the target worktree's git-dir instead of silently yielding
  # nothing for every worktree.
  local live_ids="" wt gd
  while IFS= read -r line; do
    case "$line" in
      worktree\ *)
        wt="${line#worktree }"
        gd=$(env -u GIT_DIR git -C "$wt" rev-parse --absolute-git-dir 2>/dev/null || true)
        [ -n "$gd" ] && live_ids="$live_ids $(sha256_hex "$gd" | head -c 16)"
        ;;
    esac
  done <<EOF
$porcelain
EOF

  # Defensive guard: the current process is ALWAYS inside at least one worktree,
  # so a non-empty `git worktree list` that yields ZERO live ids means the
  # git-dir resolution itself failed (e.g. a future regression). Abort rather
  # than interpret "no live ids" as "everything is dead" — never delete all.
  if [ -z "${live_ids// /}" ]; then
    warn "reap aborted: computed an empty live-worktree set (refusing to treat all instances as dead)"
    return 1
  fi

  local bindir; bindir=$(resolve_bindir 2>/dev/null || true)
  local mf id
  shopt -s nullglob
  for mf in "$home"/*/meta; do
    id=$(basename "$(dirname "$mf")")
    case " $live_ids " in
      *" $id "*) continue ;;   # still live; keep
    esac
    # Dead worktree: stop (if running) + remove the data dir, serialized under
    # the per-worktree lock (same as ensure/stop/teardown). Operates ONLY under
    # $home — never :5432, never Docker. Guard with `|| true`: under set -e a
    # non-zero with_lock (e.g. a 124 lock timeout on one wedged instance) would
    # abort the loop and strand the reap of every remaining instance.
    with_lock "$(instance_dir "$id")/lock" _reap_one "$id" "$bindir" || true
  done
  shopt -u nullglob
}

_reap_one() {
  local id="$1" bindir="$2" datadir; datadir=$(data_dir "$id")
  if [ -n "$bindir" ] && [ -s "$datadir/PG_VERSION" ]; then
    "$bindir/pg_ctl" -D "$datadir" -m fast stop >/dev/null 2>&1 || true
  fi
  if force_stop_instance "$id"; then
    rm -rf "$(instance_dir "$id")" "$(socket_dir "$id")" 2>/dev/null || true
    echo "reaped per-worktree pg instance $id (worktree gone)"
  else
    warn "reap: could not stop the postmaster for instance $id; keeping its data dir ($datadir) intact (stop it manually, then re-run 'make test-pg-reap')."
  fi
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
    active)   cmd_active ;;
    ensure)   cmd_ensure ;;
    stop)     cmd_stop ;;
    teardown) cmd_teardown ;;
    reap)     cmd_reap ;;
    status)   cmd_status ;;
    port)     cmd_port ;;
    *)
      echo "usage: worktree-test-pg.sh {url|active|ensure|stop|teardown|reap|status|port}" >&2
      return 2
      ;;
  esac
}

main "$@"
