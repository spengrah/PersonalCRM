#!/usr/bin/env bash
#
# route-parity.sh — contract-7 route-table parity harness for the
# composition-root refactor arc (PR3/PR4).
#
# Boots the crm-api backend from two working trees under each of six
# config shapes, captures the gin [GIN-debug] route table + boot log from
# each, and asserts:
#   1. the normalized (METHOD PATH SYMBOL, sorted) route tables are
#      byte-identical between the two trees, per shape; and
#   2. per-shape presence/absence sentinels fired (proving the gate that
#      shape exercises actually ran, so parity is not a false-pass from
#      both trees identically skipping a branch).
#
# This is a dev/verification tool. It is NOT wired into CI or pre-push.
#
# Usage:
#   scripts/dev/route-parity.sh <tree-a> <tree-b> [shape ...]
#
#   <tree-a>, <tree-b>  repo working-tree roots (each containing backend/).
#                       Typically a develop checkout and this branch.
#   [shape ...]         one or more of 1..7; default: all seven.
#
# Env (all optional; sensible defaults for the shared local test DB):
#   DATABASE_URL   Postgres URL (default: shared :5432 personal_crm_test)
#   API_KEY        API key handed to the boot (default: route-parity-key)
#   BOOT_TIMEOUT   seconds to wait for the "starting server" line (default 120)
#   BASE_PORT      first TCP port to bind (default 18090; each boot +1)
#
# Exit status: 0 iff every requested shape passed (identical routes + all
# sentinels). Non-zero on any diff, missing/forbidden sentinel, or boot
# failure.

set -uo pipefail

# --- args ---------------------------------------------------------------
if [[ $# -lt 2 ]]; then
  echo "usage: $0 <tree-a> <tree-b> [shape ...]" >&2
  exit 2
fi
TREE_A="$(cd "$1" && pwd)"; shift
TREE_B="$(cd "$1" && pwd)"; shift
SHAPES=("$@")
if [[ ${#SHAPES[@]} -eq 0 ]]; then
  SHAPES=(1 2 3 4 5 6 7)
fi
# Validate every requested shape against the literal set 1..7 up front.
# shape_env's `return 1` on an unknown shape is swallowed by the
# `< <(shape_env ...)` process substitution in boot_tree, so an unknown
# shape would otherwise boot the BASE env and false-PASS. Reject here.
for shape in "${SHAPES[@]}"; do
  case "$shape" in
    1|2|3|4|5|6|7) ;;
    *) echo "error: unknown shape '$shape' (valid: 1..7)" >&2; exit 2 ;;
  esac
done

DATABASE_URL="${DATABASE_URL:-postgres://crm_user:crm_password@localhost:5432/personal_crm_test?sslmode=disable}"
API_KEY="${API_KEY:-route-parity-key}"
BOOT_TIMEOUT="${BOOT_TIMEOUT:-120}"
BASE_PORT="${BASE_PORT:-18090}"

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/route-parity.XXXXXX")"
trap 'rm -rf "$WORKDIR"' EXIT

# A valid 64-hex token-encryption key (dummy — never a real secret).
TOKEN_KEY="0000000000000000000000000000000000000000000000000000000000000000"

PORT_SEQ=0

# --- shape env deltas ---------------------------------------------------
# Emits `KEY=VALUE` lines (one per line) for the shape's delta env on top
# of the base env applied in boot_tree.
shape_env() {
  local shape="$1"
  case "$shape" in
    1) : ;;                                   # base only
    2) echo "CRM_ENV=production" ;;           # prod alias — test routes absent
    3)
      echo "ENABLE_EXTERNAL_SYNC=true"
      echo "GOOGLE_CLIENT_ID=dummy-google-id"
      echo "GOOGLE_CLIENT_SECRET=dummy-google-secret"
      echo "TODOIST_CLIENT_ID=dummy-todoist-id"
      echo "TODOIST_CLIENT_SECRET=dummy-todoist-secret"
      echo "TOKEN_ENCRYPTION_KEY=$TOKEN_KEY"
      ;;
    4)
      echo "ENABLE_TELEGRAM_SYNC=true"
      echo "TELEGRAM_API_ID=12345"            # NONZERO — getEnvAsInt falls back to 0 on non-numeric, which config rejects
      echo "TELEGRAM_API_HASH=dummy-telegram-hash"
      echo "TOKEN_ENCRYPTION_KEY=$TOKEN_KEY"
      ;;
    5) echo "EVENT_BUS_INGEST_ENABLED=true" ;;
    6)
      echo "ENABLE_EXTERNAL_SYNC=true"
      echo "GOOGLE_CLIENT_ID=dummy-google-id"
      echo "GOOGLE_CLIENT_SECRET=dummy-google-secret"
      echo "TODOIST_CLIENT_ID=dummy-todoist-id"
      echo "TODOIST_CLIENT_SECRET=dummy-todoist-secret"
      echo "TOKEN_ENCRYPTION_KEY=$TOKEN_KEY"
      echo "EVENT_BUS_INTERACTION_MODE=off"
      ;;
    7)
      # WhatsApp rides on top of the external-sync stack: ENABLE_WHATSAPP_SYNC
      # without ENABLE_EXTERNAL_SYNC fails config validation outright, so this
      # shape is shape 3 plus the WhatsApp flag rather than a flag on its own.
      echo "ENABLE_EXTERNAL_SYNC=true"
      echo "GOOGLE_CLIENT_ID=dummy-google-id"
      echo "GOOGLE_CLIENT_SECRET=dummy-google-secret"
      echo "TODOIST_CLIENT_ID=dummy-todoist-id"
      echo "TODOIST_CLIENT_SECRET=dummy-todoist-secret"
      echo "TOKEN_ENCRYPTION_KEY=$TOKEN_KEY"
      echo "ENABLE_WHATSAPP_SYNC=true"
      ;;
    *) echo "unknown shape: $shape" >&2; return 1 ;;
  esac
}

# --- boot one tree under one shape; write routes + log -----------------
# boot_tree <tree> <shape> <routes_out> <log_out>
# Returns 0 on a clean boot (reached "starting server"), non-zero otherwise.
boot_tree() {
  local tree="$1" shape="$2" routes_out="$3" log_out="$4"
  local port=$((BASE_PORT + PORT_SEQ))
  PORT_SEQ=$((PORT_SEQ + 1))

  # Base env (contract 7): NODE_ENV=development keeps gin in debug mode so
  # [GIN-debug] route lines print; CRM_ENV=testing is the shape-1 baseline
  # (shape 2 overrides it via shape_env).
  local -a env=(
    "DATABASE_URL=$DATABASE_URL"
    "MIGRATIONS_PATH=$tree/backend/migrations"
    "API_KEY=$API_KEY"
    "NODE_ENV=development"
    "CRM_ENV=testing"
    "PORT=$port"
    "HOST=127.0.0.1"
  )
  # Layer the shape delta (later assignments win in `env`).
  local line
  while IFS= read -r line; do
    [[ -n "$line" ]] && env+=("$line")
  done < <(shape_env "$shape")

  # Boot in its own session so we can kill the go-run parent AND its
  # compiled child by process group. Direct redirect (NOT nohup) so
  # zerolog lines reliably reach the file — the shape-3/6 log sentinels
  # depend on this.
  setsid env "${env[@]}" go run ./cmd/crm-api >"$log_out" 2>&1 &
  local pid=$!

  # Poll for the ready line or a fatal, up to BOOT_TIMEOUT.
  local waited=0 ok=1
  while (( waited < BOOT_TIMEOUT )); do
    if grep -q "starting server" "$log_out" 2>/dev/null; then ok=0; break; fi
    if grep -qiE '"level":"fatal"|failed to bind listener|panic:' "$log_out" 2>/dev/null; then ok=1; break; fi
    if ! kill -0 "$pid" 2>/dev/null; then ok=1; break; fi
    sleep 1; waited=$((waited + 1))
  done

  # Give gin a beat to have flushed all route lines (they print during
  # registration, before "starting server", so they are already present).
  kill -TERM -"$pid" 2>/dev/null
  wait "$pid" 2>/dev/null

  # Extract + normalize the route table: METHOD PATH SYMBOL, sorted.
  grep -F -- '-->' "$log_out" 2>/dev/null \
    | grep -F '[GIN-debug]' \
    | sed -E 's/^\[GIN-debug\][[:space:]]+([A-Z]+)[[:space:]]+([^[:space:]]+)[[:space:]]+-->[[:space:]]+([^[:space:]]+).*/\1 \2 \3/' \
    | sort >"$routes_out"

  return $ok
}

# --- sentinel checks per shape -----------------------------------------
# assert_present <label> <file> <needle>   — fail if needle absent
# assert_absent  <label> <file> <needle>   — fail if needle present
SENTINEL_FAIL=0
assert_present() {
  if ! grep -qF -- "$3" "$2"; then
    echo "    SENTINEL FAIL: expected present but MISSING: $1 ($3)"; SENTINEL_FAIL=1
  else
    echo "    sentinel ok (present): $1"
  fi
}
assert_absent() {
  if grep -qF -- "$3" "$2"; then
    echo "    SENTINEL FAIL: expected absent but PRESENT: $1 ($3)"; SENTINEL_FAIL=1
  else
    echo "    sentinel ok (absent):  $1"
  fi
}

# check_sentinels <shape> <routes_file> <log_file>
check_sentinels() {
  local shape="$1" routes="$2" log="$3"
  SENTINEL_FAIL=0
  case "$shape" in
    1) assert_present "test-only routes"        "$routes" "/api/v1/test/" ;;
    2) assert_absent  "test-only routes"        "$routes" "/api/v1/test/" ;;
    3)
      assert_present "google OAuth callback"    "$routes" "/api/v1/auth/google/callback"
      assert_present "google auth-url route"    "$routes" "/api/v1/auth/google"
      assert_present "todoist settings routes"  "$routes" "/api/v1/todoist/settings"
      assert_present "gmail-registered log"     "$log"    "Gmail sync provider + rematch handler + correspondence discovery registered"
      ;;
    4) assert_present "telegram routes"         "$routes" "/api/v1/telegram/" ;;
    5) assert_present "ingest events route"     "$routes" "/api/v1/ingest/events" ;;
    6)
      assert_present "gmail-off-mode warn log"  "$log"    "Gmail provider NOT registered: event-bus interaction mode=off (pubBus nil)"
      assert_absent  "gmail-registered log"     "$log"    "Gmail sync provider + rematch handler + correspondence discovery registered"
      ;;
    7)
      # The shape boots today and registers no WhatsApp routes yet; the
      # external-sync sentinels are what prove the shape is really the
      # WhatsApp-enabled variant of shape 3 rather than a silent base boot.
      assert_present "google OAuth callback"    "$routes" "/api/v1/auth/google/callback"
      assert_present "todoist settings routes"  "$routes" "/api/v1/todoist/settings"
      ;;
  esac
  return $SENTINEL_FAIL
}

# --- preflight: DB reachable -------------------------------------------
echo "route-parity: tree-a=$TREE_A"
echo "route-parity: tree-b=$TREE_B"
echo "route-parity: shapes=${SHAPES[*]}"
echo "route-parity: DATABASE_URL=$DATABASE_URL"
echo

# --- main loop ----------------------------------------------------------
OVERALL=0
for shape in "${SHAPES[@]}"; do
  echo "=== shape $shape ==="
  ra="$WORKDIR/shape${shape}.a.routes"; la="$WORKDIR/shape${shape}.a.log"
  rb="$WORKDIR/shape${shape}.b.routes"; lb="$WORKDIR/shape${shape}.b.log"

  boot_a_ok=0; boot_b_ok=0
  cd "$TREE_A/backend"
  boot_tree "$TREE_A" "$shape" "$ra" "$la"; boot_a_ok=$?
  cd "$TREE_B/backend"
  boot_tree "$TREE_B" "$shape" "$rb" "$lb"; boot_b_ok=$?

  if [[ $boot_a_ok -ne 0 || $boot_b_ok -ne 0 ]]; then
    echo "  BOOT FAIL (a=$boot_a_ok b=$boot_b_ok) — see $la / $lb"
    [[ $boot_a_ok -ne 0 ]] && tail -3 "$la" | sed 's/^/    a| /'
    [[ $boot_b_ok -ne 0 ]] && tail -3 "$lb" | sed 's/^/    b| /'
    OVERALL=1
    echo
    continue
  fi

  na=$(wc -l <"$ra"); nb=$(wc -l <"$rb")
  echo "  routes: tree-a=$na  tree-b=$nb"

  # Minimum-route-count floor. Shapes 2/6 assert only absence/log
  # sentinels, so a doubly-empty capture (both trees emitting 0
  # [GIN-debug] lines — e.g. a botched capture) would pass parity + those
  # sentinels trivially. The SMALLEST legitimate shape (shape 2, ~35
  # routes) sits well above this floor; a broken/empty capture (~0) sits
  # well below it.
  MIN_ROUTES=25
  if (( na < MIN_ROUTES || nb < MIN_ROUTES )); then
    echo "  ROUTE COUNT FLOOR FAIL: tree-a=$na tree-b=$nb (min $MIN_ROUTES) — capture likely broken/empty"
    OVERALL=1
    echo
    continue
  fi

  if diff -u "$ra" "$rb" >"$WORKDIR/shape${shape}.diff"; then
    echo "  ROUTE PARITY: identical"
  else
    echo "  ROUTE PARITY: DIFF (tree-a=< tree-b=>)"
    sed 's/^/    /' "$WORKDIR/shape${shape}.diff"
    OVERALL=1
  fi

  # Sentinels on the branch tree's captures (parity already proved the two
  # dumps identical; the log sentinels for shapes 3/6 are checked on b).
  if check_sentinels "$shape" "$rb" "$lb"; then
    :
  else
    OVERALL=1
  fi
  echo
done

if [[ $OVERALL -eq 0 ]]; then
  echo "route-parity: ALL SHAPES PASS"
else
  echo "route-parity: FAILURES DETECTED"
fi
exit $OVERALL
